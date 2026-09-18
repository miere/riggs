use std::sync::Arc;

use rax::attachment::Attachment;
use rax::credential::CredentialHealth;
use rax::event::BackgroundEvent;
use rax::interaction::{DisplayOutcome, SignInRequest, SignInSettled, SignInState};
use rax::tool::Decision;
use rax::{NodeCall, ToolCall};
use rax_tokio::{CallError, SendError, TransferError};
use tokio::time::{Instant, timeout};

use crate::backend::{AttachmentSource, SessionKey};
use crate::state::{self, Epoch, PromptKind, PromptRoute, PromptStart, Shared, lock};
use crate::turn::{SignInPrompt, hold_background};

/// Everything a backend may reach with no turn open. It outlives every socket and link.
#[derive(Clone)]
pub struct HostHandles {
    pub gate: BackgroundGate,
    pub background: BackgroundSink,
    pub sign_ins: SignIns,
    pub credentials: CredentialReporter,
}

impl HostHandles {
    pub(crate) fn new(shared: Arc<Shared>) -> Self {
        Self {
            gate: BackgroundGate {
                shared: shared.clone(),
            },
            background: BackgroundSink {
                shared: shared.clone(),
            },
            sign_ins: SignIns {
                shared: shared.clone(),
            },
            credentials: CredentialReporter { shared },
        }
    }
}

#[derive(Clone)]
pub struct BackgroundGate {
    shared: Arc<Shared>,
}

impl BackgroundGate {
    /// Never allows a call just because no turn is open: with no link it denies `unavailable`.
    pub async fn hold_background(
        &self,
        session: &SessionKey,
        call: ToolCall,
        deadline: Instant,
    ) -> Decision {
        hold_background(&self.shared, session, call, deadline).await
    }
}

