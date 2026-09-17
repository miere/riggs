use base64::Engine;
use base64::engine::general_purpose::STANDARD;
use rax::content::{ContentBlock, EmbeddedResource};
use rax::event::StopReason;
use rax::open::{Subject, UnhandledReason};
use rax::session::PromptCapabilities;
use rax::tool::{ToolCallStatus, ToolCallUpdate, ToolKind};
use rax::{Open, ToolCall, Unhandled};
use riggs_node::AttachmentMeta;
use serde_json::Value;

use crate::wire::ToolFields;

pub(crate) const MAX_ATTACHMENT_BYTES: usize = 100 * 1024 * 1024;

pub(crate) fn stop_reason(reason: &str) -> StopReason {
    match reason {
        "canceled" => StopReason::Cancelled,
        other => {
            serde_json::from_value(Value::String(other.to_owned())).unwrap_or(StopReason::Other)
        }
    }
}

pub(crate) fn tool_status(status: Option<&str>) -> Option<ToolCallStatus> {
    match status? {
        "in_progress" => Some(ToolCallStatus::InProgress),
        "completed" => Some(ToolCallStatus::Completed),
        "failed" => Some(ToolCallStatus::Failed),
        _ => None,
    }
}

pub(crate) fn tool_kind(kind: Option<&str>) -> ToolKind {
    kind.and_then(|kind| serde_json::from_value(Value::String(kind.to_owned())).ok())
        .unwrap_or_default()
}

/// Policies match on `name`, so the rule is frozen: the agent's own tool name, then Claude Code's
/// name in `_meta`, then the ACP kind.
pub(crate) fn tool_name(fields: &ToolFields) -> String {
    let meta_name = fields
        .meta
        .as_ref()
        .and_then(|meta| meta.pointer("/claudeCode/toolName"))
        .and_then(Value::as_str);
    [fields.name.as_deref(), meta_name]
        .into_iter()
        .flatten()
        .find(|name| !name.is_empty())
        .map(str::to_owned)
        .unwrap_or_else(|| kind_name(tool_kind(fields.kind.as_deref())))
}

fn kind_name(kind: ToolKind) -> String {
    serde_json::to_value(kind)
        .ok()
        .and_then(|value| value.as_str().map(str::to_owned))
        .unwrap_or_else(|| "other".to_owned())
}

pub(crate) fn tool_call(fields: &ToolFields) -> ToolCall {
    ToolCall {
        id: fields.tool_call_id.clone().into(),
        name: tool_name(fields),
        title: fields.title.clone(),
        kind: tool_kind(fields.kind.as_deref()),
        input: fields.raw_input.clone(),
        content: tool_content(fields.content.as_deref().unwrap_or_default()),
    }
}

pub(crate) fn tool_update(fields: &ToolFields, status: ToolCallStatus) -> ToolCallUpdate {
    ToolCallUpdate {
        id: fields.tool_call_id.clone().into(),
        status,
        title: fields.title.clone(),
        content: tool_content(fields.content.as_deref().unwrap_or_default()),
        output: fields.raw_output.clone(),
    }
}

pub(crate) fn denied(id: &str, title: Option<String>) -> ToolCallUpdate {
    ToolCallUpdate {
        id: id.to_owned().into(),
        status: ToolCallStatus::Denied,
        title,
        content: Vec::new(),
        output: None,
    }
}

fn tool_content(content: &[Value]) -> Vec<Open<ContentBlock>> {
    content
        .iter()
        .filter_map(|item| match item.get("type").and_then(Value::as_str) {
            Some("content") => item
                .get("content")
                .and_then(|block| serde_json::from_value(block.clone()).ok()),
            Some("diff") => Some(ContentBlock::text(diff(item)).into()),
            _ => None,
        })
        .collect()
}

fn diff(item: &Value) -> String {
    let field = |name: &str| item.get(name).and_then(Value::as_str).unwrap_or_default();
    let path = field("path");
    let mut text = format!("--- {path}\n+++ {path}\n");
    for line in field("oldText").lines() {
        text.push_str(&format!("-{line}\n"));
    }
    for line in field("newText").lines() {
        text.push_str(&format!("+{line}\n"));
    }
    text
}

