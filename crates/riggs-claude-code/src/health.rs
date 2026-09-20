use std::io;
use std::sync::{Mutex, MutexGuard, PoisonError};

use rax::credential::CredentialHealth;
use riggs_node::CredentialReporter;
use riggs_process::{Descendants, Leader, Pipes};
use serde::Deserialize;
use time::OffsetDateTime;
use tokio::io::AsyncReadExt;
use tokio::process::Command;
use tokio::sync::mpsc;
use tokio::time::timeout;

use crate::config::ClaudeCodeConfig;

const STATUS_ARGS: &[&str] = &["auth", "status", "--json"];
const STATUS_BYTES: u64 = 64 << 10;
const REASON_CHARS: usize = 300;
const NOT_SIGNED_IN: &str = "Claude Code is not signed in on this machine";

#[derive(Debug, thiserror::Error)]
pub(crate) enum ProbeError {
    #[error("could not run `claude auth status`: {0}")]
    Spawn(#[source] io::Error),
    #[error("`claude auth status` did not answer within {0} s")]
    TimedOut(u64),
    #[error("`claude auth status` did not say whether Claude Code is signed in")]
    Unreadable,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Status {
    logged_in: bool,
}

enum Observed {
    Unknown,
    Healthy,
    Degraded { since: OffsetDateTime },
}

/// The expiry last read out of the credential, carried on every report from then on.
#[derive(Default)]
struct Expiry(Option<OffsetDateTime>);

pub(crate) struct Health {
    credential: String,
    observed: Mutex<Observed>,
    expiry: Mutex<Expiry>,
    reports: mpsc::UnboundedSender<CredentialHealth>,
    queued: Mutex<Option<mpsc::UnboundedReceiver<CredentialHealth>>>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

impl Health {
    pub(crate) fn new(config: &ClaudeCodeConfig) -> Self {
        let command = config.command.display();
        let credential = match config.env.get("HOME") {
            Some(home) => format!("{command} (HOME={home})"),
            None => command.to_string(),
        };
        let (reports, queued) = mpsc::unbounded_channel();
        Self {
            credential,
            observed: Mutex::new(Observed::Unknown),
            expiry: Mutex::new(Expiry::default()),
            reports,
            queued: Mutex::new(Some(queued)),
        }
    }

    pub(crate) fn start(&self, reporter: CredentialReporter) {
        let Some(mut queued) = lock(&self.queued).take() else {
            return;
        };
        tokio::spawn(async move {
            while let Some(health) = queued.recv().await {
                reporter.report(health).await;
            }
        });
    }

    pub(crate) fn probed(&self, result: Result<bool, ProbeError>) {
        let mut observed = lock(&self.observed);
        if !matches!(*observed, Observed::Unknown) {
            return;
        }
        match result {
            Ok(true) => self.become_healthy(&mut observed),
            Ok(false) => self.become_degraded(&mut observed, NOT_SIGNED_IN.to_owned()),
            Err(err) => {
                tracing::warn!(error = %err, "could not check whether Claude Code is signed in");
                let reason = format!("could not check whether Claude Code is signed in: {err}");
                self.become_degraded(&mut observed, reason);
            }
        }
    }

    /// The warden read the credential; a later expiry than the one reported means it was
    /// refreshed, which is worth telling the owner about.
    pub(crate) fn expires_at(&self, expiry: OffsetDateTime) {
        let changed = {
            let mut last = lock(&self.expiry);
            let changed = last.0 != Some(expiry);
            last.0 = Some(expiry);
            changed
        };
        if !changed {
            return;
        }
        let mut observed = lock(&self.observed);
        if matches!(*observed, Observed::Healthy) {
            return;
        }
        self.become_healthy(&mut observed);
    }

    pub(crate) fn degraded(&self, error: &str) {
        let mut observed = lock(&self.observed);
        if matches!(*observed, Observed::Degraded { .. }) {
            return;
        }
        let reason = format!("Claude Code's credential was refused: {}", clip(error));
        self.become_degraded(&mut observed, reason);
    }

    pub(crate) fn recovered(&self) {
        let mut observed = lock(&self.observed);
        if !matches!(*observed, Observed::Healthy) {
            self.become_healthy(&mut observed);
        }
    }

    fn become_healthy(&self, observed: &mut Observed) {
        let since = match observed {
            Observed::Degraded { since } => Some(*since),
            _ => None,
        };
        *observed = Observed::Healthy;
        self.send(CredentialHealth {
            credential: self.credential.clone(),
            degraded: false,
            reason: None,
            since,
            expires_at: lock(&self.expiry).0,
        });
    }

    fn become_degraded(&self, observed: &mut Observed, reason: String) {
        let since = OffsetDateTime::now_utc();
        tracing::warn!(credential = %self.credential, %reason, "the Claude Code credential is not working");
        self.send(CredentialHealth {
            credential: self.credential.clone(),
            degraded: true,
            reason: Some(reason),
            since: Some(since),
            expires_at: lock(&self.expiry).0,
        });
        *observed = Observed::Degraded { since };
    }

    fn send(&self, health: CredentialHealth) {
        let _ = self.reports.send(health);
    }
}

fn clip(text: &str) -> String {
    let line = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if line.chars().count() <= REASON_CHARS {
        return line;
    }
    let mut clipped: String = line.chars().take(REASON_CHARS - 1).collect();
    clipped.push('…');
    clipped
}

pub(crate) async fn probe(config: &ClaudeCodeConfig) -> Result<bool, ProbeError> {
    let mut command = Command::new(&config.command);
    command
        .args(STATUS_ARGS)
        .current_dir(&config.workdir)
        .env_remove("CLAUDECODE")
        .envs(&config.env);
    let (mut leader, Pipes { stdin, stdout, .. }) =
        Leader::spawn(&mut command, 1024).map_err(ProbeError::Spawn)?;
    drop(stdin);
    let limit = config.sign_in.status_timeout;
    let mut output = Vec::new();
    let read = timeout(limit, stdout.take(STATUS_BYTES).read_to_end(&mut output)).await;
    let _ = leader.kill_tree(Descendants::default()).await;
    match read {
        Err(_) => Err(ProbeError::TimedOut(limit.as_secs())),
        Ok(_) => serde_json::from_slice::<Status>(&output)
            .map(|status| status.logged_in)
            .map_err(|_| ProbeError::Unreadable),
    }
}