#[derive(Debug, thiserror::Error)]
pub enum NotDelivered {
    /// Not queued: notices about work that finished long ago are noise once a gateway returns.
    #[error("riggs-node: no gateway is attached, so the background event was dropped")]
    NoGateway,
    #[error("riggs-node: the link ended before the background event was sent")]
    LinkEnded,
    #[error(transparent)]
    Send(#[from] SendError),
    #[error(transparent)]
    Transfer(#[from] TransferError),
}

#[derive(Clone)]
pub struct BackgroundSink {
    shared: Arc<Shared>,
}

impl BackgroundSink {
    pub async fn send(
        &self,
        session: &SessionKey,
        event: BackgroundEvent,
    ) -> Result<(), NotDelivered> {
        let epoch = self.epoch(session)?;
        tokio::select! {
            sent = epoch.handle.background(session.session_id(), event) => Ok(sent?),
            () = epoch.ended.cancelled() => Err(NotDelivered::LinkEnded),
        }
    }

    pub async fn attach(
        &self,
        session: &SessionKey,
        source: AttachmentSource,
    ) -> Result<(), NotDelivered> {
        let epoch = self.epoch(session)?;
        let AttachmentSource { meta, size, reader } = source;
        let transfer_id = tokio::select! {
            sent = epoch.handle.send_attachment_from(reader, size) => sent?,
            () = epoch.ended.cancelled() => return Err(NotDelivered::LinkEnded),
        };
        let attachment = Attachment {
            transfer_id,
            size,
            filename: meta.filename,
            title: meta.title,
            comment: meta.comment,
            mimetype: meta.mimetype,
        };
        self.send(session, BackgroundEvent::Attachment { attachment })
            .await
    }

    fn epoch(&self, session: &SessionKey) -> Result<Epoch, NotDelivered> {
        self.shared.live().ok_or_else(|| {
            tracing::debug!(session_id = %session, "dropping a background event; no gateway is attached");
            NotDelivered::NoGateway
        })
    }
}

#[derive(Debug, thiserror::Error)]
pub enum SignInRefused {
    #[error("riggs-node: a sign-in with no conversation was refused; no gateway is attached")]
    NoGateway,
    #[error("riggs-node: the gateway cannot show a sign-in")]
    Unavailable,
    #[error("riggs-node: the gateway did not take a sign-in with no conversation: {0}")]
    NotShown(CallError),
    /// The node has already withdrawn it with `sign_in.settled{cancelled}`.
    #[error("riggs-node: the gateway did not answer the sign-in in time")]
    TimedOut,
    #[error("riggs-node: the link ended before the gateway answered the sign-in")]
    LinkEnded,
}

/// Sign-ins with no turn, such as a credential repair. Settles go one at a time, and only to the
/// link that showed the sign-in, because any later gateway never heard of it.
#[derive(Clone)]
pub struct SignIns {
    shared: Arc<Shared>,
}

impl SignIns {
    pub async fn raise(&self, request: SignInRequest) -> Result<SignInPrompt, SignInRefused> {
        let shared = &self.shared;
        let id = request.id.clone();
        let route = PromptRoute::Background;
        let (serial, epoch, answers) = match shared.open_prompt(
            &id,
            PromptKind::SignIn,
            route,
            state::open,
        ) {
            PromptStart::Registered {
                serial,
                epoch,
                answers,
            } => (serial, epoch, answers),
            PromptStart::Refused(DisplayOutcome::Unavailable) if shared.live().is_none() => {
                tracing::warn!(tool = %request.tool, "a sign-in with no conversation was refused; no gateway is attached");
                return Err(SignInRefused::NoGateway);
            }
            PromptStart::Refused(_) => return Err(SignInRefused::Unavailable),
        };
        let tool = request.tool.clone();
        let call = timeout(
            shared.call_timeout,
            epoch.handle.call(NodeCall::SignIn(request)),
        );
        let answered = tokio::select! {
            answered = call => answered,
            () = epoch.ended.cancelled() => {
                shared.forget_prompt(&id, Some(serial));
                return Err(SignInRefused::LinkEnded);
            }
        };
        match answered {
            Ok(Ok(_)) => Ok(SignInPrompt { id, answers }),
            Ok(Err(err)) => {
                shared.forget_prompt(&id, Some(serial));
                tracing::warn!(%tool, error = %err, "the gateway did not take a sign-in with no conversation");
                Err(SignInRefused::NotShown(err))
            }
            Err(_) => {
                shared.forget_prompt(&id, Some(serial));
                tracing::warn!(%tool, "the gateway did not answer a sign-in in time; withdrawing it");
                let Some(_in_order) = shared.in_order(&shared.settles).await else {
                    return Err(SignInRefused::TimedOut);
                };
                let withdraw = SignInSettled {
                    id,
                    state: SignInState::Cancelled,
                    reason: Some(
                        "this node stopped waiting for the sign-in to be shown".to_owned(),
                    ),
                    url: None,
                };
                push(shared, &epoch, NodeCall::SignInSettled(withdraw)).await;
                Err(SignInRefused::TimedOut)
            }
        }
    }

    pub async fn settle(&self, settled: SignInSettled) {
        let shared = &self.shared;
        let Some(_in_order) = shared.in_order(&shared.settles).await else {
            return;
        };
        let id = settled.id.clone();
        let terminal = settled.state.is_terminal();
        let raised = shared.prompt_epoch(&id, None);
        match shared.live() {
            Some(epoch) if raised == Some(epoch.id) => {
                push(shared, &epoch, NodeCall::SignInSettled(settled)).await;
            }
            _ => {
                tracing::debug!(prompt = %id, "dropping a sign-in update for a link that is gone");
            }
        }
        if terminal {
            shared.forget_prompt(&id, None);
        }
    }
}

/// Reports go one at a time, so a recovery never overtakes the failure it ends. The latest
/// report per credential is pushed again after every `initialize`.
#[derive(Clone)]
pub struct CredentialReporter {
    shared: Arc<Shared>,
}

impl CredentialReporter {
    pub async fn report(&self, health: CredentialHealth) {
        let shared = &self.shared;
        lock(&shared.health).insert(health.credential.clone(), health.clone());
        let Some(_in_order) = shared.in_order(&shared.health_sending).await else {
            return;
        };
        match shared.live() {
            Some(epoch) if epoch.caps.is_some() => {
                push(shared, &epoch, NodeCall::CredentialHealth(health)).await;
            }
            _ => {
                tracing::debug!(credential = %health.credential, "no gateway is attached; the next one hears this report");
            }
        }
    }
}

pub(crate) async fn push_health_snapshot(shared: &Shared, epoch: u64) {
    let Some(_in_order) = shared.in_order(&shared.health_sending).await else {
        return;
    };
    let Some(epoch) = shared.live().filter(|live| live.id == epoch) else {
        return;
    };
    let snapshot: Vec<CredentialHealth> = lock(&shared.health).values().cloned().collect();
    for health in snapshot {
        push(shared, &epoch, NodeCall::CredentialHealth(health)).await;
    }
}

async fn push(shared: &Shared, epoch: &Epoch, call: NodeCall) {
    let what = match &call {
        NodeCall::SignIn(_) => "sign_in",
        NodeCall::SignInSettled(_) => "sign_in.settled",
        NodeCall::CredentialHealth(_) => "credential.health",
        NodeCall::ReadResource(_) => "resource.read",
    };
    let answered = tokio::select! {
        answered = timeout(shared.call_timeout, epoch.handle.call(call)) => answered,
        () = epoch.ended.cancelled() => return,
        () = shared.stopping.cancelled() => return,
    };
    match answered {
        Ok(Ok(_)) | Ok(Err(CallError::LinkReset | CallError::Closed)) => {}
        Ok(Err(err)) => {
            tracing::warn!(method = what, error = %err, "the gateway did not accept an update");
        }
        Err(_) => {
            tracing::warn!(
                method = what,
                "the gateway did not answer an update in time"
            );
        }
    }
}