/// The indexes refer to `blocks`, because the gateway reports unhandled blocks by position.
pub(crate) fn prompt_blocks(
    blocks: &[Open<ContentBlock>],
    caps: &PromptCapabilities,
) -> (Vec<Value>, Vec<Unhandled>) {
    let mut sent = Vec::new();
    let mut unhandled = Vec::new();
    for (index, block) in blocks.iter().enumerate() {
        let accepted = match block {
            Open::Known(ContentBlock::Text { .. } | ContentBlock::ResourceLink { .. }) => true,
            Open::Known(ContentBlock::Image { .. }) => caps.image,
            Open::Known(ContentBlock::Audio { .. }) => caps.audio,
            Open::Known(ContentBlock::Resource { .. }) => caps.embedded_resource,
            Open::Unknown { .. } => false,
        };
        match serde_json::to_value(block) {
            Ok(value) if accepted => sent.push(value),
            _ => unhandled.push(Unhandled {
                subject: Subject::Block { index },
                reason: UnhandledReason::UnsupportedType,
                message: None,
            }),
        }
    }
    (sent, unhandled)
}

#[derive(Debug, PartialEq)]
pub(crate) enum Reply {
    Message(Open<ContentBlock>),
    Attachment {
        meta: AttachmentMeta,
        bytes: Vec<u8>,
    },
    TooLarge {
        name: String,
    },
}

/// Binary content goes out as a transfer, never inline, because a message frame is capped.
pub(crate) fn reply(block: Value) -> Option<Reply> {
    let block = match serde_json::from_value::<Open<ContentBlock>>(block) {
        Ok(block) => block,
        Err(err) => {
            tracing::warn!(error = %err, "dropping a content block that cannot be read");
            return None;
        }
    };
    let (kind, data, mimetype, uri) = match &block {
        Open::Known(ContentBlock::Image {
            data,
            mime_type,
            uri,
        }) => ("image", data, Some(mime_type), uri.as_ref()),
        Open::Known(ContentBlock::Audio { data, mime_type }) => {
            ("audio", data, Some(mime_type), None)
        }
        Open::Known(ContentBlock::Resource {
            resource:
                EmbeddedResource::Blob {
                    uri,
                    blob,
                    mime_type,
                },
        }) => ("resource", blob, mime_type.as_ref(), Some(uri)),
        _ => return Some(Reply::Message(block)),
    };
    let bytes = match STANDARD.decode(data.trim()) {
        Ok(bytes) if !bytes.is_empty() => bytes,
        Ok(_) => return None,
        Err(err) => {
            tracing::warn!(error = %err, "dropping binary content that is not valid base64");
            return None;
        }
    };
    let filename = filename(kind, uri.map(String::as_str), mimetype.map(String::as_str));
    if bytes.len() > MAX_ATTACHMENT_BYTES {
        return Some(Reply::TooLarge { name: filename });
    }
    Some(Reply::Attachment {
        meta: AttachmentMeta {
            filename: Some(filename),
            title: None,
            comment: None,
            mimetype: mimetype.cloned(),
        },
        bytes,
    })
}

