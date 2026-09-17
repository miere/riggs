use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use rax::ErrorKind;
use rax::credential::CredentialRenewal;
use tokio::sync::watch;
use tokio::time::{Instant, timeout};
use tokio_util::sync::CancellationToken;

use crate::process::Ctx;
use crate::sign_in::{self, Phase};

const NOT_SHOWN: &str = "the Claude Code credential on this machine was rejected, and no sign-in could be put in front of its owner";
const REJECTED: &str = "the agent's credential was rejected";

#[derive(Clone)]
struct Run {
    serial: u64,
    stop: CancellationToken,
    phase: watch::Receiver<Phase>,
}

impl Run {
    async fn done(&self) {
        let mut phase = self.phase.clone();
        let _ = phase.wait_for(|phase| *phase == Phase::Done).await;
    }

    async fn shown_within(&self, wait: std::time::Duration) -> bool {
        let mut phase = self.phase.clone();
        let settled = timeout(wait, phase.wait_for(|phase| *phase != Phase::Starting)).await;
        matches!(settled, Ok(Ok(phase)) if *phase == Phase::Shown)
    }
}

#[derive(Default)]
struct State {
    running: Option<Run>,
    failed_at: Option<Instant>,
    serial: u64,
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

    pub(crate) async fn renew(self: Arc<Self>) -> CredentialRenewal {
        if self.ctx.host.get().is_none() {
            return CredentialRenewal::NothingToRenew;
        }
        let previous = lock(&self.state).running.clone();
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
        if state.running.is_some() {
            return CredentialRenewal::AlreadyRunning;
        }
        self.start(&mut state);
        CredentialRenewal::Started
    }

    pub(crate) async fn stop(&self) {
        let running = lock(&self.state).running.clone();
        if let Some(run) = running {
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
        if let Some(run) = &state.running {
            return Some(run.clone());
        }
        let cooling = state
            .failed_at
            .is_some_and(|failed| failed.elapsed() < self.ctx.config.sign_in.cooldown);
        if cooling {
            return None;
        }
        Some(self.start(&mut state))
    }

    fn start(self: &Arc<Self>, state: &mut State) -> Run {
        state.serial += 1;
        let (phase, watching) = watch::channel(Phase::Starting);
        let run = Run {
            serial: state.serial,
            stop: CancellationToken::new(),
            phase: watching,
        };
        state.running = Some(run.clone());
        state.failed_at = None;
        tracing::warn!("asking this node's owner to sign Claude Code in again");
        let repair = self.clone();
        let mine = run.clone();
        tokio::spawn(async move {
            let ctx = &repair.ctx;
            let outcome = match ctx.host.get() {
                Some(host) => sign_in::run(&ctx.config, host, &mine.stop, &phase).await,
                None => Err(sign_in::SignInError::Cancelled),
            };
            {
                let mut state = lock(&repair.state);
                if state
                    .running
                    .as_ref()
                    .is_some_and(|run| run.serial == mine.serial)
                {
                    state.running = None;
                    if outcome.is_err() && !mine.stop.is_cancelled() {
                        state.failed_at = Some(Instant::now());
                    }
                }
            }
            match outcome {
                Ok(()) => {
                    tracing::info!("the Claude Code sign-in succeeded");
                    ctx.health.recovered();
                }
                Err(err) => {
                    tracing::warn!(error = %err, "the Claude Code sign-in did not complete");
                }
            }
            phase.send_replace(Phase::Done);
        });
        run
    }
}
