use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use rax::attachment::Attachment;
use rax::event::BackgroundEvent;
use rax::id::{PromptId, RequestId};
use rax::interaction::{
    DisplayAnswer, DisplayOutcome, PlanRequest, QuestionRequest, SignInRequest,
};
use rax::tool::{Decision, DeniedBy};
use rax::{ErrorKind, Event, ToolCall};
use rax_tokio::TransferError;
use rax_tokio::node::NodeHandle;
use tokio::sync::{mpsc, oneshot};
use tokio::time::{Instant, sleep_until};

use crate::backend::{AttachmentSource, Backend, BackendError, BackendEvent, SessionKey};
use crate::state::{
    self, HoldStart, NOBODY, PromptKind, PromptRoute, PromptStart, Shared, TIMED_OUT, TurnEntry,
    UNSENT, WITHDRAWN, deny, outcome,
};

/// Lets the binary repair a failure before the gateway sees it, such as putting a sign-in in front
/// of the owner and reporting `credential` once it is shown.
pub type TurnFailed =
    Arc<dyn Fn(rax::Error) -> Pin<Box<dyn Future<Output = rax::Error> + Send>> + Send + Sync>;

pub(crate) enum Item {
    Backend(BackendEvent),
    Raise(Event),
}

#[derive(Clone)]
pub struct TurnSink {
    sender: mpsc::Sender<Item>,
}

#[derive(Debug, thiserror::Error)]
#[error("riggs-node: this turn is no longer being delivered")]
pub struct TurnClosed;

impl TurnSink {
    /// Waits for room, which is how a gateway that stopped reading slows the agent down. An error
    /// means nobody delivers this turn any more: drain and stop rather than retry.
    pub async fn send(&self, event: BackendEvent) -> Result<(), TurnClosed> {
        self.sender
            .send(Item::Backend(event))
            .await
            .map_err(|_| TurnClosed)
    }
}

pub(crate) fn channel(capacity: usize) -> (mpsc::Sender<Item>, mpsc::Receiver<Item>) {
    mpsc::channel(capacity.max(1))
}

pub(crate) fn sink(sender: mpsc::Sender<Item>) -> TurnSink {
    TurnSink { sender }
}

/// Weak, so a sub-agent that keeps the gate never keeps its turn open. Once the turn has ended,
/// holds are sent as background tool calls for the session.
#[derive(Clone)]
pub struct ToolGate {
    pub(crate) shared: Arc<Shared>,
    pub(crate) session: SessionKey,
    pub(crate) stream: RequestId,
    pub(crate) outbox: mpsc::WeakSender<Item>,
}

impl ToolGate {
    /// Pass a deadline earlier than the agent's own hook timeout, so the model hears a deny rather
    /// than an internal error.
    pub async fn hold(&self, call: ToolCall, deadline: Instant) -> Decision {
        let ticket = match self
            .shared
            .hold(&call.id, Some((&self.session, &self.stream)))
        {
            HoldStart::Registered(ticket) => ticket,
            HoldStart::NoTurn => {
                return hold_background(&self.shared, &self.session, call, deadline).await;
            }
            HoldStart::Refused(decision) => return decision,
        };
        let release = Release::new(&self.shared, &ticket);
        let Some(outbox) = self.outbox.upgrade() else {
            drop(release);
            return hold_background(&self.shared, &self.session, call, deadline).await;
        };
        let announce = async move {
            outbox
                .send(Item::Raise(Event::ToolCall { tool_call: call }))
                .await
                .is_ok()
        };
        let decision = wait(ticket.decision, announce, deadline).await;
        drop(release);
        decision
    }
}

pub(crate) async fn hold_background(
    shared: &Shared,
    session: &SessionKey,
    call: ToolCall,
    deadline: Instant,
) -> Decision {
    let ticket = match shared.hold(&call.id, None) {
        HoldStart::Registered(ticket) => ticket,
        HoldStart::NoTurn => return deny(NOBODY),
        HoldStart::Refused(decision) => return decision,
    };
    let release = Release::new(shared, &ticket);
    let handle = ticket.epoch.handle.clone();
    let session_id = session.session_id();
    let announce = async move {
        let event = BackgroundEvent::ToolCall { tool_call: call };
        match handle.background(session_id, event).await {
            Ok(()) => true,
            Err(err) => {
                tracing::warn!(error = %err, "could not raise an approval request");
                false
            }
        }
    };
    let decision = wait(ticket.decision, announce, deadline).await;
    drop(release);
    decision
}

