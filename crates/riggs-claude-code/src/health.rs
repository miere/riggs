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

/// What `claude auth status` says about the credential Claude Code would use.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct Status {
    pub(crate) logged_in: bool,
    /// Absent from older binaries, which then read as a credential that may not be shared.
    #[serde(default)]
    auth_method: Option<String>,
}

/// The credentials Anthropic lets their holder share with the people they authorise: an API key,
/// and a cloud provider's. Anything else, a Claude subscription above all, serves its owner alone.
const SHAREABLE: &[&str] = &["api_key", "third_party"];

impl Status {
    /// Only a credential named as shareable is: a renamed field, a new method or a value never
    /// seen before all confine the node to its owner, so a change in Claude Code can lock people
    /// out but never open a subscription to them.
    pub(crate) fn shareable(&self) -> bool {
        self.logged_in
            && self
                .auth_method
                .as_deref()
                .is_some_and(|method| SHAREABLE.contains(&method))
    }
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

pub(crate) async fn probe(config: &ClaudeCodeConfig) -> Result<Status, ProbeError> {
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
        Ok(_) => serde_json::from_slice::<Status>(&output).map_err(|_| ProbeError::Unreadable),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use serde_json::json;

    use super::Status;

    fn shareable(printed: serde_json::Value) -> bool {
        serde_json::from_value::<Status>(printed)
            .unwrap()
            .shareable()
    }

    /// The shapes `claude auth status` printed in 2.1.296 for each credential.
    #[test]
    fn only_an_api_key_or_a_cloud_provider_may_be_shared() {
        let subscription = json!({"loggedIn": true, "authMethod": "claude.ai",
            "apiProvider": "firstParty", "subscriptionType": "max"});
        assert!(!shareable(subscription));
        let setup_token =
            json!({"loggedIn": true, "authMethod": "oauth_token", "apiProvider": "firstParty"});
        assert!(!shareable(setup_token));
        let api_key =
            json!({"loggedIn": true, "authMethod": "api_key", "apiProvider": "firstParty"});
        assert!(shareable(api_key));
        let bedrock =
            json!({"loggedIn": true, "authMethod": "third_party", "apiProvider": "bedrock"});
        assert!(shareable(bedrock));
    }

    /// Asks a real `claude`, named by `RIGGS_REAL_CLAUDE`, so a release that renames what
    /// `auth status` prints fails here instead of quietly confining every node to its owner. None
    /// of the credentials is real: `auth status` reports which one it would use without calling
    /// Anthropic.
    #[tokio::test]
    #[ignore = "needs a real claude binary in RIGGS_REAL_CLAUDE"]
    async fn a_real_claude_names_its_credential_as_riggs_expects() {
        let command =
            std::env::var("RIGGS_REAL_CLAUDE").expect("RIGGS_REAL_CLAUDE names a claude binary");
        let home = tempfile::tempdir().unwrap();
        let cases: &[(&[(&str, &str)], bool)] = &[
            (&[("ANTHROPIC_API_KEY", "sk-ant-riggs-contract")], true),
            (&[("CLAUDE_CODE_USE_BEDROCK", "1")], true),
            (&[("CLAUDE_CODE_USE_VERTEX", "1")], true),
            (&[("CLAUDE_CODE_OAUTH_TOKEN", "riggs-contract")], false),
            (&[], false),
        ];
        for (env, expected) in cases {
            let mut config = crate::config::ClaudeCodeConfig::new(home.path());
            config.command = command.clone().into();
            config.env.insert(
                "CLAUDE_CONFIG_DIR".into(),
                home.path().display().to_string(),
            );
            for (name, value) in *env {
                config.env.insert((*name).into(), (*value).into());
            }
            let status = super::probe(&config).await.unwrap();
            assert_eq!(status.shareable(), *expected, "{env:?} printed {status:?}");
        }
    }

    #[test]
    fn anything_unrecognised_serves_the_owner_alone() {
        assert!(!shareable(json!({"loggedIn": true})));
        assert!(!shareable(
            json!({"loggedIn": true, "authMethod": "api-key"})
        ));
        assert!(!shareable(
            json!({"loggedIn": false, "authMethod": "api_key"})
        ));
    }
}
