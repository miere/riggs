use std::collections::{HashMap, HashSet};
use std::io::Cursor;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use agent_client_protocol::{self as acp, ConnectionTo, Handled, Responder, UntypedMessage};
use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::tool::{DeniedBy, PlanEntry, ToolCallStatus, ToolCallUpdate};
use rax::{Decision, Open};
use riggs_node::{
    AttachmentMeta, AttachmentSource, BackendEvent, HostHandles, SessionKey, ToolGate, TurnSink,
};
use serde_json::{Value, json};
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

use crate::error::AcpError;
use crate::map::{self, Reply};
use crate::permission::{self, Answer};
use crate::wire::{self, PermissionOption, ToolFields, Update};

const PERMISSION_METHOD: &str = "session/request_permission";
const UPDATE_METHOD: &str = "session/update";
const CANCEL_METHOD: &str = "session/cancel";

/// Where a session's traffic goes. A probe process has no owner, so it relays nothing.
#[derive(Clone)]
pub(crate) struct Owner {
    pub(crate) key: SessionKey,
    pub(crate) host: HostHandles,
}

/// Routing state for one agent process. The turn is closed in the connection's dispatch order,
/// so an update the agent sends after answering `session/prompt` never reaches the next turn.
pub(crate) struct Routes {
    owner: Option<Owner>,
    permission_timeout: Duration,
    state: Mutex<State>,
}

#[derive(Default)]
struct State {
    session: Option<String>,
    restoring: bool,
    generation: u64,
    turn: Option<TurnRoute>,
    held: HashMap<String, Held>,
    tools: HashMap<String, ToolFields>,
    announced: HashSet<String>,
    denied: HashSet<String>,
}

pub(crate) struct TurnRoute {
    pub(crate) generation: u64,
    pub(crate) events: TurnSink,
    gate: ToolGate,
    cancelled: CancellationToken,
    interrupt: CancellationToken,
}

struct Held {
    responder: Responder<Value>,
    title: Option<String>,
    generation: Option<u64>,
}

pub(crate) enum Target {
    Turn(TurnSink),
    Background,
}

pub(crate) enum Out {
    Message(Open<ContentBlock>),
    Status(String),
    Tool(ToolCallUpdate),
    Plan(Vec<PlanEntry>),
    Attachment {
        meta: AttachmentMeta,
        bytes: Vec<u8>,
    },
}

pub(crate) struct OpenedTurn {
    pub(crate) generation: u64,
    pub(crate) session: String,
    pub(crate) interrupt: CancellationToken,
}

impl Routes {
    pub(crate) fn new(owner: Option<Owner>, permission_timeout: Duration) -> Self {
        Self {
            owner,
            permission_timeout,
            state: Mutex::default(),
        }
    }

    fn state(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(PoisonError::into_inner)
    }

    pub(crate) fn set_session(&self, session: &str, restoring: bool) {
        let mut state = self.state();
        state.session = Some(session.to_owned());
        state.restoring = restoring;
    }

    pub(crate) fn restored(&self) {
        self.state().restoring = false;
    }

    pub(crate) fn open_turn(
        &self,
        events: TurnSink,
        gate: ToolGate,
        cancelled: CancellationToken,
    ) -> Result<OpenedTurn, AcpError> {
        let mut state = self.state();
        let Some(session) = state.session.clone() else {
            return Err(AcpError::AgentGone(
                "the agent has no session open".to_owned(),
            ));
        };
        if state.turn.is_some() {
            return Err(AcpError::Busy);
        }
        state.generation += 1;
        let generation = state.generation;
        let interrupt = CancellationToken::new();
        state.turn = Some(TurnRoute {
            generation,
            events,
            gate,
            cancelled,
            interrupt: interrupt.clone(),
        });
        Ok(OpenedTurn {
            generation,
            session,
            interrupt,
        })
    }

