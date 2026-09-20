use std::collections::HashMap;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use rax::ErrorKind;
use rax::credential::CredentialRenewal;
use tokio::sync::watch;
use tokio::time::{Instant, timeout};
use tokio_util::sync::CancellationToken;

use crate::process::Ctx;
use crate::sign_in::{self, Ask, Phase};

const NOT_SHOWN: &str = "the Claude Code credential on this machine was rejected, and no sign-in could be put in front of its owner";
const REJECTED: &str = "the agent's credential was rejected";

#[derive(Clone)]
struct Run {
    serial: u64,
    stop: CancellationToken,
    phase: watch::Receiver<Phase>,
    /// The outcome once it finishes, so a caller waiting on this sign-in hears how it went.
    outcome: watch::Receiver<Option<Result<(), String>>>,
}

impl Run {
    async fn done(&self) {
        let mut phase = self.phase.clone();
        let _ = phase.wait_for(|phase| *phase == Phase::Done).await;
    }

    /// Waits for this sign-in to finish and reports how it ended.
    async fn finished(&self) -> Result<(), String> {
        let mut outcome = self.outcome.clone();
        match outcome.wait_for(Option::is_some).await {
            Ok(settled) => settled.clone().unwrap_or(Ok(())),
            Err(_) => Err("the sign-in stopped without an answer".to_owned()),
        }
    }

    async fn shown_within(&self, wait: std::time::Duration) -> bool {
        let mut phase = self.phase.clone();
        let settled = timeout(wait, phase.wait_for(|phase| *phase != Phase::Starting)).await;
        matches!(settled, Ok(Ok(phase)) if *phase == Phase::Shown)
    }
}

/// Each credential signs in on its own: Claude Code's flow and a gcloud one neither wait for
/// nor cancel each other.
#[derive(Default)]
struct Credential {
    running: Option<Run>,
    failed_at: Option<Instant>,
}

#[derive(Default)]
struct State {
    credentials: HashMap<String, Credential>,
    serial: u64,
}

impl State {
    fn credential(&mut self, profile: &str) -> &mut Credential {
        self.credentials.entry(profile.to_owned()).or_default()
    }
}

pub(crate) struct Repair {
    ctx: Arc<Ctx>,
    state: Mutex<State>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

impl Repair {
    pub(crate) fn new(ctx: Arc<Ctx>) -> Self {
        Self {
            ctx,
            state: Mutex::new(State::default()),
        }
    }

    pub(crate) async fn failed(self: Arc<Self>, error: rax::Error) -> rax::Error {
        if error.kind != ErrorKind::Credential {
            return error;
        }
        let shown = match self.current() {
            Some(run) => run.shown_within(self.ctx.config.sign_in.show_wait).await,
            None => false,
        };
        if shown {
            rax::Error::new(
                ErrorKind::Credential,
                format!("{REJECTED}: {}", error.message),
            )
        } else {
            rax::Error::new(
                ErrorKind::Unknown,
                format!("{NOT_SHOWN}: {}", error.message),
            )
        }
    }

    /// Runs a sign-in the agent asked for, waiting for it to finish. A sign-in already running
    /// for the same credential is joined rather than duplicated.
    pub(crate) async fn sign_in(self: Arc<Self>, ask: Ask) -> Result<(), String> {
        if self.ctx.host.get().is_none() {
            return Err("this machine is not attached to a gateway, so nobody can be asked".into());
        }
        let run = {
            let mut state = lock(&self.state);
            match state.credential(&ask.profile.name).running.clone() {
                Some(run) => run,
                None => self.start(&mut state, ask),
            }
        };
        run.finished().await
    }

    pub(crate) async fn renew(self: Arc<Self>) -> CredentialRenewal {
        if self.ctx.host.get().is_none() {
            return CredentialRenewal::NothingToRenew;
        }
        let profile = crate::profile::CLAUDE_CODE.to_owned();
        let previous = lock(&self.state).credential(&profile).running.clone();
        if let Some(previous) = previous {
            previous.stop.cancel();
            if timeout(self.ctx.config.sign_in.drain, previous.done())
                .await
                .is_err()
            {
                tracing::warn!(
                    "an earlier Claude Code sign-in would not stop; refusing to start a second"
                );
                return CredentialRenewal::AlreadyRunning;
            }
        }
        let mut state = lock(&self.state);
        if state.credential(&profile).running.is_some() {
            return CredentialRenewal::AlreadyRunning;
        }
        self.start(&mut state, Ask::claude_code(&self.ctx.config));
        CredentialRenewal::Started
    }

    pub(crate) async fn stop(&self) {
        let running: Vec<Run> = lock(&self.state)
            .credentials
            .values()
            .filter_map(|credential| credential.running.clone())
            .collect();
        for run in running {
            run.stop.cancel();
            let _ = timeout(self.ctx.config.sign_in.drain, run.done()).await;
        }
    }

    fn current(self: &Arc<Self>) -> Option<Run> {
        if self.ctx.host.get().is_none() {
            tracing::warn!(
                "a credential failure reached a Claude Code backend that was never started; the repair hook belongs to the backend the node serves"
            );
            return None;
        }
        let mut state = lock(&self.state);
        let profile = crate::profile::CLAUDE_CODE.to_owned();
        let credential = state.credential(&profile);
        if let Some(run) = &credential.running {
            return Some(run.clone());
        }
        let cooling = credential
            .failed_at
            .is_some_and(|failed| failed.elapsed() < self.ctx.config.sign_in.cooldown);
        if cooling {
            return None;
        }
        Some(self.start(&mut state, Ask::claude_code(&self.ctx.config)))
    }

    fn start(self: &Arc<Self>, state: &mut State, ask: Ask) -> Run {
        state.serial += 1;
        let (phase, watching) = watch::channel(Phase::Starting);
        let (settled, outcome) = watch::channel(None);
        let run = Run {
            serial: state.serial,
            stop: CancellationToken::new(),
            phase: watching,
            outcome,
        };
        let name = ask.profile.name.clone();
        let credential = state.credential(&name);
        credential.running = Some(run.clone());
        credential.failed_at = None;
        tracing::warn!(profile = %name, tool = %ask.tool, "asking this node's owner to sign in");
        let repair = self.clone();
        let mine = run.clone();
        tokio::spawn(async move {
            let ctx = &repair.ctx;
            let outcome = match ctx.host.get() {
                Some(host) => sign_in::run(&ctx.config, host, &ask, &mine.stop, &phase).await,
                None => Err(sign_in::SignInError::Cancelled),
            };
            {
                let mut state = lock(&repair.state);
                let credential = state.credential(&name);
                if credential
                    .running
                    .as_ref()
                    .is_some_and(|run| run.serial == mine.serial)
                {
                    credential.running = None;
                    if outcome.is_err() && !mine.stop.is_cancelled() {
                        credential.failed_at = Some(Instant::now());
                    }
                }
            }
            let result = match outcome {
                Ok(()) => {
                    tracing::info!(profile = %name, "the sign-in succeeded");
                    if name == crate::profile::CLAUDE_CODE {
                        ctx.health.recovered();
                    }
                    Ok(())
                }
                Err(err) => {
                    tracing::warn!(profile = %name, error = %err, "the sign-in did not complete");
                    Err(err.to_string())
                }
            };
            settled.send_replace(Some(result));
            phase.send_replace(Phase::Done);
        });
        run
    }
}
