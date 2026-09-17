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
    #[serde(default)]
    pub turns: Vec<Vec<Step>>,
    #[serde(default)]
    pub auth: Auth,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Auth {
    /// Printed by `auth status`, which exits 1 unless it says `loggedIn: true`. Signed in when unset.
    #[serde(default)]
    pub status: Option<Value>,
    /// One list per `auth login` run in the state directory; the last repeats.
    #[serde(default)]
    pub logins: Vec<Vec<LoginStep>>,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "snake_case", deny_unknown_fields)]
pub enum LoginStep {
    Say(String),
    /// Written without a newline, as the real CLI writes its code prompt.
    Prompt(String),
    Stderr(String),
    /// Reads lines like the real CLI: one without `#` is refused and read again, `accept` signs
    /// in and exits 0, anything else fails with exit 1.
    AwaitCode {
        accept: String,
    },
    SpawnGrandchild(String),
    Hang,
    Exit(i32),
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
