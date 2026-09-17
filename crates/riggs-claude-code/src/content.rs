use rax::content::ContentBlock;
use rax::open::{Subject, UnhandledReason};
use rax::{Open, Unhandled};
use serde_json::{Value, json};

pub(crate) struct Converted {
    pub(crate) blocks: Vec<Value>,
    pub(crate) unhandled: Vec<Unhandled>,
}

pub(crate) fn prompt(content: Vec<Open<ContentBlock>>) -> Converted {
    let mut converted = Converted {
        blocks: Vec::new(),
        unhandled: Vec::new(),
    };
    for (index, block) in content.into_iter().enumerate() {
        match block {
            Open::Known(ContentBlock::Text { text }) => {
                if !text.is_empty() {
                    converted.blocks.push(text_block(&text));
                }
            }
            Open::Known(ContentBlock::Image {
                data, mime_type, ..
            }) => converted.blocks.push(json!({
                "type": "image",
                "source": {"type": "base64", "media_type": mime_type, "data": data},
            })),
            Open::Known(ContentBlock::ResourceLink { uri, name, .. }) => {
                converted.blocks.push(text_block(&link(&name, &uri)));
            }
            Open::Known(ContentBlock::Audio { .. } | ContentBlock::Resource { .. })
            | Open::Unknown { .. } => converted.unhandled.push(Unhandled {
                subject: Subject::Block { index },
                reason: UnhandledReason::UnsupportedType,
                message: Some("Claude Code cannot read this kind of content".to_owned()),
            }),
        }
    }
    converted
}

pub(crate) fn context(context: &[Open<ContentBlock>]) -> (Option<String>, Vec<Unhandled>) {
    let mut lines = Vec::new();
    let mut unhandled = Vec::new();
    for (index, block) in context.iter().enumerate() {
        match block {
            Open::Known(ContentBlock::Text { text }) => lines.push(text.clone()),
            Open::Known(ContentBlock::ResourceLink { uri, name, .. }) => {
                lines.push(link(name, uri));
            }
            _ => unhandled.push(Unhandled {
                subject: Subject::Block { index },
                reason: UnhandledReason::UnsupportedType,
                message: Some("Claude Code cannot read this kind of context".to_owned()),
            }),
        }
    }
    let text = lines.join("\n");
    ((!text.trim().is_empty()).then_some(text), unhandled)
}

pub(crate) fn text_block(text: &str) -> Value {
    json!({"type": "text", "text": text})
}

fn link(name: &str, uri: &str) -> String {
    format!("[{name}: {uri}]")
}
