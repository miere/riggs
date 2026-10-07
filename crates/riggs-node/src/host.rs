use std::sync::Arc;

use rax::attachment::Attachment;
use rax::credential::CredentialHealth;
use rax::event::BackgroundEvent;
use rax::interaction::{DisplayOutcome, SignInRequest, SignInSettled, SignInState};
use rax::tool::{CallTool, Decision, ToolOutcome};
use rax::{NodeCall, NodeReply, ToolCall, UpdateMetadata};
use rax_tokio::{CallError, SendError, TransferError};
use serde_json::Value;
use tokio::io::AsyncRead;
use tokio::sync::oneshot;
use tokio::time::{Instant, timeout};

use crate::backend::{AttachmentMeta, AttachmentSource, Delivery, SessionKey};
use crate::state::{self, Epoch, PromptKind, PromptRoute, PromptStart, Shared, Unreached, lock};
use crate::turn::{SignInPrompt, hold_background};

/// Everything a backend may reach with no turn open. It outlives every socket and link.
#[derive(Clone)]
pub struct HostHandles {
    pub gate: BackgroundGate,
    pub background: BackgroundSink,
    pub sign_ins: SignIns,
    pub credentials: CredentialReporter,
    pub tools: GatewayTools,
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
            credentials: CredentialReporter {
                shared: shared.clone(),
            },
            tools: GatewayTools { shared },
        }
    }
}

/// The tools each session's gateway runs on this node's behalf. The node publishes them to its agent
/// and calls them back; it never learns what any of them do.
#[derive(Clone)]
pub struct GatewayTools {
    shared: Arc<Shared>,
}

/// Every variant is a reason the tool never ran, so each must reach the agent as a refusal it can
/// act on. Silence would leave a turn hanging on a gateway that is not coming back.
#[derive(Debug, thiserror::Error)]
pub enum ToolUnreachable {
    #[error(
        "the gateway that opened this session is not attached, so its tools cannot be reached right now"
    )]
    NoGateway,
    #[error("the link ended before the gateway answered")]
    LinkEnded,
    #[error("the gateway did not answer in time")]
    TimedOut,
    #[error("the gateway refused it: {0}")]
    Refused(String),
}

