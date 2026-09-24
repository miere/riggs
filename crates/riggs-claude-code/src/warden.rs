//! Keeps Claude Code's credential fresh from outside any sandbox.
//!
//! A sandboxed `claude` can read its OAuth credential but cannot write one back, and Anthropic's
//! refresh tokens rotate: the server retires the old one the moment it issues a new one. So a
//! refresh whose result cannot be saved does not merely fail, it destroys the credential, and the
//! next turn presents a token the server has already retired.
//!
//! The warden watches the expiry and, shortly before it lapses, runs one minimal turn in a process
//! that was never sandboxed. Claude Code refreshes on that turn and saves the result normally.
//! Nothing here handles the secret: the expiry is read out of the credential and the rest of the
//! bytes are dropped.
//!
//! Every node run by one user shares one credential, and every node's warden reads the same expiry
//! and fires in the same instant. Two forcing turns then present the same refresh token, one of
//! them presents it after it was retired, and the credential is lost. So the forcing turn runs
//! under a file lock kept beside the credential, and a warden that waited for it looks again
//! before spending a turn of its own.

use std::fs::{File, OpenOptions, TryLockError};
use std::os::unix::fs::OpenOptionsExt as _;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use serde::Deserialize;
use time::OffsetDateTime;
use tokio::process::Command;
use tokio::time::timeout;
use tokio_util::sync::CancellationToken;

use crate::config::ClaudeCodeConfig;
use crate::health::Health;
use crate::process::Ctx;

/// Claude Code refreshes only inside its own last-five-minutes window, so the forcing turn aims
/// inside that with margin on both sides.
const FORCE_AT: Duration = Duration::from_secs(3 * 60);
/// How often to look again while inside the window, until the expiry actually moves.
const RETRY: Duration = Duration::from_secs(30);
/// Caps one wait, so a suspended machine re-reads the real expiry promptly after waking rather
/// than sleeping out a timer set before it went away.
const MAX_SLEEP: Duration = Duration::from_secs(5 * 60);
/// A minimal turn answers in seconds; longer than this means the CLI is wedged.
const REFRESH_TIMEOUT: Duration = Duration::from_secs(90);
/// How many forcing turns one unchanged expiry may provoke before backing off, so a credential
/// that can no longer refresh at all cannot spend a turn every retry for as long as the node runs.
const MAX_ATTEMPTS: u32 = 6;
/// Two failed passes, not one: a keychain lookup can lose to a locked keychain or a waking
/// machine, and an alert on the first miss would cry wolf.
const DEGRADED_AFTER: u32 = 2;
/// Long enough for another warden's forcing turn to finish; past it, the holder is presumed wedged
/// and this pass gives up rather than race it.
const LOCK_WAIT: Duration = Duration::from_secs(REFRESH_TIMEOUT.as_secs() + 30);
const LOCK_POLL: Duration = Duration::from_millis(250);
const LOCK_FILE: &str = ".riggs-refresh.lock";
const KEYCHAIN_SERVICE: &str = "Claude Code-credentials";
/// By absolute path, so a PATH entry cannot substitute another binary for a credential read.
const SECURITY: &str = "/usr/bin/security";

/// The credential's expiry, and nothing else from the blob it came out of.
#[derive(Deserialize)]
struct Blob {
    #[serde(rename = "claudeAiOauth")]
    claude_ai_oauth: Oauth,
}

#[derive(Deserialize)]
struct Oauth {
    /// Milliseconds since the Unix epoch.
    #[serde(rename = "expiresAt")]
    expires_at: i64,
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum ExpiryError {
    #[error("the credential store could not be read: {0}")]
    Unreadable(String),
    #[error("the credential carries no expiry")]
    NoExpiry,
}

struct Watch {
    /// The expiry the last forcing turns were aimed at, to tell a refresh from a repeat.
    aimed_at: Option<OffsetDateTime>,
    attempts: u32,
    failures: u32,
}

/// Watches this node's credential until `stop` is cancelled.
pub(crate) async fn watch(ctx: Arc<Ctx>, stop: CancellationToken) {
    let (config, health) = (&ctx.config, &ctx.health);
    let mut watch = Watch {
        aimed_at: None,
        attempts: 0,
        failures: 0,
    };
    loop {
        let wait = pass(config, health, &mut watch).await;
        tokio::select! {
            () = stop.cancelled() => return,
            () = tokio::time::sleep(wait) => {}
        }
    }
}

/// What to do after one look at the credential.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Step {
    Sleep(Duration),
    Refresh,
}

