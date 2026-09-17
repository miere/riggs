use serde::Deserialize;
use serde_json::Value;

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Script {
    /// Names the probe transcripts the steps were copied from, so a drift can be re-checked.
    #[serde(default)]
    pub source: Vec<String>,
    /// Holds the `initialize` answer until this file exists, so a test can act while a spawn is
    /// still in progress.
    #[serde(default)]
    pub ready_file: Option<String>,
    /// Indexed by every prompt the session has ever had, across processes, so a resumed process
    /// continues the story; the last list repeats.
    pub turns: Vec<Vec<Step>>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub enum Step {
    Emit(Value),
    Say(String),
    Result {
        #[serde(default)]
        text: String,
        #[serde(default = "one")]
        num_turns: u64,
    },
    /// Honours the hook timeout from `initialize`, so a node that answers too late is caught.
    Hook {
        id: String,
        name: String,
        input: Value,
        #[serde(default)]
        agent_id: Option<String>,
        #[serde(default)]
        parent: Option<String>,
        #[serde(default)]
        allow: Vec<Step>,
        #[serde(default)]
        deny: Vec<Step>,
    },
    CallTool {
        id: String,
        name: String,
        arguments: Value,
        #[serde(rename = "as")]
        var: String,
    },
    ToolResult {
        id: String,
        content: Value,
        #[serde(default)]
        is_error: bool,
        #[serde(default)]
        parent: Option<String>,
    },
    Stderr(String),
    BigLine(usize),
    Touch(String),
    Truncate(String),
    SpawnGrandchild(String),
    WaitFile(String),
    Hang,
    Exit(i32),
}

fn one() -> u64 {
    1
}