impl GatewayTools {
    /// Runs `name` from the group `namespace` the session was opened with. The groups themselves
    /// travel with `session.new`, never through here: this node declares `tool_groups`, so it
    /// ignores any catalogue a gateway still sends at `initialize`.
    pub async fn call(
        &self,
        session: &SessionKey,
        namespace: &str,
        name: &str,
        arguments: Value,
    ) -> Result<ToolOutcome, ToolUnreachable> {
        let epoch = self
            .shared
            .owner_link(session)
            .map_err(|_| ToolUnreachable::NoGateway)?;
        let call = NodeCall::CallTool(CallTool {
            session_id: session.session_id(),
            namespace: Some(namespace.to_owned()),
            name: name.to_owned(),
            arguments: Some(arguments),
        });
        let answered = tokio::select! {
            answered = timeout(self.shared.call_timeout, epoch.handle.call(call)) => answered,
            () = epoch.ended.cancelled() => return Err(ToolUnreachable::LinkEnded),
        };
        match answered {
            Ok(Ok(NodeReply::CallTool(outcome))) => Ok(outcome),
            // A reply to another method cannot happen without a broken gateway, but the agent still
            // has to be told something rather than wait.
            Ok(Ok(_)) => Err(ToolUnreachable::Refused(
                "the gateway answered a different call".to_owned(),
            )),
            Ok(Err(err)) => Err(ToolUnreachable::Refused(err.to_string())),
            Err(_) => Err(ToolUnreachable::TimedOut),
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
    #[error(
        "riggs-node: the session's gateway is not attached, so the background event was dropped"
    )]
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
        let AttachmentSource {
            meta,
            size,
            reader,
            mut delivery,
        } = source;
        let sent = self
            .send_attachment(session, meta, size, reader, &mut delivery)
            .await;
        if let (Err(err), Some(delivery)) = (&sent, delivery) {
            let _ = delivery.send(Delivery::Failed(err.to_string()));
        }
        sent
    }

    /// Hands `delivery` to the receipt it waits on when the gateway sends one, and otherwise
    /// leaves it for the caller to settle.
    async fn send_attachment(
        &self,
        session: &SessionKey,
        meta: AttachmentMeta,
        size: u64,
        reader: Box<dyn AsyncRead + Send + Unpin>,
        delivery: &mut Option<oneshot::Sender<Delivery>>,
    ) -> Result<(), NotDelivered> {
        let epoch = self.epoch(session)?;
        let transfer_id = tokio::select! {
            sent = epoch.handle.send_attachment_from(reader, size) => sent?,
            () = epoch.ended.cancelled() => return Err(NotDelivered::LinkEnded),
        };
        if self.shared.acknowledges(epoch.id)
            && let Some(waiting) = delivery.take()
        {
            self.shared
                .expect_receipt(epoch.id, transfer_id.clone(), waiting);
        }
        let attachment = Attachment {
            transfer_id: transfer_id.clone(),
            size,
            filename: meta.filename,
            title: meta.title,
            comment: meta.comment,
            mimetype: meta.mimetype,
        };
        let event = BackgroundEvent::Attachment { attachment };
        let sent = tokio::select! {
            sent = epoch.handle.background(session.session_id(), event) => sent.map_err(NotDelivered::from),
            () = epoch.ended.cancelled() => Err(NotDelivered::LinkEnded),
        };
        match sent {
            Ok(()) => {
                if let Some(unacknowledged) = delivery.take() {
                    let _ = unacknowledged.send(Delivery::Unconfirmed);
                }
                Ok(())
            }
            Err(err) => {
                if delivery.is_none() {
                    *delivery = self.shared.forget_receipt(epoch.id, &transfer_id);
                }
                Err(err)
            }
        }
    }

    fn epoch(&self, session: &SessionKey) -> Result<Epoch, NotDelivered> {
        self.shared.owner_link(session).map_err(|unreached| {
            match unreached {
                Unreached::Unowned => tracing::debug!(session_id = %session, "dropping a background event; no gateway owns this session"),
                Unreached::Detached => tracing::debug!(session_id = %session, "dropping a background event; its gateway is not attached"),
            }
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
            PromptStart::Refused(DisplayOutcome::Unavailable)
                if shared.live(&shared.primary).is_none() =>
            {
                tracing::warn!(tool = %request.tool, gateway = %shared.primary, "a sign-in with no conversation was refused; the primary gateway is not attached");
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
        match raised.and_then(|raised| shared.epoch(raised)) {
            Some(epoch) => {
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

/// Reports go one at a time, so a recovery never overtakes the failure it ends, and to every
/// attached gateway, since the credential belongs to the whole node. The latest report per
/// credential is pushed again after every `initialize`.
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
        let lives: Vec<Epoch> = shared
            .lives()
            .into_iter()
            .filter(|epoch| epoch.caps.is_some())
            .collect();
        if lives.is_empty() {
            tracing::debug!(credential = %health.credential, "no gateway is attached; the next one hears this report");
        }
        for epoch in lives {
            push(shared, &epoch, NodeCall::CredentialHealth(health.clone())).await;
        }
    }
}

/// One update at a time, each carrying whatever is latest when its turn comes, so a burst of
/// edits ends with each gateway holding the last one. Each link gets its own merge. A gateway that
/// is not attached has nothing to do: its next `initialize` declares the latest anyway.
pub(crate) async fn push_metadata(shared: &Shared) {
    let Some(_in_order) = shared.in_order(&shared.metadata_sending).await else {
        return;
    };
    for epoch in shared.lives() {
        if epoch.caps.is_none() {
            continue;
        }
        let metadata = shared.metadata_for(&epoch.gateway);
        push(
            shared,
            &epoch,
            NodeCall::UpdateMetadata(UpdateMetadata { metadata }),
        )
        .await;
    }
}

pub(crate) async fn push_health_snapshot(shared: &Shared, epoch: u64) {
    let Some(_in_order) = shared.in_order(&shared.health_sending).await else {
        return;
    };
    let Some(epoch) = shared.epoch(epoch) else {
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
        NodeCall::CallTool(_) => "tool.call",
        NodeCall::UpdateMetadata(_) => "metadata.update",
    };
    let answered = tokio::select! {
        answered = timeout(shared.call_timeout, epoch.handle.call(call)) => answered,
        () = epoch.ended.cancelled() => return,
        () = shared.stopping.cancelled() => return,
    };
    let gateway = &epoch.gateway;
    match answered {
        Ok(Ok(_)) | Ok(Err(CallError::LinkReset | CallError::Closed)) => {}
        Ok(Err(err)) => {
            tracing::warn!(%gateway, method = what, error = %err, "the gateway did not accept an update");
        }
        Err(_) => {
            tracing::warn!(
                %gateway,
                method = what,
                "the gateway did not answer an update in time"
            );
        }
    }
}
