//! Frames copied from the claude 2.1.271 probe transcripts, trimmed of usage and timing fields
//! Riggs never reads.

use serde_json::{Value, json};

pub const MODEL: &str = "claude-haiku-4-5-20251001";
pub const HOOK_TIMEOUT_TEXT: &str = "PreToolUse hook did not respond before its timeout (host client may be unreachable). The tool call was not executed; other configured hooks may not have completed.";
pub const INTERRUPT_REJECTION: &str = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed.";

fn uuid(seed: u64) -> String {
    format!("00000000-0000-4000-8000-{seed:012x}")
}

pub fn init(session: &str, cwd: &str, tools: &[String], seed: u64) -> Value {
    let mut all: Vec<String> = ["Agent", "Bash", "Edit", "Read", "ToolSearch", "Write"]
        .into_iter()
        .map(str::to_owned)
        .collect();
    all.extend(tools.iter().map(|tool| format!("mcp__riggs__{tool}")));
    json!({
        "type": "system", "subtype": "init", "cwd": cwd, "session_id": session, "tools": all,
        "mcp_servers": [{"name": "riggs", "status": "connected"}], "model": MODEL,
        "permissionMode": "dontAsk", "apiKeySource": "none", "claude_code_version": "2.1.271",
        "output_style": "default",
        "capabilities": ["interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1"],
        "uuid": uuid(seed),
    })
}

pub fn initialized(pid: u32) -> Value {
    json!({
        "output_style": "default", "available_output_styles": ["default"], "pid": pid,
        "current_permission_mode": "dontAsk", "hooks_applied": true, "session_state": "idle",
    })
}

pub fn assistant(session: &str, content: Value, parent: Option<&str>, seed: u64) -> Value {
    json!({
        "type": "assistant",
        "message": {
            "model": MODEL, "id": format!("msg_fake{seed}"), "type": "message", "role": "assistant",
            "content": content, "container": null, "stop_reason": null, "stop_sequence": null,
            "stop_details": null,
        },
        "parent_tool_use_id": parent, "session_id": session, "uuid": uuid(seed),
        "request_id": format!("req_fake{seed}"),
    })
}

pub fn tool_use(id: &str, name: &str, input: &Value) -> Value {
    json!([{"type": "tool_use", "id": id, "name": name, "input": input, "caller": {"type": "direct"}}])
}

pub fn tool_result(
    session: &str,
    id: &str,
    content: &Value,
    is_error: bool,
    parent: Option<&str>,
    seed: u64,
) -> Value {
    json!({
        "type": "user",
        "message": {"role": "user", "content": [
            {"type": "tool_result", "content": content, "is_error": is_error, "tool_use_id": id},
        ]},
        "parent_tool_use_id": parent, "session_id": session, "uuid": uuid(seed),
    })
}

pub fn hook_callback(
    request_id: &str,
    session: &str,
    cwd: &str,
    name: &str,
    input: &Value,
    id: &str,
    agent_id: Option<&str>,
) -> Value {
    let mut hook = json!({
        "session_id": session, "transcript_path": format!("/fake/.claude/projects/fake/{session}.jsonl"),
        "cwd": cwd, "prompt_id": uuid(7), "permission_mode": "dontAsk",
        "hook_event_name": "PreToolUse", "tool_name": name, "tool_input": input, "tool_use_id": id,
    });
    if let (Some(agent_id), Some(fields)) = (agent_id, hook.as_object_mut()) {
        fields.insert("agent_id".to_owned(), json!(agent_id));
        fields.insert("agent_type".to_owned(), json!("general-purpose"));
    }
    json!({
        "type": "control_request", "request_id": request_id,
        "request": {"subtype": "hook_callback", "callback_id": "gate", "input": hook, "tool_use_id": id},
    })
}

pub fn mcp_request(request_id: &str, message: Value) -> Value {
    json!({
        "type": "control_request", "request_id": request_id,
        "request": {"subtype": "mcp_message", "server_name": "riggs", "message": message},
    })
}

pub fn control_success(request_id: &str, response: Value) -> Value {
    json!({"type": "control_response", "response": {"subtype": "success", "request_id": request_id, "response": response}})
}

pub fn result(session: &str, text: &str, num_turns: u64, index: u64) -> Value {
    json!({
        "type": "result", "subtype": "success", "is_error": false, "num_turns": num_turns,
        "result": text, "stop_reason": "end_turn", "terminal_reason": "completed",
        "session_id": session, "total_cost_usd": 0, "permission_denials": [],
        "api_error_status": null, "queued_turn_count": 0, "result_index": index, "uuid": uuid(index),
    })
}

pub fn interrupted(session: &str, index: u64) -> Value {
    json!({
        "type": "result", "subtype": "error_during_execution", "is_error": true, "num_turns": 3,
        "stop_reason": "tool_use", "terminal_reason": "aborted_tools", "session_id": session,
        "errors": ["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=tool_use"],
        "permission_denials": [], "queued_turn_count": 0, "result_index": index, "uuid": uuid(index),
    })
}

pub fn interrupt_marker(session: &str, seed: u64) -> Value {
    json!({
        "type": "user",
        "message": {"role": "user", "content": [{"type": "text", "text": "[Request interrupted by user for tool use]"}]},
        "parent_tool_use_id": null, "session_id": session, "uuid": uuid(seed),
    })
}

pub fn resume_miss(session: &str) -> Value {
    json!({
        "type": "result", "subtype": "error_during_execution", "duration_ms": 0, "duration_api_ms": 0,
        "is_error": true, "num_turns": 0, "stop_reason": null, "session_id": session,
        "total_cost_usd": 0, "modelUsage": {}, "permission_denials": [], "uuid": uuid(0),
        "errors": [format!("No conversation found with session ID: {session}")], "result_index": 0,
    })
}
