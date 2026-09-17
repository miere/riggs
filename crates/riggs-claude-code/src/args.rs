use riggs_node::SessionKey;

use crate::config::ClaudeCodeConfig;

pub(crate) const DISALLOWED_TOOLS: &str = "AskUserQuestion,EnterPlanMode,ExitPlanMode";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Launch {
    Create,
    Resume,
}

pub(crate) fn argv(config: &ClaudeCodeConfig, key: &SessionKey, launch: Launch) -> Vec<String> {
    let mut args: Vec<String> = [
        "-p",
        "--input-format",
        "stream-json",
        "--output-format",
        "stream-json",
        "--verbose",
        "--permission-mode",
        "dontAsk",
        "--permission-prompt-tool",
        "stdio",
        "--disallowedTools",
        DISALLOWED_TOOLS,
    ]
    .into_iter()
    .map(str::to_owned)
    .collect();
    if let Some(model) = &config.model {
        args.extend(["--model".to_owned(), model.clone()]);
    }
    args.extend(config.args.iter().cloned());
    let flag = match launch {
        Launch::Create => "--session-id",
        Launch::Resume => "--resume",
    };
    args.extend([flag.to_owned(), key.to_string()]);
    args
}
