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

const LOGIN_ARGS: &[&str] = &["auth", "login", "--claudeai"];
const VERIFIED_VERSION: &str = "2.1.271";
const LAUNCHERS: &[&str] = &["open", "xdg-open"];
const LAUNCHER_SCRIPT: &[u8] = b"#!/bin/sh\nexit 0\n";
const OUTPUT_BYTES: usize = 16 << 10;
const DETAIL_CHARS: usize = 400;
const VERSION_TIMEOUT: Duration = Duration::from_secs(5);
const STOP_WAIT: Duration = Duration::from_secs(5);
const TIGHT: &str = "https://claude.com/";
const CONSENT_PATH: &str = "oauth/authorize";

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
    pub(crate) async fn start(
        config: &ClaudeCodeConfig,
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
        let mut command = Command::new(&config.command);
        command
            .args(LOGIN_ARGS)
            .current_dir(&config.workdir)
            .env_remove("CLAUDECODE")
            .envs(&config.env)
            .env("PATH", path)
            .env("BROWSER", guard.path().join("open"));
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
        tokio::spawn(read(stdout, output.clone(), scan));
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
                drift: drift(config).await,
            },
            Scan::Searching => LoginError::LinkTimeout {
                secs: config.sign_in.link_wait.as_secs(),
                detail: login.detail().await,
                drift: drift(config).await,
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
            if let Some(url) = consent_url(&String::from_utf8_lossy(&line)) {
                scan.send_replace(Scan::Found(url));
                searching = false;
                break;
            }
            line.clear();
        }
    }
    if searching {
        if let Some(url) = consent_url(&String::from_utf8_lossy(&line)) {
            scan.send_replace(Scan::Found(url));
        } else {
            scan.send_replace(Scan::Ended);
        }
    }
}

pub(crate) fn consent_url(line: &str) -> Option<String> {
    let candidates: Vec<&str> = line
        .match_indices("https://")
        .filter_map(|(at, _)| line.get(at..))
        .map(|rest| rest.split(char::is_whitespace).next().unwrap_or_default())
        .collect();
    let consent = |candidate: &&str| {
        candidate
            .find(CONSENT_PATH)
            .is_some_and(|at| candidate.len() > at + CONSENT_PATH.len())
    };
    let tight = candidates
        .iter()
        .copied()
        .filter(|candidate| candidate.starts_with(TIGHT))
        .find(consent);
    let found = tight.or_else(|| candidates.iter().copied().find(consent))?;
    let trimmed = found.trim_end_matches(['.', ',', ';', ':', '\'', '"', ')', ']', '>']);
    trimmed
        .find(CONSENT_PATH)
        .is_some_and(|at| trimmed.len() > at + CONSENT_PATH.len())
        .then(|| trimmed.to_owned())
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

async fn drift(config: &ClaudeCodeConfig) -> String {
    let mut command = Command::new(&config.command);
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
        Some(version) if output.status.success() && version != VERIFIED_VERSION => format!(
            " (Claude Code reports version {version}, but this sign-in was verified against {VERIFIED_VERSION}; its output may have changed)"
        ),
        _ => String::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const REAL_LINE: &str = "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&code_challenge=mZioQLsnrQWX7owYtUe_kW-JQJc1iItirISM4gUlCpo&code_challenge_method=S256&state=y_pucGhaVzjF97wrrPRD5WeTCoR6vFh6jKV3LFJIRiA";

    #[test]
    fn the_consent_link_is_taken_from_the_real_login_line() {
        let url = REAL_LINE.split_once("visit: ").map(|(_, url)| url);
        assert_eq!(consent_url(REAL_LINE).as_deref(), url);
    }

    #[test]
    fn a_moved_consent_page_still_matches_and_trailing_punctuation_is_trimmed() {
        assert_eq!(
            consent_url(
                "visit: https://auth.anthropic.example/cai/oauth/authorize?code=true&state=abc."
            )
            .as_deref(),
            Some("https://auth.anthropic.example/cai/oauth/authorize?code=true&state=abc")
        );
        assert_eq!(
            consent_url(
                "(https://auth.example/oauth/authorize?x=1) then https://claude.com/cai/oauth/authorize?y=2"
            )
            .as_deref(),
            Some("https://claude.com/cai/oauth/authorize?y=2")
        );
    }

    #[test]
    fn links_that_are_not_a_consent_page_never_match() {
        for line in [
            "redirect_uri is https://platform.claude.com/oauth/code/callback for this flow",
            "See https://code.claude.com/docs/en/overview for help",
            "report the issue at https://github.com/anthropics/claude-code/issues",
            "https://accounts.google.com/o/oauth2/auth?client_id=x",
            "posting to https://api.example.com/v1/oauth/token now",
            "Opening browser to sign in…",
            "bare https://claude.com/cai/oauth/authorize",
            "Paste code here if prompted > ",
        ] {
            assert_eq!(consent_url(line), None, "{line}");
        }
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
