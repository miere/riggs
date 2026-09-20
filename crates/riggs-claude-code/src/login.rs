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
        let (
            leader,
            Pipes {
                stdin,
                stdout,
                stderr,
            },
        ) = Leader::spawn(&mut command, OUTPUT_BYTES).map_err(LoginError::Spawn)?;
        tracing::debug!(pid = leader.pid(), "Claude Code sign-in started");
        let (kill, kills) = mpsc::channel(1);
        let (exit, exited) = watch::channel(None);
        let (scan, mut scanned) = watch::channel(Scan::Searching);
        let output = Arc::new(Mutex::new(VecDeque::new()));
        tokio::spawn(reap(leader, kills, exit));
        tokio::spawn(read(stdout, output.clone(), scan, profile.clone()));
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

async fn read(
    mut stdout: ChildStdout,
    output: Arc<Mutex<VecDeque<u8>>>,
    scan: watch::Sender<Scan>,
    profile: Profile,
) {
    let mut buffer = vec![0u8; 8192];
    let mut line = Vec::new();
    let mut searching = true;
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
        if !searching {
            continue;
        }
        for byte in chunk {
            if *byte != b'\n' {
                if line.len() < OUTPUT_BYTES {
                    line.push(*byte);
                }
                continue;
            }
            if let Some(url) = profile.link_in(&String::from_utf8_lossy(&line)) {
                scan.send_replace(Scan::Found(url));
                searching = false;
                break;
            }
            line.clear();
        }
    }
    if searching {
        if let Some(url) = profile.link_in(&String::from_utf8_lossy(&line)) {
            scan.send_replace(Scan::Found(url));
        } else {
            scan.send_replace(Scan::Ended);
        }
    }
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
    use super::*;

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
