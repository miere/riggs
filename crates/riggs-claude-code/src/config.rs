use std::collections::BTreeMap;
use std::path::PathBuf;

use crate::sandbox::SandboxConfig;
use std::time::Duration;

pub const HOOK_TIMEOUT: Duration = Duration::from_secs(3600);
pub const HOOK_MARGIN: Duration = Duration::from_secs(60);
pub const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(60);
pub const INTERRUPT_GRACE: Duration = Duration::from_secs(30);
pub const MAX_LINE_BYTES: usize = 64 << 20;
pub const SIGN_IN_LINK_WAIT: Duration = Duration::from_secs(60);
/// Longer than a sign-in someone asked for: the owner is not expecting this one and has to notice it.
pub const SIGN_IN_EXPIRY: Duration = Duration::from_secs(20 * 60);
pub const SIGN_IN_CONFIRM_WAIT: Duration = Duration::from_secs(30);
pub const SIGN_IN_SHOW_WAIT: Duration = Duration::from_secs(10);
/// Stops a node whose owner cannot be reached from asking again on every failing turn.
pub const SIGN_IN_COOLDOWN: Duration = Duration::from_secs(10 * 60);
pub const SIGN_IN_DRAIN: Duration = Duration::from_secs(1);
pub const STATUS_TIMEOUT: Duration = Duration::from_secs(15);

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
    pub sign_in: SignInConfig,
    /// Where Claude Code's credential is kept, when it is not the login keychain or
    /// `$HOME/.claude/.credentials.json`.
    pub credential_file: Option<PathBuf>,
    /// How the agent is confined. Off by default, because a box is a promise about a machine
    /// this crate cannot check by itself.
    pub sandbox: SandboxConfig,
}

/// Timings for putting a `claude auth login` in front of the node's owner when the credential fails.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SignInConfig {
    /// A login that prints no link by then is broken, and there is nothing to show anyone.
    pub link_wait: Duration,
    pub expiry: Duration,
    /// A finished login still waits for the gateway to confirm its owner may use it.
    pub confirm_wait: Duration,
    /// How long a failing turn waits for the sign-in to be shown before it reports `credential`.
    pub show_wait: Duration,
    pub cooldown: Duration,
    /// How long `credential.renew` waits for the open sign-in to stop before refusing to start another.
    pub drain: Duration,
    pub status_timeout: Duration,
    /// Holds the no-op `open` stand-ins that keep the login from opening a browser on this machine.
    pub scratch_dir: PathBuf,
}

impl Default for SignInConfig {
    fn default() -> Self {
        Self {
            link_wait: SIGN_IN_LINK_WAIT,
            expiry: SIGN_IN_EXPIRY,
            confirm_wait: SIGN_IN_CONFIRM_WAIT,
            show_wait: SIGN_IN_SHOW_WAIT,
            cooldown: SIGN_IN_COOLDOWN,
            drain: SIGN_IN_DRAIN,
            status_timeout: STATUS_TIMEOUT,
            scratch_dir: std::env::temp_dir(),
        }
    }
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
            sign_in: SignInConfig::default(),
            credential_file: None,
            sandbox: SandboxConfig::default(),
        }
    }
}