/// Decides from the time left and how many turns this expiry has already cost. Kept apart from
/// the reading and the running so the schedule can be tested without either.
fn step(left: time::Duration, attempts: u32) -> Step {
    let force_at = time::Duration::seconds(FORCE_AT.as_secs().cast_signed());
    if left > force_at {
        let until: Duration = (left - force_at).try_into().unwrap_or(MAX_SLEEP);
        return Step::Sleep(until.min(MAX_SLEEP));
    }
    if attempts >= MAX_ATTEMPTS {
        return Step::Sleep(MAX_SLEEP);
    }
    Step::Refresh
}

/// One look at the credential; returns how long to wait before the next.
async fn pass(config: &ClaudeCodeConfig, health: &Health, watch: &mut Watch) -> Duration {
    let expiry = match read_expiry(config).await {
        Ok(expiry) => expiry,
        Err(err) => {
            watch.failures += 1;
            if watch.failures >= DEGRADED_AFTER {
                health.degraded(&format!("the credential's expiry could not be read: {err}"));
            } else {
                tracing::debug!(error = %err, "could not read the credential's expiry");
            }
            return RETRY;
        }
    };
    watch.failures = 0;
    health.expires_at(expiry);
    if watch.aimed_at != Some(expiry) {
        watch.aimed_at = Some(expiry);
        watch.attempts = 0;
    }
    let left = expiry - OffsetDateTime::now_utc();
    match step(left, watch.attempts) {
        Step::Sleep(wait) => return wait,
        Step::Refresh => {}
    }
    watch.attempts += 1;
    tracing::info!(
        attempt = watch.attempts,
        "refreshing Claude Code's credential before it lapses"
    );
    match refresh_alone(config, expiry).await {
        Ok(Refreshed::Ran) => RETRY,
        Ok(Refreshed::ByAnother) => {
            tracing::info!("another node refreshed the credential while this one waited");
            // The next pass reads the new expiry without delay.
            Duration::ZERO
        }
        Err(reason) => {
            tracing::warn!(%reason, "the credential refresh did not run");
            RETRY
        }
    }
}

#[derive(Debug, PartialEq, Eq)]
enum Refreshed {
    Ran,
    ByAnother,
}

/// The forcing turn, run by at most one warden per credential at a time. `aimed_at` is the expiry
/// this warden read before it queued: once the lock is held, a different one means the turn it
/// waited on already did the work.
async fn refresh_alone(
    config: &ClaudeCodeConfig,
    aimed_at: OffsetDateTime,
) -> Result<Refreshed, String> {
    let path = lock_path(config)?;
    let _held = lock(&path).await?;
    match read_expiry(config).await {
        Ok(expiry) if expiry != aimed_at => return Ok(Refreshed::ByAnother),
        Ok(_) => {}
        Err(err) => return Err(err.to_string()),
    }
    force_refresh(config).await.map(|()| Refreshed::Ran)
}

/// Beside the credential, so nodes share a lock exactly when they share a credential. A keychain
/// credential is keyed by Claude Code's configuration directory, as its keychain entry is.
fn lock_path(config: &ClaudeCodeConfig) -> Result<PathBuf, String> {
    if let Some(file) = &config.credential_file {
        let dir = file.parent().unwrap_or_else(|| Path::new("."));
        return Ok(dir.join(LOCK_FILE));
    }
    let var = |name: &str| {
        config
            .env
            .get(name)
            .map(PathBuf::from)
            .or_else(|| std::env::var_os(name).map(PathBuf::from))
            .filter(|path| !path.as_os_str().is_empty())
    };
    let dir = var("CLAUDE_CONFIG_DIR")
        .or_else(|| var("HOME").map(|home| home.join(".claude")))
        .ok_or("HOME is not set, so there is nowhere to keep the refresh lock")?;
    Ok(dir.join(LOCK_FILE))
}

/// Polled rather than blocked on, so a wedged holder costs this pass `LOCK_WAIT` and no thread.
/// The kernel drops the lock with the file, however its holder exits.
async fn lock(path: &Path) -> Result<File, String> {
    let describe = |err: std::io::Error| format!("cannot lock {}: {err}", path.display());
    if let Some(dir) = path.parent() {
        tokio::fs::create_dir_all(dir).await.map_err(describe)?;
    }
    let file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(path)
        .map_err(describe)?;
    let deadline = tokio::time::Instant::now() + LOCK_WAIT;
    loop {
        match file.try_lock() {
            Ok(()) => return Ok(file),
            Err(TryLockError::Error(err)) => return Err(describe(err)),
            Err(TryLockError::WouldBlock) if tokio::time::Instant::now() >= deadline => {
                return Err(format!(
                    "another refresh held {} for over {} s",
                    path.display(),
                    LOCK_WAIT.as_secs()
                ));
            }
            Err(TryLockError::WouldBlock) => tokio::time::sleep(LOCK_POLL).await,
        }
    }
}

