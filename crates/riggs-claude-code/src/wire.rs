use serde::Deserialize;
use serde_json::{Value, json};

pub(crate) const GATE_CALLBACK: &str = "gate";
pub(crate) const MCP_SERVER: &str = "riggs";
pub(crate) const RESUME_MISS: &str = "No conversation found with session ID";

pub(crate) fn line(frame: &Value) -> Vec<u8> {
    let mut bytes = frame.to_string().into_bytes();
    bytes.push(b'\n');
    bytes
}

pub(crate) fn id_of(value: &Value) -> Option<String> {
    match value {
        Value::String(id) => Some(id.clone()),
        Value::Number(id) => Some(id.to_string()),
        _ => None,
    }
}

pub(crate) fn text_of<'a>(value: &'a Value, key: &str) -> Option<&'a str> {
    value.get(key).and_then(Value::as_str)
}

pub(crate) fn control_request(request_id: &str, request: Value) -> Value {
    json!({"type": "control_request", "request_id": request_id, "request": request})
}

pub(crate) fn initialize(hook_timeout_secs: u64) -> Value {
    json!({
        "subtype": "initialize",
        "hooks": {"PreToolUse": [{"hookCallbackIds": [GATE_CALLBACK], "timeout": hook_timeout_secs}]},
        "sdkMcpServers": [MCP_SERVER],
    })
}

pub(crate) fn success(request_id: &str, response: Value) -> Value {
    json!({
        "type": "control_response",
        "response": {"subtype": "success", "request_id": request_id, "response": response},
    })
}

pub(crate) fn failure(request_id: &str, error: &str) -> Value {
    json!({
        "type": "control_response",
        "response": {"subtype": "error", "request_id": request_id, "error": error},
    })
}

pub(crate) fn hook_answer(request_id: &str, deny: Option<&str>) -> Value {
    let output = match deny {
        None => json!({"hookEventName": "PreToolUse", "permissionDecision": "allow"}),
        Some(reason) => json!({
            "hookEventName": "PreToolUse",
            "permissionDecision": "deny",
            "permissionDecisionReason": reason,
        }),
    };
    success(request_id, json!({"hookSpecificOutput": output}))
}

pub(crate) fn permission_answer(request_id: &str, input: &Value, deny: Option<&str>) -> Value {
    let response = match deny {
        None => json!({"behavior": "allow", "updatedInput": input}),
        Some(reason) => json!({"behavior": "deny", "message": reason}),
    };
    success(request_id, response)
}

pub(crate) fn mcp_answer(request_id: &str, message: Value) -> Value {
    success(request_id, json!({"mcp_response": message}))
}

pub(crate) fn user(content: Vec<Value>) -> Value {
    json!({"type": "user", "message": {"role": "user", "content": content}})
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub(crate) struct ResultFrame {
    pub(crate) subtype: String,
    pub(crate) is_error: bool,
    pub(crate) stop_reason: Option<String>,
    pub(crate) terminal_reason: Option<String>,
    pub(crate) num_turns: Option<u64>,
    pub(crate) api_error_status: Option<u16>,
    pub(crate) errors: Option<Vec<String>>,
    pub(crate) result: Option<String>,
}