struct Release<'a> {
    shared: &'a Shared,
    id: rax::id::ToolCallId,
    serial: u64,
}

impl<'a> Release<'a> {
    fn new(shared: &'a Shared, ticket: &state::HoldTicket) -> Self {
        Self {
            shared,
            id: ticket.id.clone(),
            serial: ticket.serial,
        }
    }
}

impl Drop for Release<'_> {
    fn drop(&mut self) {
        self.shared.release_hold(&self.id, self.serial);
    }
}

async fn wait(
    mut decision: oneshot::Receiver<Decision>,
    announce: impl Future<Output = bool>,
    deadline: Instant,
) -> Decision {
    let announced = async {
        if announce.await {
            std::future::pending::<Decision>().await
        } else {
            deny(UNSENT)
        }
    };
    tokio::select! {
        decided = &mut decision => decided.unwrap_or_else(|_| deny(WITHDRAWN)),
        unsent = announced => unsent,
        () = sleep_until(deadline) => Decision::Deny {
            by: DeniedBy::Timeout,
            reason: Some(TIMED_OUT.to_owned()),
        },
    }
}

/// Weak for the same reason as [`ToolGate`]. A prompt raised after its turn ended is answered
/// `no_conversation`, because nobody is there to answer it.
#[derive(Clone)]
pub struct TurnPrompts {
    pub(crate) shared: Arc<Shared>,
    pub(crate) session: SessionKey,
    pub(crate) stream: RequestId,
    pub(crate) outbox: mpsc::WeakSender<Item>,
}

pub struct SignInPrompt {
    pub id: PromptId,
    /// Closes once the sign-in is settled terminally, dismissed, or its link ends.
    pub answers: mpsc::Receiver<DisplayAnswer>,
}

impl SignInPrompt {
    pub(crate) fn refused(id: PromptId, refusal: DisplayOutcome) -> Self {
        let (sender, answers) = mpsc::channel(1);
        let _ = sender.try_send(outcome(&id, refusal));
        Self { id, answers }
    }
}

impl TurnPrompts {
    pub async fn question(&self, request: QuestionRequest) -> DisplayAnswer {
        let id = request.id.clone();
        let event = Event::Question { question: request };
        self.ask(id, PromptKind::Question, event).await
    }

    pub async fn plan(&self, request: PlanRequest) -> DisplayAnswer {
        let id = request.id.clone();
        self.ask(id, PromptKind::Plan, Event::Plan { plan: request })
            .await
    }

    /// Settle it with `BackendEvent::SignInSettled` on the same turn.
    pub async fn sign_in(&self, request: SignInRequest) -> SignInPrompt {
        let id = request.id.clone();
        let (serial, answers) = match self.open(&id, PromptKind::SignIn, state::open) {
            PromptStart::Registered {
                serial, answers, ..
            } => (serial, answers),
            PromptStart::Refused(refusal) => return SignInPrompt::refused(id, refusal),
        };
        if !self.announce(Event::SignIn { sign_in: request }).await {
            self.shared.forget_prompt(&id, Some(serial));
            return SignInPrompt::refused(id, DisplayOutcome::NoConversation);
        }
        SignInPrompt { id, answers }
    }

    fn open<T>(
        &self,
        id: &PromptId,
        kind: PromptKind,
        answerer: impl FnOnce() -> (state::Answerer, T),
    ) -> PromptStart<T> {
        let route = PromptRoute::Turn {
            session: &self.session,
            stream: &self.stream,
        };
        self.shared.open_prompt(id, kind, route, answerer)
    }

    async fn announce(&self, event: Event) -> bool {
        match self.outbox.upgrade() {
            Some(outbox) => outbox.send(Item::Raise(event)).await.is_ok(),
            None => false,
        }
    }

    async fn ask(&self, id: PromptId, kind: PromptKind, event: Event) -> DisplayAnswer {
        let (serial, mut answer) = match self.open(&id, kind, state::once) {
            PromptStart::Registered {
                serial, answers, ..
            } => (serial, answers),
            PromptStart::Refused(refusal) => return outcome(&id, refusal),
        };
        let announced = async {
            if self.announce(event).await {
                std::future::pending::<()>().await;
            }
        };
        tokio::select! {
            answered = &mut answer => {
                answered.unwrap_or_else(|_| outcome(&id, DisplayOutcome::Dismissed))
            }
            () = announced => {
                self.shared.forget_prompt(&id, Some(serial));
                outcome(&id, DisplayOutcome::NoConversation)
            }
        }
    }
}

