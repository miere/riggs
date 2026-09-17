use std::io;

use rax::id::PromptId;
use rax::interaction::{DisplayOutcome, SignInRequest, SignInSettled, SignInState};
use riggs_node::{HostHandles, SignInPrompt, SignInRefused};
use tokio::sync::watch;
use tokio::time::sleep;
use tokio_util::sync::CancellationToken;

use crate::config::ClaudeCodeConfig;
use crate::interaction::prompt_id;
use crate::login::{Exit, Login, LoginError};

const TOOL: &str = "Claude Code";
const EXPIRED: &str = "the sign-in expired before it was completed";
const LOST_ACCESS: &str = "the owner of this machine lost access before the sign-in finished";
const UNCONFIRMED: &str = "nobody confirmed the owner could still use the gateway";

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Phase {
    Starting,
    Shown,
    Done,
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum SignInError {
    #[error(transparent)]
    Login(#[from] LoginError),
    #[error("the sign-in could not be shown to the owner of this machine, so it was stopped: {0}")]
    NotShown(#[source] SignInRefused),
    #[error("the owner of this machine did not complete the sign-in ({0:?})")]
    Stopped(DisplayOutcome),
    #[error("could not hand the verification code to the sign-in command: {0}")]
    Code(#[source] io::Error),
    #[error("{0}")]
    Failed(String),
    #[error("{EXPIRED}")]
    Expired,
    #[error("{LOST_ACCESS}")]
    LostAccess,
    #[error("the sign-in finished, but {UNCONFIRMED}")]
    Unconfirmed,
    #[error("the sign-in was cancelled")]
    Cancelled,
}

struct Settler<'a> {
    host: &'a HostHandles,
    id: PromptId,
}

impl Settler<'_> {
    async fn settle(&self, state: SignInState, reason: Option<&str>) {
        let settled = SignInSettled {
            id: self.id.clone(),
            state,
            reason: reason.map(str::to_owned),
            url: None,
        };
        self.host.sign_ins.settle(settled).await;
    }
}

pub(crate) async fn run(
    config: &ClaudeCodeConfig,
    host: &HostHandles,
    stop: &CancellationToken,
    phase: &watch::Sender<Phase>,
) -> Result<(), SignInError> {
    let (login, url) = Login::start(config, stop).await?;
    let request = SignInRequest {
        id: prompt_id(),
        tool: TOOL.to_owned(),
        url: Some(url),
        needs_code: true,
        command: None,
    };
    let prompt = match host.sign_ins.raise(request).await {
        Ok(prompt) => prompt,
        Err(refused) => {
            login.stop().await;
            return Err(SignInError::NotShown(refused));
        }
    };
    phase.send_replace(Phase::Shown);
    let settler = Settler {
        host,
        id: prompt.id.clone(),
    };
    let finished = drive(config, &login, prompt, &settler, stop).await;
    login.stop().await;
    finished
}

async fn drive(
    config: &ClaudeCodeConfig,
    login: &Login,
    mut prompt: SignInPrompt,
    settler: &Settler<'_>,
    stop: &CancellationToken,
) -> Result<(), SignInError> {
    let expiry = sleep(config.sign_in.expiry);
    tokio::pin!(expiry);
    loop {
        tokio::select! {
            () = stop.cancelled() => {
                settler.settle(SignInState::Cancelled, None).await;
                return Err(SignInError::Cancelled);
            }
            answer = prompt.answers.recv() => {
                let outcome = answer.as_ref().map_or(DisplayOutcome::Dismissed, |answer| answer.outcome);
                match (outcome, answer.and_then(|answer| answer.code)) {
                    (DisplayOutcome::Answered, Some(code)) if !code.trim().is_empty() => {
                        if let Err(err) = login.send_code(code.trim()).await {
                            let error = SignInError::Code(err);
                            settler.settle(SignInState::Failed, Some(&error.to_string())).await;
                            return Err(error);
                        }
                        settler.settle(SignInState::Working, None).await;
                    }
                    (DisplayOutcome::Answered | DisplayOutcome::Approved, _) => {}
                    (outcome, _) => {
                        settler.settle(SignInState::Cancelled, None).await;
                        return Err(SignInError::Stopped(outcome));
                    }
                }
            }
            exit = login.exited() => {
                return match exit {
                    Exit::Succeeded => confirm(config, &mut prompt, settler, stop).await,
                    Exit::Failed => {
                        let detail = login.failure().await;
                        settler.settle(SignInState::Failed, Some(&detail)).await;
                        Err(SignInError::Failed(detail))
                    }
                };
            }
            () = &mut expiry => {
                settler.settle(SignInState::TimedOut, Some(EXPIRED)).await;
                return Err(SignInError::Expired);
            }
        }
    }
}

async fn confirm(
    config: &ClaudeCodeConfig,
    prompt: &mut SignInPrompt,
    settler: &Settler<'_>,
    stop: &CancellationToken,
) -> Result<(), SignInError> {
    settler.settle(SignInState::Confirming, None).await;
    let wait = sleep(config.sign_in.confirm_wait);
    tokio::pin!(wait);
    loop {
        tokio::select! {
            () = stop.cancelled() => {
                settler.settle(SignInState::Cancelled, None).await;
                return Err(SignInError::Cancelled);
            }
            answer = prompt.answers.recv() => match answer.map(|answer| answer.outcome) {
                Some(DisplayOutcome::Approved) => {
                    settler.settle(SignInState::Success, None).await;
                    return Ok(());
                }
                Some(DisplayOutcome::Answered) => {}
                _ => {
                    settler.settle(SignInState::Failed, Some(LOST_ACCESS)).await;
                    return Err(SignInError::LostAccess);
                }
            },
            () = &mut wait => {
                settler.settle(SignInState::Failed, Some(UNCONFIRMED)).await;
                return Err(SignInError::Unconfirmed);
            }
        }
    }
}
