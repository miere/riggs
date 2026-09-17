use std::collections::BTreeMap;
use std::path::PathBuf;
use std::time::Duration;

pub const HOOK_TIMEOUT: Duration = Duration::from_secs(3600);
pub const HOOK_MARGIN: Duration = Duration::from_secs(60);
pub const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(60);
pub const INTERRUPT_GRACE: Duration = Duration::from_secs(30);
pub const MAX_LINE_BYTES: usize = 64 << 20;

/// Plain data: loading it from a file belongs to the binary.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ClaudeCodeConfig {
    pub command: PathBuf,
    /// Appended after Riggs's own flags, so they can add to the launch but not undo the gate.
    pub args: Vec<String>,
    pub model: Option<String>,
    /// Applied over the node's own environment, which loses `CLAUDECODE` first.
    pub env: BTreeMap<String, String>,
    /// The same for every spawn: Claude Code files transcripts by working directory, and a changed
    /// one can fork a session id.
    pub workdir: PathBuf,
    /// Registered with Claude Code, which denies a call whose hook outlives it.
    pub hook_timeout: Duration,
    /// Riggs denies a held call this long before `hook_timeout`, so the model reads a deny rather
    /// than Claude Code's internal timeout text.
    pub hook_margin: Duration,
    pub handshake_timeout: Duration,
    /// How long an interrupted turn may take to stop before its process is killed.
    pub interrupt_grace: Duration,
    /// A longer stdout line fails the turn and restarts the process rather than stalling it.
    pub max_line_bytes: usize,
}

impl ClaudeCodeConfig {
    pub fn new(workdir: impl Into<PathBuf>) -> Self {
        Self {
            command: PathBuf::from("claude"),
            args: Vec::new(),
            model: None,
            env: BTreeMap::new(),
            workdir: workdir.into(),
            hook_timeout: HOOK_TIMEOUT,
            hook_margin: HOOK_MARGIN,
            handshake_timeout: HANDSHAKE_TIMEOUT,
            interrupt_grace: INTERRUPT_GRACE,
            max_line_bytes: MAX_LINE_BYTES,
        }
    }
}
