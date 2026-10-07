use std::collections::BTreeMap;
use std::path::PathBuf;
use std::time::Duration;

pub const STARTUP_TIMEOUT: Duration = Duration::from_secs(60);
pub const CANCEL_GRACE_PERIOD: Duration = Duration::from_secs(30);
pub const PERMISSION_TIMEOUT: Duration = Duration::from_secs(30 * 60);
pub const IDLE_TIMEOUT: Duration = Duration::from_secs(15 * 60);

/// Plain data, so the binary maps its own config format onto it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AcpConfig {
    pub command: String,
    pub args: Vec<String>,
    /// `None` removes the variable from the inherited environment.
    pub env: BTreeMap<String, Option<String>>,
    pub workdir: Option<PathBuf>,
    /// ACP cancel is a notification every agent must honour, so there is nothing to probe.
    pub interruptible: Option<bool>,
    /// Bounds spawning plus the handshake and the `session/new` or `session/load` behind it.
    pub startup_timeout: Duration,
    /// How long a cancelled turn waits for the agent's answer before its process is killed.
    pub cancel_grace_period: Duration,
    /// Keep it below any agent-side timeout, so the agent hears a reject rather than an error.
    pub permission_timeout: Duration,
    /// How long a session's agent may sit with nothing to do before it is stopped. An agent that
    /// can reload sessions picks the conversation back up at the next prompt; for one that cannot,
    /// the session ends. Zero never stops it.
    pub idle_timeout: Duration,
}

impl AcpConfig {
    pub fn new(command: impl Into<String>) -> Self {
        Self {
            command: command.into(),
            args: Vec::new(),
            env: BTreeMap::new(),
            workdir: None,
            interruptible: None,
            startup_timeout: STARTUP_TIMEOUT,
            cancel_grace_period: CANCEL_GRACE_PERIOD,
            permission_timeout: PERMISSION_TIMEOUT,
            idle_timeout: IDLE_TIMEOUT,
        }
    }
}