#[derive(Clone)]
pub(crate) struct Failures {
    pub(crate) backend: Arc<dyn Backend>,
    pub(crate) hook: Option<TurnFailed>,
}

impl Failures {
    pub(crate) async fn map(&self, err: BackendError) -> rax::Error {
        let error = rax::Error::new(self.backend.classify(&err), err.to_string());
        match &self.hook {
            Some(hook) => hook(error).await,
            None => error,
        }
    }
}

pub(crate) struct Pump {
    pub(crate) shared: Arc<Shared>,
    pub(crate) handle: NodeHandle,
    pub(crate) key: SessionKey,
    pub(crate) turn: TurnEntry,
    pub(crate) failures: Failures,
}

impl Pump {
    pub(crate) async fn run(self, mut items: mpsc::Receiver<Item>, mut healthy: bool) {
        while let Some(item) = items.recv().await {
            if healthy && !self.turn.orphaned.is_cancelled() {
                healthy = self.forward(item).await;
            }
        }
        self.shared.finish_turn(&self.key, &self.turn.stream);
        if healthy && !self.turn.orphaned.is_cancelled() {
            self.deliver(self.handle.end(self.turn.stream.clone()))
                .await;
        }
    }

    async fn forward(&self, item: Item) -> bool {
        let event = match item {
            Item::Raise(event) => event,
            Item::Backend(BackendEvent::Message(content)) => Event::Message { content },
            Item::Backend(BackendEvent::Status(text)) => Event::Status { text },
            Item::Backend(BackendEvent::ToolCallUpdate(tool_call_update)) => {
                Event::ToolCallUpdate { tool_call_update }
            }
            Item::Backend(BackendEvent::PlanUpdate(entries)) => Event::PlanUpdate { entries },
            Item::Backend(BackendEvent::Attachment(source)) => return self.attach(source).await,
            Item::Backend(BackendEvent::SignInSettled(settled)) => {
                let stream = Some(&self.turn.stream);
                if self.shared.prompt_epoch(&settled.id, stream).is_none() {
                    tracing::debug!(prompt = %settled.id, "dropping a settle for a sign-in that was never shown");
                    return true;
                }
                if settled.state.is_terminal() {
                    self.shared.forget_prompt(&settled.id, None);
                }
                Event::SignInSettled {
                    sign_in_settled: settled,
                }
            }
            Item::Backend(BackendEvent::Complete(stop_reason)) => Event::Complete { stop_reason },
            Item::Backend(BackendEvent::Error(err)) => Event::Error {
                error: self.failures.map(err).await,
            },
        };
        self.deliver(self.handle.event(self.turn.stream.clone(), event))
            .await
    }

    async fn attach(&self, source: AttachmentSource) -> bool {
        let AttachmentSource { meta, size, reader } = source;
        let sent = tokio::select! {
            sent = self.handle.send_attachment_from(reader, size) => sent,
            () = self.turn.orphaned.cancelled() => return false,
        };
        let event = match sent {
            Ok(transfer_id) => Event::Attachment {
                attachment: Attachment {
                    transfer_id,
                    size,
                    filename: meta.filename,
                    title: meta.title,
                    comment: meta.comment,
                    mimetype: meta.mimetype,
                },
            },
            Err(TransferError::Send(err)) => {
                tracing::warn!(stream = %self.turn.stream, error = %err, "could not forward an attachment");
                return false;
            }
            Err(err) => {
                let name = meta.filename.as_deref().unwrap_or("attachment");
                tracing::warn!(stream = %self.turn.stream, error = %err, "an attachment could not be transferred");
                let message = format!("attachment {name:?} could not be transferred: {err}");
                Event::Error {
                    error: rax::Error::new(ErrorKind::Unknown, message),
                }
            }
        };
        self.deliver(self.handle.event(self.turn.stream.clone(), event))
            .await
    }

    async fn deliver<E: std::fmt::Display>(
        &self,
        send: impl Future<Output = Result<(), E>>,
    ) -> bool {
        tokio::select! {
            sent = send => match sent {
                Ok(()) => true,
                Err(err) => {
                    tracing::warn!(stream = %self.turn.stream, error = %err, "could not forward an event");
                    false
                }
            },
            () = self.turn.orphaned.cancelled() => false,
        }
    }
}