/// One minimal turn, outside any sandbox, so Claude Code refreshes and saves the result. The
/// answer is thrown away: the handshake is the whole point.
async fn force_refresh(config: &ClaudeCodeConfig) -> Result<(), String> {
    let mut command = Command::new(&config.command);
    command
        .args(["-p", "ok"])
        .current_dir(&config.workdir)
        .env_remove("CLAUDECODE")
        .envs(&config.env)
        .stdin(Stdio::null())
        .kill_on_drop(true);
    if let Some(model) = &config.model {
        command.args(["--model", model]);
    }
    let run = timeout(REFRESH_TIMEOUT, command.output()).await;
    match run {
        Err(_) => Err(format!(
            "the refresh turn did not finish within {} s",
            REFRESH_TIMEOUT.as_secs()
        )),
        Ok(Err(err)) => Err(err.to_string()),
        Ok(Ok(output)) if output.status.success() => Ok(()),
        Ok(Ok(output)) => Err(one_line(&String::from_utf8_lossy(&output.stderr))),
    }
}

/// On macOS the live store is the login keychain, so it is read first: a machine can also hold a
/// credentials file left stale by an earlier login, and acting on a months-old expiry would be
/// worse than not looking.
async fn read_expiry(config: &ClaudeCodeConfig) -> Result<OffsetDateTime, ExpiryError> {
    if let Some(path) = &config.credential_file {
        let raw = tokio::fs::read(path)
            .await
            .map_err(|err| ExpiryError::Unreadable(err.to_string()))?;
        return parse_expiry(&raw);
    }
    let mut failures = Vec::new();
    if cfg!(target_os = "macos") {
        match from_keychain(config).await {
            Ok(expiry) => return Ok(expiry),
            Err(err) => failures.push(format!("keychain: {err}")),
        }
    }
    match from_file(config).await {
        Ok(expiry) => Ok(expiry),
        Err(err) => {
            failures.push(format!("credentials file: {err}"));
            Err(ExpiryError::Unreadable(failures.join("; ")))
        }
    }
}

async fn from_keychain(config: &ClaudeCodeConfig) -> Result<OffsetDateTime, ExpiryError> {
    let mut command = Command::new(SECURITY);
    command
        .args(["find-generic-password", "-w", "-s", KEYCHAIN_SERVICE])
        .stdin(Stdio::null())
        .kill_on_drop(true);
    if let Some(home) = config.env.get("HOME") {
        command.env("HOME", home);
    }
    let output = command
        .output()
        .await
        .map_err(|err| ExpiryError::Unreadable(err.to_string()))?;
    if !output.status.success() {
        return Err(ExpiryError::Unreadable(one_line(&String::from_utf8_lossy(
            &output.stderr,
        ))));
    }
    parse_expiry(&output.stdout)
}

async fn from_file(config: &ClaudeCodeConfig) -> Result<OffsetDateTime, ExpiryError> {
    let home = config
        .env
        .get("HOME")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("HOME").map(PathBuf::from))
        .ok_or_else(|| ExpiryError::Unreadable("HOME is not set".to_owned()))?;
    let raw = tokio::fs::read(home.join(".claude/.credentials.json"))
        .await
        .map_err(|err| ExpiryError::Unreadable(err.to_string()))?;
    parse_expiry(&raw)
}

/// The bytes here are a credential, so nothing derived from them leaves but the timestamp, and no
/// error quotes the input.
fn parse_expiry(raw: &[u8]) -> Result<OffsetDateTime, ExpiryError> {
    let blob: Blob = serde_json::from_slice(raw)
        .map_err(|_| ExpiryError::Unreadable("it is not the expected JSON".to_owned()))?;
    let millis = blob.claude_ai_oauth.expires_at;
    if millis <= 0 {
        return Err(ExpiryError::NoExpiry);
    }
    OffsetDateTime::from_unix_timestamp_nanos(i128::from(millis) * 1_000_000)
        .map_err(|_| ExpiryError::NoExpiry)
}

