use rax::ToolCall;
use rax::id::ToolCallId;
use rax::tool::{DeniedBy, ToolKind};
use serde_json::Value;

use crate::wire::MCP_SERVER;

const TITLE_KEYS: &[&str] = &["command", "file_path", "path", "pattern", "url"];

pub(crate) fn call(id: &str, name: &str, input: Value) -> ToolCall {
    ToolCall {
        id: ToolCallId(id.to_owned()),
        name: name.to_owned(),
        title: Some(title(&input)),
        kind: kind(name),
        input: Some(input),
        content: Vec::new(),
    }
}

pub(crate) fn kind(name: &str) -> ToolKind {
    let ask = format!("mcp__{MCP_SERVER}__ask");
    let plan = format!("mcp__{MCP_SERVER}__present_plan");
    match name {
        "Read" | "NotebookRead" => ToolKind::Read,
        "Write" | "Edit" | "MultiEdit" | "NotebookEdit" => ToolKind::Edit,
        "Glob" | "Grep" | "LS" | "ToolSearch" => ToolKind::Search,
        "Bash" | "BashOutput" | "KillShell" | "KillBash" | "Monitor" | "PowerShell" => {
            ToolKind::Execute
        }
        "WebFetch" | "WebSearch" => ToolKind::Fetch,
        "Agent" | "Task" | "TodoWrite" | "TaskCreate" | "TaskUpdate" | "Skill" => ToolKind::Think,
        "EnterPlanMode" | "ExitPlanMode" => ToolKind::SwitchMode,
        "AskUserQuestion" => ToolKind::Think,
        other if other == ask || other == plan => ToolKind::Think,
        _ => ToolKind::Other,
    }
}

fn title(input: &Value) -> String {
    TITLE_KEYS
        .iter()
        .filter_map(|key| input.get(*key).and_then(Value::as_str))
        .find(|value| !value.trim().is_empty())
        .map_or_else(|| input.to_string(), str::to_owned)
}

pub(crate) fn deny_reason(by: DeniedBy, reason: Option<String>) -> String {
    if let Some(reason) = reason.filter(|reason| !reason.trim().is_empty()) {
        return reason;
    }
    match by {
        DeniedBy::User => "The person denied this tool call. Do not retry it — ask them how they would like to proceed.",
        DeniedBy::Policy => "This tool call is not allowed here. Do not retry it.",
        DeniedBy::Timeout => "Nobody answered the approval request in time, so this call was not run.",
        DeniedBy::Unavailable | DeniedBy::Other => {
            "Nobody could be asked to approve this, so it was not run. Do not retry it."
        }
    }
    .to_owned()
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[test]
    fn a_tool_call_carries_the_raw_name_a_kind_and_a_readable_title() {
        let bash = call(
            "toolu_1",
            "Bash",
            json!({"command": "ls", "description": "list"}),
        );
        assert_eq!(
            (bash.name.as_str(), bash.kind, bash.title.as_deref()),
            ("Bash", ToolKind::Execute, Some("ls"))
        );
        let ask = call("toolu_2", "mcp__riggs__ask", json!({"questions": []}));
        assert_eq!(ask.kind, ToolKind::Think);
        assert_eq!(ask.title.as_deref(), Some(r#"{"questions":[]}"#));
        assert_eq!(kind("mcp__riggs__attach"), ToolKind::Other);
        assert_eq!(kind("Grep"), ToolKind::Search);
    }

    #[test]
    fn a_deny_always_carries_a_reason() {
        assert_eq!(
            deny_reason(DeniedBy::Policy, Some("  ".to_owned())),
            "This tool call is not allowed here. Do not retry it."
        );
        assert_eq!(deny_reason(DeniedBy::User, Some("no".to_owned())), "no");
    }
}
