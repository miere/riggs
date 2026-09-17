use std::path::Path;

use riggs_process::{Leader, Pipes};
use tokio::process::Command;

use crate::config::AcpConfig;
use crate::error::AcpError;

const STDERR_TAIL_BYTES: usize = 64 * 1024;
const NESTED_CLAUDE_MARKER: &str = "CLAUDECODE";

pub(crate) fn spawn(config: &AcpConfig, workdir: &Path) -> Result<(Leader, Pipes), AcpError> {
    let mut command = Command::new(&config.command);
    command
        .args(&config.args)
        .current_dir(workdir)
        .env_remove(NESTED_CLAUDE_MARKER);
    for (name, value) in &config.env {
        match value {
            Some(value) => command.env(name, value),
            None => command.env_remove(name),
        };
    }
    Leader::spawn(&mut command, STDERR_TAIL_BYTES).map_err(|source| AcpError::Spawn {
        command: config.command.clone(),
        source,
    })
}
