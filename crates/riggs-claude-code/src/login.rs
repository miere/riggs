use std::collections::VecDeque;
use std::fs::OpenOptions;
use std::io::{self, Write as _};
use std::os::unix::fs::OpenOptionsExt as _;
use std::path::Path;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use riggs_process::{Descendants, Leader, Pipes, Tail};
use tempfile::TempDir;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::process::{ChildStdin, ChildStdout, Command};
use tokio::sync::{mpsc, watch};
use tokio::task::JoinHandle;
use tokio::time::{sleep, timeout};
use tokio_util::sync::CancellationToken;

use crate::config::ClaudeCodeConfig;
use crate::profile::Profile;

const LAUNCHERS: &[&str] = &["open", "xdg-open"];
const LAUNCHER_SCRIPT: &[u8] = b"#!/bin/sh\nexit 0\n";
const OUTPUT_BYTES: usize = 16 << 10;
const DETAIL_CHARS: usize = 400;
const VERSION_TIMEOUT: Duration = Duration::from_secs(5);
const STOP_WAIT: Duration = Duration::from_secs(5);

#[derive(Debug, thiserror::Error)]
pub(crate) enum LoginError {
    #[error("could not prepare the sign-in command: {0}")]
    Guard(#[source] io::Error),
    #[error("could not start the sign-in command: {0}")]
    Spawn(#[source] io::Error),
    #[error("the sign-in command finished without offering a sign-in link: {detail}{drift}")]
    NoLink { detail: String, drift: String },
    #[error("the sign-in command did not offer a sign-in link within {secs} s: {detail}{drift}")]
    LinkTimeout {
        secs: u64,
        detail: String,
        drift: String,
    },
    #[error("the sign-in was cancelled before it offered a link")]
    Cancelled,
}

#[derive(Debug, Clone)]
enum Scan {
    Searching,
    Found(String),
    Ended,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Exit {
    Succeeded,
    Failed,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

pub(crate) struct Login {
    stdin: tokio::sync::Mutex<ChildStdin>,
    kill: mpsc::Sender<()>,
    exited: watch::Receiver<Option<Exit>>,
    output: Arc<Mutex<VecDeque<u8>>>,
    stderr: Tail,
    _guard: TempDir,
}

impl Login {
    /// Runs `profile` and returns once it offers a link, because a sign-in with nothing to open
    /// cannot be finished by anyone.
    pub(crate) async fn start(
        config: &ClaudeCodeConfig,
        profile: &Profile,
        stop: &CancellationToken,
    ) -> Result<(Self, String), LoginError> {
        let guard = guard(&config.sign_in.scratch_dir).map_err(LoginError::Guard)?;
        let inherited = config
            .env
            .get("PATH")
            .cloned()
            .or_else(|| std::env::var("PATH").ok());
        let path = match inherited.filter(|path| !path.is_empty()) {
            Some(path) => format!("{}:{path}", guard.path().display()),
            None => guard.path().display().to_string(),
        };
        let mut command = Command::new(&profile.command);
        command
            .args(&profile.args)
            .current_dir(&config.workdir)
            .env_remove("CLAUDECODE")
            .envs(&config.env);
        if profile.suppress_browser {
            command
                .env("PATH", path)
                .env("BROWSER", guard.path().join("open"));
        }
        let (scan, mut scanned) = watch::channel(Scan::Searching);
        // The link is looked for on stderr as well as stdout, and each stream is scanned by its
        // own finder: `gcloud` prints its link on stderr and only the prompt that follows it on
        // stdout, and interleaving the two through one buffer would split a URL across a line
        // that then matches nothing.
        let mut errors = Finder::new(profile.clone());
        let found = scan.clone();
        let (
            leader,
            Pipes {
                stdin,
                stdout,
                stderr,
            },
        ) = Leader::spawn_tapped(&mut command, OUTPUT_BYTES, move |chunk| {
            if let Some(url) = errors.feed(chunk) {
                found.send_replace(Scan::Found(url));
            }
        })
        .map_err(LoginError::Spawn)?;
        tracing::debug!(pid = leader.pid(), "Claude Code sign-in started");
        let (kill, kills) = mpsc::channel(1);
        let (exit, exited) = watch::channel(None);
        let output = Arc::new(Mutex::new(VecDeque::new()));
        tokio::spawn(reap(leader, kills, exit));
        let reading = tokio::spawn(read(stdout, output.clone(), scan.clone(), profile.clone()));
        tokio::spawn(ended(reading, stderr.clone(), scan));
        let login = Self {
            stdin: tokio::sync::Mutex::new(stdin),
            kill,
            exited,
            output,
            stderr,
            _guard: guard,
        };
        let settled = async {
            match scanned
                .wait_for(|scan| !matches!(scan, Scan::Searching))
                .await
            {
                Ok(scan) => scan.clone(),
                Err(_) => Scan::Ended,
            }
        };
        let waited = tokio::select! {
            scan = settled => scan,
            () = sleep(config.sign_in.link_wait) => Scan::Searching,
            () = stop.cancelled() => {
                login.stop().await;
                return Err(LoginError::Cancelled);
            }
        };
        let failure = match waited {
            Scan::Found(url) => return Ok((login, url)),
            Scan::Ended => LoginError::NoLink {
                detail: login.detail().await,
                drift: drift(config, profile).await,
            },
            Scan::Searching => LoginError::LinkTimeout {
                secs: config.sign_in.link_wait.as_secs(),
                detail: login.detail().await,
                drift: drift(config, profile).await,
            },
        };
        login.stop().await;
        Err(failure)
    }

    pub(crate) async fn send_code(&self, code: &str) -> io::Result<()> {
        let mut stdin = self.stdin.lock().await;
        stdin.write_all(code.as_bytes()).await?;
        stdin.write_all(b"\n").await?;
        stdin.flush().await
    }

    pub(crate) async fn exited(&self) -> Exit {
        let mut exited = self.exited.clone();
        match exited.wait_for(Option::is_some).await {
            Ok(exit) => exit.unwrap_or(Exit::Failed),
            Err(_) => Exit::Failed,
        }
    }

    pub(crate) async fn stop(&self) {
        let _ = self.kill.try_send(());
        if timeout(STOP_WAIT, self.exited()).await.is_err() {
            tracing::warn!("the Claude Code sign-in did not exit after it was killed");
        }
    }

    pub(crate) async fn failure(&self) -> String {
        let detail = self.detail().await;
        if detail.is_empty() {
            "the sign-in command failed".to_owned()
        } else {
            format!("the sign-in command failed: {detail}")
        }
    }

    async fn detail(&self) -> String {
        let _ = timeout(Duration::from_secs(1), self.stderr.closed()).await;
        let stdout = {
            let tail = lock(&self.output);
            let (front, back) = tail.as_slices();
            let mut bytes = front.to_vec();
            bytes.extend_from_slice(back);
            String::from_utf8_lossy(&bytes).into_owned()
        };
        one_line(&format!("{stdout} {}", self.stderr.text()))
    }
}

fn guard(scratch: &Path) -> io::Result<TempDir> {
    let dir = tempfile::Builder::new()
        .prefix("riggs-sign-in-")
        .tempdir_in(scratch)?;
    for name in LAUNCHERS {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o700)
            .open(dir.path().join(name))?;
        file.write_all(LAUNCHER_SCRIPT)?;
    }
    Ok(dir)
}

async fn reap(
    mut leader: Leader,
    mut kills: mpsc::Receiver<()>,
    exit: watch::Sender<Option<Exit>>,
) {
    let natural = tokio::select! {
        status = leader.wait() => Some(status),
        _ = kills.recv() => None,
    };
    let _ = leader.kill_tree(Descendants::default()).await;
    let outcome = match natural {
        Some(Ok(status)) if status.success() => Exit::Succeeded,
        _ => Exit::Failed,
    };
    exit.send_replace(Some(outcome));
}

/// Reads one stream a line at a time, looking for the link a profile describes. It keeps its own
/// line buffer, so one of these belongs to each stream rather than to the sign-in.
struct Finder {
    profile: Profile,
    line: Vec<u8>,
    searching: bool,
}

impl Finder {
    fn new(profile: Profile) -> Self {
        Self {
            profile,
            line: Vec::new(),
            searching: true,
        }
    }

    /// The link, the first time a finished line holds one. Later chunks are ignored, because a
    /// sign-in is offered once.
    fn feed(&mut self, chunk: &[u8]) -> Option<String> {
        if !self.searching {
            return None;
        }
        for byte in chunk {
            if *byte != b'\n' {
                if self.line.len() < OUTPUT_BYTES {
                    self.line.push(*byte);
                }
                continue;
            }
            if let Some(url) = self.link() {
                return Some(url);
            }
            self.line.clear();
        }
        None
    }

    /// The link on a last line the stream ended without terminating.
    fn flush(&mut self) -> Option<String> {
        self.searching.then(|| self.link()).flatten()
    }

    fn link(&mut self) -> Option<String> {
        let url = self.profile.link_in(&String::from_utf8_lossy(&self.line))?;
        self.searching = false;
        Some(url)
    }
}

async fn read(
    mut stdout: ChildStdout,
    output: Arc<Mutex<VecDeque<u8>>>,
    scan: watch::Sender<Scan>,
    profile: Profile,
) {
    let mut finder = Finder::new(profile);
    let mut buffer = vec![0u8; 8192];
    while let Ok(read) = stdout.read(&mut buffer).await {
        if read == 0 {
            break;
        }
        let chunk = buffer.get(..read).unwrap_or_default();
        {
            let mut tail = lock(&output);
            tail.extend(chunk);
            let excess = tail.len().saturating_sub(OUTPUT_BYTES);
            tail.drain(..excess);
        }
        if let Some(url) = finder.feed(chunk) {
            scan.send_replace(Scan::Found(url));
        }
    }
    if let Some(url) = finder.flush() {
        scan.send_replace(Scan::Found(url));
    }
}

/// Both streams are spent and neither offered a link, so waiting out the rest of the timeout
/// would only delay the same answer.
async fn ended(reading: JoinHandle<()>, stderr: Tail, scan: watch::Sender<Scan>) {
    let _ = reading.await;
    stderr.closed().await;
    scan.send_if_modified(|scan| {
        let searching = matches!(scan, Scan::Searching);
        if searching {
            *scan = Scan::Ended;
        }
        searching
    });
}

fn one_line(text: &str) -> String {
    let joined = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if joined.chars().count() <= DETAIL_CHARS {
        return joined;
    }
    let mut clipped: String = joined.chars().take(DETAIL_CHARS - 1).collect();
    clipped.push('…');
    clipped
}

/// A CLI that has moved on from the release a profile was read against explains a flow that
/// suddenly prints nothing we recognise.
async fn drift(config: &ClaudeCodeConfig, profile: &Profile) -> String {
    let Some(verified) = profile.verified_version else {
        return String::new();
    };
    let mut command = Command::new(&profile.command);
    command
        .arg("--version")
        .current_dir(&config.workdir)
        .env_remove("CLAUDECODE")
        .envs(&config.env)
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true);
    let Ok(Ok(output)) = timeout(VERSION_TIMEOUT, command.output()).await else {
        return String::new();
    };
    let text = String::from_utf8_lossy(&output.stdout);
    match text.split_whitespace().next() {
        Some(version) if output.status.success() && version != verified => format!(
            " ({} reports version {version}, but this sign-in was verified against {verified}; its output may have changed)",
            profile.command.display()
        ),
        _ => String::new(),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use std::path::PathBuf;
    use std::time::Instant;

    use super::*;
    use crate::profile::{CLAUDE_CODE, GCLOUD};

    /// What `gcloud auth login --no-launch-browser` prints: the link on stderr, and on stdout
    /// only the prompt that follows it, which never ends in a newline.
    const GCLOUD_OUTPUT: &str = "printf 'Go to the following link in your browser:\\n\\n    https://accounts.google.com/o/oauth2/auth?client_id=32555940559\\n\\n' >&2;\
         printf 'Once finished, enter the verification code provided in your browser: ';\
         read code";
    const CLAUDE_OUTPUT: &str =
        "printf 'visit: https://claude.com/cai/oauth/authorize?code=true\\n'; read code";

    fn config() -> ClaudeCodeConfig {
        ClaudeCodeConfig::new(std::env::temp_dir())
    }

    /// A built-in's own link rules, over output a test can produce without the real CLI.
    fn shell(name: &str, script: &str) -> Profile {
        Profile {
            command: PathBuf::from("/bin/sh"),
            args: vec!["-c".to_owned(), script.to_owned()],
            ..Profile::builtin(name, &config()).unwrap()
        }
    }

    #[tokio::test]
    async fn a_link_offered_on_stderr_is_found() {
        let (login, url) = Login::start(
            &config(),
            &shell(GCLOUD, GCLOUD_OUTPUT),
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(
            url,
            "https://accounts.google.com/o/oauth2/auth?client_id=32555940559"
        );
        login.stop().await;
    }

    #[tokio::test]
    async fn a_link_offered_on_stdout_is_still_found() {
        let (login, url) = Login::start(
            &config(),
            &shell(CLAUDE_CODE, CLAUDE_OUTPUT),
            &CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(url, "https://claude.com/cai/oauth/authorize?code=true");
        login.stop().await;
    }

    /// Both streams are spent, so the wait ends there rather than at `link_wait`.
    #[tokio::test]
    async fn a_flow_that_offers_nothing_on_either_stream_ends_without_waiting() {
        let started = Instant::now();
        let failure = Login::start(
            &config(),
            &shell(GCLOUD, "echo out; echo err >&2"),
            &CancellationToken::new(),
        )
        .await
        .err()
        .unwrap();
        assert!(matches!(failure, LoginError::NoLink { .. }), "{failure}");
        assert!(failure.to_string().contains("out err"), "{failure}");
        assert!(started.elapsed() < config().sign_in.link_wait);
    }

    #[test]
    fn output_is_quoted_on_one_clipped_line() {
        assert_eq!(
            one_line("  Login failed:\n  bad\tcode \n"),
            "Login failed: bad code"
        );
        let long = "x ".repeat(500);
        let clipped = one_line(&long);
        assert_eq!(clipped.chars().count(), DETAIL_CHARS);
        assert!(clipped.ends_with('…'));
    }
}