    pub(crate) fn close_turn(&self, generation: u64) -> Option<TurnRoute> {
        let mut state = self.state();
        if state
            .turn
            .as_ref()
            .is_some_and(|turn| turn.generation == generation)
        {
            return state.turn.take();
        }
        None
    }

    /// Held requests are answered `cancelled` before `session/cancel` is queued, as ACP requires,
    /// because both go through the same ordered outgoing queue.
    pub(crate) fn interrupt(&self, cx: &ConnectionTo<acp::Agent>) -> Vec<(Target, ToolCallUpdate)> {
        let mut state = self.state();
        let Some(session) = state.session.clone() else {
            return Vec::new();
        };
        let Some(turn) = &state.turn else {
            return Vec::new();
        };
        if turn.interrupt.is_cancelled() {
            return Vec::new();
        }
        turn.interrupt.cancel();
        let denied = answer_all_cancelled(&mut state);
        let cancel = UntypedMessage::new(CANCEL_METHOD, json!({ "sessionId": session }))
            .and_then(|cancel| cx.send_notification(cancel));
        if let Err(err) = cancel {
            tracing::warn!(error = %err, "could not send session/cancel to the agent");
        }
        denied
    }

    pub(crate) fn cancel_held(&self) {
        answer_all_cancelled(&mut self.state());
    }

    pub(crate) async fn notification(&self, message: UntypedMessage) {
        if message.method != UPDATE_METHOD {
            tracing::debug!(method = %message.method, "ignoring a notification riggs does not handle");
            return;
        }
        let notification: wire::SessionNotification = match serde_json::from_value(message.params) {
            Ok(notification) => notification,
            Err(err) => {
                tracing::warn!(error = %err, "dropping a session/update riggs cannot read");
                return;
            }
        };
        let update = match wire::update(notification.update) {
            Ok(update) => update,
            Err(err) => {
                tracing::warn!(error = %err, "dropping a session/update riggs cannot read");
                return;
            }
        };
        let (target, outs) = {
            let mut state = self.state();
            if state.session.as_deref() != Some(notification.session_id.as_str()) {
                tracing::debug!(session = %notification.session_id, "dropping an update for another agent session");
                return;
            }
            if state.restoring {
                return;
            }
            let outs = match update {
                Update::Content(blocks) => blocks.into_iter().filter_map(reply).collect(),
                Update::Tool(fields) => state.tool(*fields).map(Out::Tool).into_iter().collect(),
                Update::Plan(entries) => vec![Out::Plan(entries)],
                Update::Silent(kind) => {
                    tracing::debug!(%kind, "not relaying a session/update");
                    return;
                }
                Update::Unknown(kind) => {
                    tracing::warn!(%kind, "dropping a session/update of a kind riggs does not know");
                    return;
                }
            };
            (state.target(None), outs)
        };
        for out in outs {
            self.deliver(&target, out).await;
        }
    }

