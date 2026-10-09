use std::collections::BTreeMap;
use std::path::PathBuf;

use crate::sandbox::SandboxConfig;
use std::time::Duration;

pub const HOOK_TIMEOUT: Duration = Duration::from_secs(3600);
pub const HOOK_MARGIN: Duration = Duration::from_secs(60);
pub const HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(60);
pub const INTERRUPT_GRACE: Duration = Duration::from_secs(30);
pub const IDLE_TIMEOUT: Duration = Duration::from_secs(15 * 60);
pub const MAX_LINE_BYTES: usize = 64 << 20;
const CREDENTIAL_FILE: &str = ".credentials.json";
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
    /// How long a session's process may sit with nothing to do before it is stopped. The session
    /// stays: its next prompt resumes the conversation in a fresh process. Zero never stops it.
    pub idle_timeout: Duration,
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
            idle_timeout: IDLE_TIMEOUT,
            max_line_bytes: MAX_LINE_BYTES,
            sign_in: SignInConfig::default(),
            credential_file: None,
            sandbox: SandboxConfig::default(),
        }
    }

    /// A variable as the agent will see it: its own environment wins over the node's.
    fn var(&self, name: &str) -> Option<PathBuf> {
        self.env
            .get(name)
            .map(PathBuf::from)
            .or_else(|| std::env::var_os(name).map(PathBuf::from))
            .filter(|path| !path.as_os_str().is_empty())
    }

    /// Claude Code's configuration directory: `CLAUDE_CONFIG_DIR`, or `.claude` in the home.
    pub(crate) fn claude_dir(&self) -> Option<PathBuf> {
        self.var("CLAUDE_CONFIG_DIR")
            .or_else(|| self.var("HOME").map(|home| home.join(".claude")))
    }

    /// Where Claude Code's credential is when it is a file rather than a keychain entry.
    pub(crate) fn credential_path(&self) -> Option<PathBuf> {
        self.credential_file
            .clone()
            .or_else(|| self.claude_dir().map(|dir| dir.join(CREDENTIAL_FILE)))
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    #[test]
    fn the_credential_follows_claude_codes_own_directory() {
        let mut config = ClaudeCodeConfig::new("/tmp");
        config.env.insert("HOME".into(), "/home/me".into());
        assert_eq!(
            config.credential_path().unwrap(),
            PathBuf::from("/home/me/.claude/.credentials.json")
        );

        config
            .env
            .insert("CLAUDE_CONFIG_DIR".into(), "/home/me/work".into());
        assert_eq!(
            config.credential_path().unwrap(),
            PathBuf::from("/home/me/work/.credentials.json")
        );

        config.credential_file = Some(PathBuf::from("/creds/one.json"));
        assert_eq!(
            config.credential_path().unwrap(),
            PathBuf::from("/creds/one.json")
        );
    }
}