fn filename(kind: &str, uri: Option<&str>, mimetype: Option<&str>) -> String {
    let base = uri
        .map(|uri| uri.split(['?', '#']).next().unwrap_or(uri))
        .and_then(|path| path.rsplit('/').next())
        .filter(|name| !name.is_empty());
    if let Some(name) = base {
        return name.to_owned();
    }
    let extension = match mimetype.unwrap_or_default() {
        "image/png" => ".png",
        "image/jpeg" => ".jpg",
        "image/gif" => ".gif",
        "image/webp" => ".webp",
        "application/pdf" => ".pdf",
        "audio/wav" | "audio/x-wav" => ".wav",
        "audio/mpeg" => ".mp3",
        "text/plain" => ".txt",
        "application/json" => ".json",
        _ => "",
    };
    format!("{kind}{extension}")
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use serde_json::json;

    use super::*;

    #[test]
    fn stop_reasons_map_one_to_one_and_unknown_is_other() {
        assert_eq!(stop_reason("end_turn"), StopReason::EndTurn);
        assert_eq!(stop_reason("max_tokens"), StopReason::MaxTokens);
        assert_eq!(
            stop_reason("max_turn_requests"),
            StopReason::MaxTurnRequests
        );
        assert_eq!(stop_reason("refusal"), StopReason::Refusal);
        assert_eq!(stop_reason("cancelled"), StopReason::Cancelled);
        assert_eq!(stop_reason("canceled"), StopReason::Cancelled);
        assert_eq!(stop_reason("tired"), StopReason::Other);
    }

    #[test]
    fn tool_status_skips_pending() {
        assert_eq!(tool_status(Some("pending")), None);
        assert_eq!(tool_status(None), None);
        assert_eq!(
            tool_status(Some("in_progress")),
            Some(ToolCallStatus::InProgress)
        );
        assert_eq!(tool_status(Some("failed")), Some(ToolCallStatus::Failed));
    }

    #[test]
    fn tool_name_prefers_the_agent_name_then_meta_then_kind() {
        let mut fields = ToolFields {
            tool_call_id: "tc1".into(),
            kind: Some("execute".into()),
            ..Default::default()
        };
        assert_eq!(tool_name(&fields), "execute");
        fields.meta = Some(json!({"claudeCode": {"toolName": "Bash"}}));
        assert_eq!(tool_name(&fields), "Bash");
        fields.name = Some("shell".into());
        assert_eq!(tool_name(&fields), "shell");
        fields.kind = Some("brand_new".into());
        fields.name = None;
        fields.meta = None;
        assert_eq!(tool_name(&fields), "other");
    }

    #[test]
    fn a_tool_call_carries_input_content_and_rendered_diffs() {
        let fields = ToolFields {
            tool_call_id: "tc1".into(),
            title: Some("Edit a.txt".into()),
            kind: Some("edit".into()),
            raw_input: Some(json!({"path": "a.txt"})),
            content: Some(vec![
                json!({"type": "content", "content": {"type": "text", "text": "note"}}),
                json!({"type": "diff", "path": "a.txt", "oldText": "old", "newText": "new"}),
                json!({"type": "terminal", "terminalId": "t1"}),
            ]),
            ..Default::default()
        };
        let call = tool_call(&fields);
        assert_eq!(call.kind, ToolKind::Edit);
        assert_eq!(call.input, Some(json!({"path": "a.txt"})));
        assert_eq!(
            call.content,
            vec![
                Open::Known(ContentBlock::text("note")),
                Open::Known(ContentBlock::text("--- a.txt\n+++ a.txt\n-old\n+new\n")),
            ]
        );
    }

    #[test]
    fn prompt_blocks_the_agent_cannot_take_are_unhandled_by_index() {
        let blocks: Vec<Open<ContentBlock>> = vec![
            ContentBlock::text("hi").into(),
            ContentBlock::Image {
                data: "AA==".into(),
                mime_type: "image/png".into(),
                uri: None,
            }
            .into(),
            ContentBlock::link("chat://thread/1", "thread").into(),
            Open::Unknown {
                name: "video".into(),
                raw: json!({"type": "video"}),
            },
        ];
        let (sent, unhandled) = prompt_blocks(&blocks, &PromptCapabilities::default());
        assert_eq!(
            sent,
            vec![
                json!({"type": "text", "text": "hi"}),
                json!({"type": "resource_link", "uri": "chat://thread/1", "name": "thread"}),
            ]
        );
        let indexes: Vec<_> = unhandled
            .iter()
            .map(|unhandled| unhandled.subject.clone())
            .collect();
        assert_eq!(
            indexes,
            [Subject::Block { index: 1 }, Subject::Block { index: 3 }]
        );
        let caps = PromptCapabilities {
            image: true,
            ..Default::default()
        };
        assert_eq!(prompt_blocks(&blocks, &caps).1.len(), 1);
    }

    #[test]
    fn binary_replies_become_named_attachments() {
        let image = reply(json!({"type": "image", "data": "iVBORw==", "mimeType": "image/png"}));
        let Some(Reply::Attachment { meta, bytes }) = image else {
            panic!("expected an attachment, got {image:?}");
        };
        assert_eq!(meta.filename.as_deref(), Some("image.png"));
        assert_eq!(bytes, STANDARD.decode("iVBORw==").unwrap());

        let blob = reply(
            json!({"type": "resource", "resource": {"uri": "file:///tmp/report.pdf?x=1", "blob": "JVBERg==", "mimeType": "application/pdf"}}),
        );
        let Some(Reply::Attachment { meta, .. }) = blob else {
            panic!("expected an attachment, got {blob:?}");
        };
        assert_eq!(meta.filename.as_deref(), Some("report.pdf"));
        assert_eq!(meta.mimetype.as_deref(), Some("application/pdf"));
    }

    #[test]
    fn text_replies_and_links_stay_messages_and_bad_binary_is_dropped() {
        assert_eq!(
            reply(json!({"type": "text", "text": "hi"})),
            Some(Reply::Message(ContentBlock::text("hi").into()))
        );
        assert!(matches!(
            reply(json!({"type": "resource_link", "uri": "https://x", "name": "x"})),
            Some(Reply::Message(_))
        ));
        assert_eq!(
            reply(json!({"type": "image", "data": "not base64!", "mimeType": "image/png"})),
            None
        );
        assert_eq!(
            reply(json!({"type": "image", "data": "", "mimeType": "image/png"})),
            None
        );
    }
}