    pub(crate) fn request(
        self: &Arc<Self>,
        message: UntypedMessage,
        responder: Responder<Value>,
        cx: &ConnectionTo<acp::Agent>,
    ) -> Handled<(UntypedMessage, Responder<Value>)> {
        if message.method != PERMISSION_METHOD {
            return Handled::No {
                message: (message, responder),
                retry: false,
            };
        }
        let request = match wire::permission_request(message.params) {
            Ok(request) => request,
            Err(err) => {
                tracing::warn!(error = %err, "refusing a permission request riggs cannot read");
                respond(responder, acp::Error::invalid_params().data(json!(err)));
                return Handled::Yes;
            }
        };
        if request.skipped_options > 0 {
            tracing::warn!(
                skipped = request.skipped_options,
                "ignoring permission options of a kind riggs does not know"
            );
        }
        let Some(owner) = self.owner.clone() else {
            answer(responder, &Answer::Cancelled, None);
            return Handled::Yes;
        };
        let id = request.tool.tool_call_id.clone();
        let mut state = self.state();
        if state.session.as_deref() != Some(request.session_id.as_str()) {
            drop(state);
            tracing::warn!(session = %request.session_id, "refusing a permission request for another agent session");
            answer(responder, &Answer::Cancelled, None);
            return Handled::Yes;
        }
        if request.options.is_empty() {
            let target = state.target(None);
            drop(state);
            tracing::warn!(tool_call_id = %id, "the agent asked for approval with no options; refusing");
            answer(responder, &Answer::Cancelled, None);
            let routes = self.clone();
            let note = Out::Status("the agent asked for approval but offered no options".into());
            let spawned = cx.spawn(async move {
                routes.deliver(&target, note).await;
                Ok(())
            });
            if let Err(err) = spawned {
                tracing::debug!(error = %err, "could not report a refused approval");
            }
            return Handled::Yes;
        }
        if state.held.contains_key(&id) {
            drop(state);
            tracing::warn!(tool_call_id = %id, "the agent asked twice about the same tool call; refusing the second");
            answer(responder, &Answer::Cancelled, None);
            return Handled::Yes;
        }
        let mut fields = state.tools.get(&id).cloned().unwrap_or_default();
        fields.merge(&request.tool);
        fields.tool_call_id.clone_from(&id);
        let call = map::tool_call(&fields);
        let turn = state.turn.as_ref().map(|turn| {
            (
                turn.gate.clone(),
                turn.cancelled.clone(),
                turn.interrupt.clone(),
                turn.generation,
            )
        });
        state.held.insert(
            id.clone(),
            Held {
                responder,
                title: fields.title.clone(),
                generation: turn.as_ref().map(|turn| turn.3),
            },
        );
        state.announced.insert(id.clone());
        drop(state);
        let deadline = Instant::now() + self.permission_timeout;
        let routes = self.clone();
        let options = request.options;
        let wait_id = id.clone();
        let wait = async move {
            let decision = match turn {
                Some((gate, cancelled, interrupt, _)) => {
                    let decided = tokio::select! {
                        decision = gate.hold(call, deadline) => Some(decision),
                        () = interrupt.cancelled() => None,
                    };
                    decided.filter(|_| !cancelled.is_cancelled() && !interrupt.is_cancelled())
                }
                None => Some(
                    owner
                        .host
                        .gate
                        .hold_background(&owner.key, call, deadline)
                        .await,
                ),
            };
            routes.settle(&wait_id, &options, decision).await;
            Ok(())
        };
        if let Err(err) = cx.spawn(wait) {
            tracing::warn!(error = %err, "could not wait for an approval; refusing it");
            if let Some(held) = self.state().held.remove(&id) {
                answer(held.responder, &Answer::Cancelled, None);
            }
        }
        Handled::Yes
    }

    async fn settle(&self, id: &str, options: &[PermissionOption], decision: Option<Decision>) {
        let (target, update, note) = {
            let mut state = self.state();
            let Some(held) = state.held.remove(id) else {
                return;
            };
            let chosen = match &decision {
                Some(decision) => permission::choose(options, decision),
                None => Answer::Cancelled,
            };
            answer(held.responder, &chosen, decision.as_ref());
            if chosen.allowed() {
                return;
            }
            state.denied.insert(id.to_owned());
            let note = match &decision {
                Some(Decision::Allow) => {
                    tracing::warn!(tool_call_id = %id, "the agent offered no one-time approval; refusing rather than approving for good");
                    Some("the agent offered no one-time approval, so the call was refused")
                }
                Some(Decision::Deny {
                    by: DeniedBy::Timeout,
                    ..
                }) => Some("approval timed out"),
                _ => None,
            };
            (
                state.target(held.generation),
                map::denied(id, held.title),
                note,
            )
        };
        self.deliver(&target, Out::Tool(update)).await;
        if let Some(note) = note {
            self.deliver(&target, Out::Status(note.to_owned())).await;
        }
    }

