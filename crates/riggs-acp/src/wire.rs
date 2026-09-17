use rax::session::PromptCapabilities;
use rax::tool::{PlanEntry, PlanEntryPriority, PlanEntryStatus};
use serde::Deserialize;
use serde_json::Value;

use crate::error::AcpError;

pub(crate) const SUPPORTED_PROTOCOL: u64 = 1;

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct AgentInfo {
    pub(crate) load_session: bool,
    pub(crate) prompt: PromptCapabilities,
    pub(crate) auth_methods: Vec<String>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct InitializeResponse {
    protocol_version: Value,
    #[serde(default)]
    agent_capabilities: AgentCapabilities,
    #[serde(default)]
    auth_methods: Vec<AuthMethod>,
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct AgentCapabilities {
    #[serde(default)]
    load_session: bool,
    #[serde(default)]
    prompt_capabilities: AgentPromptCapabilities,
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct AgentPromptCapabilities {
    #[serde(default)]
    image: bool,
    #[serde(default)]
    audio: bool,
    #[serde(default)]
    embedded_context: bool,
}

#[derive(Deserialize)]
struct AuthMethod {
    id: String,
    #[serde(default)]
    name: Option<String>,
}

pub(crate) fn initialized(value: Value) -> Result<AgentInfo, AcpError> {
    let response: InitializeResponse = decode("initialize", value)?;
    if response.protocol_version.as_u64() != Some(SUPPORTED_PROTOCOL) {
        return Err(AcpError::ProtocolVersion(
            response.protocol_version.to_string(),
        ));
    }
    let caps = response.agent_capabilities;
    Ok(AgentInfo {
        load_session: caps.load_session,
        prompt: PromptCapabilities {
            image: caps.prompt_capabilities.image,
            audio: caps.prompt_capabilities.audio,
            embedded_resource: caps.prompt_capabilities.embedded_context,
        },
        auth_methods: response
            .auth_methods
            .into_iter()
            .map(|method| match method.name {
                Some(name) => format!("{name} ({})", method.id),
                None => method.id,
            })
            .collect(),
    })
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct SessionCreated {
    session_id: String,
}

pub(crate) fn session_created(value: Value) -> Result<String, AcpError> {
    let created: SessionCreated = decode("session/new", value)?;
    Ok(created.session_id)
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct PromptResponse {
    stop_reason: String,
}

pub(crate) fn stop_reason(value: Value) -> Result<String, AcpError> {
    let response: PromptResponse = decode("session/prompt", value)?;
    Ok(response.stop_reason)
}

fn decode<T: serde::de::DeserializeOwned>(method: &str, value: Value) -> Result<T, AcpError> {
    serde_json::from_value(value).map_err(|err| AcpError::Unreadable {
        method: method.to_owned(),
        detail: err.to_string(),
    })
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct SessionNotification {
    pub(crate) session_id: String,
    pub(crate) update: Value,
}

/// Every field but the id is optional, because ACP sends the same shape for announcements and
/// partial updates.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ToolFields {
    pub(crate) tool_call_id: String,
    #[serde(default)]
    pub(crate) name: Option<String>,
    #[serde(default)]
    pub(crate) title: Option<String>,
    #[serde(default)]
    pub(crate) kind: Option<String>,
    #[serde(default)]
    pub(crate) status: Option<String>,
    #[serde(default)]
    pub(crate) content: Option<Vec<Value>>,
    #[serde(default)]
    pub(crate) raw_input: Option<Value>,
    #[serde(default)]
    pub(crate) raw_output: Option<Value>,
    #[serde(default, rename = "_meta")]
    pub(crate) meta: Option<Value>,
}

impl ToolFields {
    pub(crate) fn merge(&mut self, newer: &ToolFields) {
        fn replace<T: Clone>(mine: &mut Option<T>, theirs: &Option<T>) {
            if theirs.is_some() {
                mine.clone_from(theirs);
            }
        }
        replace(&mut self.name, &newer.name);
        replace(&mut self.title, &newer.title);
        replace(&mut self.kind, &newer.kind);
        replace(&mut self.status, &newer.status);
        replace(&mut self.content, &newer.content);
        replace(&mut self.raw_input, &newer.raw_input);
        replace(&mut self.raw_output, &newer.raw_output);
        replace(&mut self.meta, &newer.meta);
    }
}

#[derive(Debug, PartialEq)]
pub(crate) enum Update {
    Content(Vec<Value>),
    Tool(Box<ToolFields>),
    Plan(Vec<PlanEntry>),
    Silent(String),
    Unknown(String),
}

pub(crate) fn update(update: Value) -> Result<Update, String> {
    let kind = update
        .get("sessionUpdate")
        .and_then(Value::as_str)
        .ok_or_else(|| "a session/update with no sessionUpdate kind".to_owned())?
        .to_owned();
    Ok(match kind.as_str() {
        "agent_message_chunk" | "agent_message" => match update.get("content") {
            Some(Value::Array(blocks)) => Update::Content(blocks.clone()),
            Some(block @ Value::Object(_)) => Update::Content(vec![block.clone()]),
            _ => return Err(format!("{kind} with no content")),
        },
        "tool_call" | "tool_call_update" => Update::Tool(Box::new(
            serde_json::from_value(update).map_err(|err| format!("{kind}: {err}"))?,
        )),
        "plan" => Update::Plan(plan(&update)),
        "agent_thought_chunk"
        | "user_message_chunk"
        | "available_commands_update"
        | "current_mode_update"
        | "config_option_update"
        | "session_info_update"
        | "usage_update" => Update::Silent(kind),
        _ => Update::Unknown(kind),
    })
}

#[derive(Deserialize)]
struct WirePlanEntry {
    content: String,
    priority: PlanEntryPriority,
    status: PlanEntryStatus,
}

fn plan(update: &Value) -> Vec<PlanEntry> {
    update
        .get("entries")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|entry| serde_json::from_value::<WirePlanEntry>(entry.clone()).ok())
        .map(|entry| PlanEntry {
            content: entry.content,
            priority: entry.priority,
            status: entry.status,
        })
        .collect()
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum OptionKind {
    AllowOnce,
    AllowAlways,
    RejectOnce,
    RejectAlways,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct PermissionOption {
    pub(crate) id: String,
    pub(crate) kind: OptionKind,
}

#[derive(Debug)]
pub(crate) struct PermissionRequest {
    pub(crate) session_id: String,
    pub(crate) tool: ToolFields,
    pub(crate) options: Vec<PermissionOption>,
    pub(crate) skipped_options: usize,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct WirePermissionRequest {
    session_id: String,
    tool_call: ToolFields,
    #[serde(default)]
    options: Vec<Value>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct WireOption {
    option_id: String,
    kind: String,
}

pub(crate) fn permission_request(params: Value) -> Result<PermissionRequest, String> {
    let request: WirePermissionRequest =
        serde_json::from_value(params).map_err(|err| err.to_string())?;
    let total = request.options.len();
    let options: Vec<PermissionOption> = request
        .options
        .into_iter()
        .filter_map(|option| serde_json::from_value::<WireOption>(option).ok())
        .filter_map(|option| {
            let kind = match option.kind.as_str() {
                "allow_once" => OptionKind::AllowOnce,
                "allow_always" => OptionKind::AllowAlways,
                "reject_once" => OptionKind::RejectOnce,
                "reject_always" => OptionKind::RejectAlways,
                _ => return None,
            };
            Some(PermissionOption {
                id: option.option_id,
                kind,
            })
        })
        .collect();
    Ok(PermissionRequest {
        session_id: request.session_id,
        tool: request.tool_call,
        skipped_options: total - options.len(),
        options,
    })
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use serde_json::json;

    use super::*;

    #[test]
    fn capabilities_come_from_initialize() {
        let info = initialized(json!({
            "protocolVersion": 1,
            "agentCapabilities": {"loadSession": true, "promptCapabilities": {"image": true, "embeddedContext": true}},
            "authMethods": [{"id": "login", "name": "Log in"}, {"id": "key"}]
        }))
        .unwrap();
        assert!(info.load_session);
        assert_eq!(
            info.prompt,
            PromptCapabilities {
                image: true,
                audio: false,
                embedded_resource: true
            }
        );
        assert_eq!(info.auth_methods, ["Log in (login)", "key"]);
    }

    #[test]
    fn a_bare_initialize_means_no_load_and_text_only() {
        let info = initialized(json!({"protocolVersion": 1})).unwrap();
        assert!(!info.load_session);
        assert_eq!(info.prompt, PromptCapabilities::default());
    }

    #[test]
    fn another_protocol_version_is_unsupported() {
        let err = initialized(json!({"protocolVersion": 2})).unwrap_err();
        assert!(matches!(err, AcpError::ProtocolVersion(version) if version == "2"));
    }

    #[test]
    fn message_chunks_accept_the_legacy_kind_and_array_content() {
        let chunk = update(json!({"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "hi"}})).unwrap();
        assert_eq!(
            chunk,
            Update::Content(vec![json!({"type": "text", "text": "hi"})])
        );
        let legacy = update(json!({"sessionUpdate": "agent_message", "content": [{"type": "text", "text": "a"}, {"type": "text", "text": "b"}]})).unwrap();
        assert!(matches!(legacy, Update::Content(blocks) if blocks.len() == 2));
    }

    #[test]
    fn unknown_and_silent_kinds_are_told_apart() {
        assert_eq!(
            update(json!({"sessionUpdate": "brand_new_kind"})).unwrap(),
            Update::Unknown("brand_new_kind".into())
        );
        assert_eq!(
            update(json!({"sessionUpdate": "usage_update", "used": 1})).unwrap(),
            Update::Silent("usage_update".into())
        );
        assert!(update(json!({"content": "x"})).is_err());
    }

    #[test]
    fn plan_entries_that_cannot_be_read_are_skipped() {
        let plan = update(json!({"sessionUpdate": "plan", "entries": [
            {"content": "read", "priority": "high", "status": "completed"},
            {"content": "odd", "priority": "urgent", "status": "pending"},
        ]}))
        .unwrap();
        assert_eq!(
            plan,
            Update::Plan(vec![PlanEntry {
                content: "read".into(),
                priority: PlanEntryPriority::High,
                status: PlanEntryStatus::Completed
            }])
        );
    }

    #[test]
    fn unknown_option_kinds_are_skipped_not_fatal() {
        let request = permission_request(json!({
            "sessionId": "s1",
            "toolCall": {"toolCallId": "tc1", "title": "rm"},
            "options": [
                {"optionId": "a", "name": "Allow", "kind": "allow_once"},
                {"optionId": "m", "name": "Maybe", "kind": "allow_sometimes"},
                {"optionId": "r", "name": "Reject", "kind": "reject_once"},
            ]
        }))
        .unwrap();
        assert_eq!(request.skipped_options, 1);
        assert_eq!(
            request.options,
            [
                PermissionOption {
                    id: "a".into(),
                    kind: OptionKind::AllowOnce
                },
                PermissionOption {
                    id: "r".into(),
                    kind: OptionKind::RejectOnce
                },
            ]
        );
    }

    #[test]
    fn a_later_update_only_overrides_the_fields_it_carries() {
        let mut known = ToolFields {
            tool_call_id: "tc1".into(),
            title: Some("ls".into()),
            kind: Some("execute".into()),
            ..Default::default()
        };
        known.merge(&ToolFields {
            tool_call_id: "tc1".into(),
            status: Some("in_progress".into()),
            ..Default::default()
        });
        assert_eq!(known.title.as_deref(), Some("ls"));
        assert_eq!(known.status.as_deref(), Some("in_progress"));
    }
}