fn one_line(text: &str) -> String {
    let joined = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if joined.chars().count() <= 200 {
        return joined;
    }
    let mut clipped: String = joined.chars().take(199).collect();
    clipped.push('…');
    clipped
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use super::*;

    #[test]
    fn a_credential_with_room_left_is_looked_at_again_before_it_lapses() {
        let minutes = |n| time::Duration::minutes(n);
        assert_eq!(step(minutes(4), 0), Step::Sleep(Duration::from_secs(60)));
        assert_eq!(step(minutes(60), 0), Step::Sleep(MAX_SLEEP));
        assert_eq!(step(minutes(3), 0), Step::Refresh);
        assert_eq!(step(minutes(1), 3), Step::Refresh);
        assert_eq!(
            step(-minutes(5), 0),
            Step::Refresh,
            "a lapsed one still tries"
        );
    }

    #[test]
    fn an_expiry_that_will_not_move_stops_costing_turns() {
        assert_eq!(
            step(time::Duration::minutes(1), MAX_ATTEMPTS - 1),
            Step::Refresh
        );
        assert_eq!(
            step(time::Duration::minutes(1), MAX_ATTEMPTS),
            Step::Sleep(MAX_SLEEP)
        );
    }

    #[tokio::test]
    async fn the_expiry_comes_from_the_file_the_config_names() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("creds.json");
        std::fs::write(&path, br#"{"claudeAiOauth":{"expiresAt":1789000000000}}"#).unwrap();
        let mut config = ClaudeCodeConfig::new("/tmp");
        config.credential_file = Some(path);
        let expiry = read_expiry(&config).await.unwrap();
        assert_eq!(expiry.unix_timestamp(), 1_789_000_000);

        config.credential_file = Some(dir.path().join("missing.json"));
        assert!(read_expiry(&config).await.is_err());
    }

    /// Two nodes on one credential, both aimed at the same expiry: the second waits out the first
    /// and finds the work done, rather than presenting a refresh token the first just retired.
    #[tokio::test]
    async fn two_wardens_on_one_credential_refresh_it_once() {
        let dir = tempfile::tempdir().unwrap();
        let creds = dir.path().join("creds.json");
        std::fs::write(&creds, br#"{"claudeAiOauth":{"expiresAt":1789000000000}}"#).unwrap();
        let claude = dir.path().join("claude");
        std::fs::write(
            &claude,
            "#!/bin/sh\necho run >> \"$RUNS\"\nsleep 0.5\nprintf '{\"claudeAiOauth\":{\"expiresAt\":1789030000000}}' > \"$CREDS\"\n",
        )
        .unwrap();
        std::fs::set_permissions(&claude, std::os::unix::fs::PermissionsExt::from_mode(0o700))
            .unwrap();
        let mut config = ClaudeCodeConfig::new(dir.path());
        config.command = claude;
        config.credential_file = Some(creds.clone());
        let runs = dir.path().join("runs");
        config.env.insert("RUNS".into(), runs.display().to_string());
        config
            .env
            .insert("CREDS".into(), creds.display().to_string());
        let aimed = read_expiry(&config).await.unwrap();

        let (first, second) =
            tokio::join!(refresh_alone(&config, aimed), refresh_alone(&config, aimed));
        let mut outcomes = [first.unwrap(), second.unwrap()];
        outcomes.sort_by_key(|outcome| *outcome == Refreshed::ByAnother);
        assert_eq!(outcomes, [Refreshed::Ran, Refreshed::ByAnother]);
        assert_eq!(std::fs::read_to_string(&runs).unwrap(), "run\n");
    }

    #[test]
    fn the_lock_sits_beside_the_credential_it_guards() {
        let mut config = ClaudeCodeConfig::new("/tmp");
        config.credential_file = Some(PathBuf::from("/creds/one.json"));
        assert_eq!(
            lock_path(&config).unwrap(),
            Path::new("/creds/.riggs-refresh.lock")
        );

        config.credential_file = None;
        config.env.insert("HOME".into(), "/home/me".into());
        assert_eq!(
            lock_path(&config).unwrap(),
            Path::new("/home/me/.claude/.riggs-refresh.lock")
        );
        config
            .env
            .insert("CLAUDE_CONFIG_DIR".into(), "/home/me/work".into());
        assert_eq!(
            lock_path(&config).unwrap(),
            Path::new("/home/me/work/.riggs-refresh.lock")
        );
    }

    #[test]
    fn an_expiry_is_read_from_the_credential_and_nothing_else_is() {
        let raw = br#"{"claudeAiOauth":{"accessToken":"secret","refreshToken":"secret","expiresAt":1789000000000,"scopes":["user:inference"]}}"#;
        let expiry = parse_expiry(raw).unwrap();
        assert_eq!(expiry.unix_timestamp(), 1_789_000_000);
    }

    #[test]
    fn a_credential_with_no_expiry_is_refused_without_quoting_it() {
        let cases: [&[u8]; 3] = [
            br#"{"claudeAiOauth":{"expiresAt":0}}"#,
            br#"{"claudeAiOauth":{"accessToken":"hunter2"}}"#,
            b"not json at all: hunter2",
        ];
        for raw in cases {
            let err = parse_expiry(raw).unwrap_err().to_string();
            assert!(!err.contains("hunter2"), "{err}");
        }
    }
}