    pub(crate) async fn deliver(&self, target: &Target, out: Out) {
        let Some(owner) = &self.owner else {
            return;
        };
        match target {
            Target::Turn(events) => {
                let event = match out {
                    Out::Message(content) => BackendEvent::Message(content),
                    Out::Status(text) => BackendEvent::Status(text),
                    Out::Tool(update) => BackendEvent::ToolCallUpdate(update),
                    Out::Plan(entries) => BackendEvent::PlanUpdate(entries),
                    Out::Attachment { meta, bytes } => {
                        BackendEvent::Attachment(attachment(meta, bytes))
                    }
                };
                if events.send(event).await.is_err() {
                    tracing::debug!(session_id = %owner.key, "the turn stopped being delivered");
                }
            }
            Target::Background => {
                let background = &owner.host.background;
                let event = match out {
                    Out::Message(content) => BackgroundEvent::Message { content },
                    Out::Status(text) => BackgroundEvent::Status { text },
                    Out::Tool(tool_call_update) => {
                        BackgroundEvent::ToolCallUpdate { tool_call_update }
                    }
                    Out::Plan(entries) => BackgroundEvent::PlanUpdate { entries },
                    Out::Attachment { meta, bytes } => {
                        if let Err(err) =
                            background.attach(&owner.key, attachment(meta, bytes)).await
                        {
                            tracing::debug!(session_id = %owner.key, error = %err, "background attachment not delivered");
                        }
                        return;
                    }
                };
                if let Err(err) = background.send(&owner.key, event).await {
                    tracing::debug!(session_id = %owner.key, error = %err, "background event not delivered");
                }
            }
        }
    }
}

impl State {
    fn target(&self, generation: Option<u64>) -> Target {
        match &self.turn {
            Some(turn) if generation.is_none_or(|generation| generation == turn.generation) => {
                Target::Turn(turn.events.clone())
            }
            _ => Target::Background,
        }
    }

    fn tool(&mut self, fields: ToolFields) -> Option<ToolCallUpdate> {
        let id = fields.tool_call_id.clone();
        let status = map::tool_status(fields.status.as_deref());
        let known = self.tools.entry(id.clone()).or_default();
        known.merge(&fields);
        known.tool_call_id.clone_from(&id);
        if matches!(
            status,
            Some(ToolCallStatus::Completed | ToolCallStatus::Failed)
        ) {
            self.tools.remove(&id);
        }
        if !self.announced.contains(&id) || self.denied.contains(&id) {
            return None;
        }
        Some(map::tool_update(&fields, status?))
    }
}

fn answer_all_cancelled(state: &mut State) -> Vec<(Target, ToolCallUpdate)> {
    let held: Vec<(String, Held)> = state.held.drain().collect();
    held.into_iter()
        .map(|(id, held)| {
            answer(held.responder, &Answer::Cancelled, None);
            state.denied.insert(id.clone());
            (state.target(held.generation), map::denied(&id, held.title))
        })
        .collect()
}

fn reply(block: Value) -> Option<Out> {
    Some(match map::reply(block)? {
        Reply::Message(content) => Out::Message(content),
        Reply::Attachment { meta, bytes } => Out::Attachment { meta, bytes },
        Reply::TooLarge { name } => {
            Out::Status(format!("attachment {name:?} is too large to send"))
        }
    })
}

fn attachment(meta: AttachmentMeta, bytes: Vec<u8>) -> AttachmentSource {
    AttachmentSource {
        meta,
        size: bytes.len() as u64,
        reader: Box::new(Cursor::new(bytes)),
    }
}

fn answer(responder: Responder<Value>, chosen: &Answer, decision: Option<&Decision>) {
    if let Err(err) = responder.respond(permission::response(chosen, decision)) {
        tracing::debug!(error = %err, "could not answer a permission request");
    }
}

fn respond(responder: Responder<Value>, error: acp::Error) {
    if let Err(err) = responder.respond_with_error(error) {
        tracing::debug!(error = %err, "could not refuse a request");
    }
}
