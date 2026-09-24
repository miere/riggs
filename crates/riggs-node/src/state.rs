use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Mutex, MutexGuard, PoisonError};
use std::time::Duration;

use rax::Metadata;
use rax::credential::CredentialHealth;
use rax::id::{PromptId, RequestId, ToolCallId};
use rax::interaction::{DisplayAnswer, DisplayOutcome};
use rax::session::GatewayCapabilities;
use rax::tool::{Decision, DeniedBy, ToolVerdict};
use rax_tokio::node::NodeHandle;
use tokio::sync::{mpsc, oneshot};
use tokio_util::sync::CancellationToken;

use crate::backend::SessionKey;

pub(crate) const INTERRUPTED: &str =
    "Skipped: the turn was interrupted before the approval was answered. The action was not run.";
pub(crate) const LINK_ENDED: &str = "Skipped: the connection to the gateway dropped before the approval was answered. The action was not run.";
pub(crate) const NOBODY: &str = "Skipped: no gateway is attached, so nobody could be asked to approve this. The action was not run.";
pub(crate) const TIMED_OUT: &str =
    "Skipped: nobody answered the approval request in time. The action was not run.";
pub(crate) const WITHDRAWN: &str =
    "Skipped: the approval request was withdrawn before it was answered. The action was not run.";
pub(crate) const SHUTTING_DOWN: &str = "Skipped: this node is shutting down, so the approval could not be answered. The action was not run.";
pub(crate) const UNSENT: &str =
    "Skipped: the approval request could not be sent. The action was not run.";
pub(crate) const DUPLICATE: &str =
    "Skipped: a call with this id is already waiting for approval. The action was not run.";

pub(crate) fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

pub(crate) fn deny(reason: &str) -> Decision {
    Decision::Deny {
        by: DeniedBy::Unavailable,
        reason: Some(reason.to_owned()),
    }
}

pub(crate) fn outcome(id: &PromptId, outcome: DisplayOutcome) -> DisplayAnswer {
    DisplayAnswer {
        id: id.clone(),
        outcome,
        answers: Default::default(),
        choice: None,
        user_id: None,
        note: None,
        code: None,
    }
}

#[derive(Clone)]
pub(crate) struct Epoch {
    pub(crate) id: u64,
    pub(crate) handle: NodeHandle,
    pub(crate) caps: Option<GatewayCapabilities>,
    pub(crate) ended: CancellationToken,
}

enum Phase {
    Detached,
    Live(Epoch),
    Lapsed {
        handle: NodeHandle,
        caps: Option<GatewayCapabilities>,
        orphans: Vec<RequestId>,
    },
}

#[derive(Clone)]
pub(crate) struct TurnEntry {
    pub(crate) stream: RequestId,
    pub(crate) epoch: u64,
    pub(crate) cancel: CancellationToken,
    pub(crate) orphaned: CancellationToken,
    pub(crate) finished: CancellationToken,
}

struct Held {
    serial: u64,
    epoch: u64,
    stream: Option<RequestId>,
    decide: oneshot::Sender<Decision>,
}

pub(crate) enum Answerer {
    Once(oneshot::Sender<DisplayAnswer>),
    Open(mpsc::Sender<DisplayAnswer>),
}

struct Pending {
    serial: u64,
    epoch: u64,
    stream: Option<RequestId>,
    answer: Answerer,
}

struct State {
    phase: Phase,
    turns: HashMap<SessionKey, TurnEntry>,
    held: HashMap<ToolCallId, Held>,
    prompts: HashMap<PromptId, Pending>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum ResetReason {
    ResumeRefused,
    GraceExpired,
    Closed,
    CredentialRejected,
    LinkEnded,
}

pub(crate) struct Reset {
    pub(crate) old_epoch: u64,
    pub(crate) turns: Vec<SessionKey>,
    pub(crate) calls_denied: usize,
}

pub(crate) enum Reserve {
    Reserved(TurnEntry),
    Busy,
    Draining(CancellationToken),
    Stale,
}

pub(crate) enum HoldStart {
    Registered(HoldTicket),
    NoTurn,
    Refused(Decision),
}

pub(crate) struct HoldTicket {
    pub(crate) id: ToolCallId,
    pub(crate) serial: u64,
    pub(crate) epoch: Epoch,
    pub(crate) decision: oneshot::Receiver<Decision>,
}

pub(crate) enum PromptRoute<'a> {
    Turn {
        session: &'a SessionKey,
        stream: &'a RequestId,
    },
    Background,
}

#[derive(Clone, Copy)]
pub(crate) enum PromptKind {
    Question,
    Plan,
    SignIn,
}

pub(crate) enum PromptStart<T> {
    Registered {
        serial: u64,
        epoch: Epoch,
        answers: T,
    },
    Refused(DisplayOutcome),
}

pub(crate) struct Shared {
    state: Mutex<State>,
    next_serial: AtomicU64,
    next_epoch: AtomicU64,
    pub(crate) call_timeout: Duration,
    /// Cancelled by `stop`, so nothing the node is telling the gateway outlives a shutdown.
    pub(crate) stopping: CancellationToken,
    pub(crate) settles: tokio::sync::Mutex<()>,
    pub(crate) health_sending: tokio::sync::Mutex<()>,
    pub(crate) health: Mutex<std::collections::BTreeMap<String, CredentialHealth>>,
    pub(crate) metadata_sending: tokio::sync::Mutex<()>,
    /// The owner's latest, which every `initialize` declares and every update replaces whole.
    pub(crate) metadata: Mutex<Metadata>,
}

impl Shared {
    pub(crate) fn new(call_timeout: Duration) -> Self {
        Self {
            state: Mutex::new(State {
                phase: Phase::Detached,
                turns: HashMap::new(),
                held: HashMap::new(),
                prompts: HashMap::new(),
            }),
            next_serial: AtomicU64::new(1),
            next_epoch: AtomicU64::new(1),
            call_timeout,
            stopping: CancellationToken::new(),
            settles: tokio::sync::Mutex::new(()),
            health_sending: tokio::sync::Mutex::new(()),
            health: Mutex::new(Default::default()),
            metadata_sending: tokio::sync::Mutex::new(()),
            metadata: Mutex::new(Metadata::new()),
        }
    }

    /// `None` once the node is stopping: a queue that orders calls must not outlast the calls.
    pub(crate) async fn in_order<'a>(
        &self,
        queue: &'a tokio::sync::Mutex<()>,
    ) -> Option<tokio::sync::MutexGuard<'a, ()>> {
        tokio::select! {
            guard = queue.lock() => Some(guard),
            () = self.stopping.cancelled() => None,
        }
    }

    fn serial(&self) -> u64 {
        self.next_serial.fetch_add(1, Ordering::Relaxed)
    }

    fn state(&self) -> MutexGuard<'_, State> {
        lock(&self.state)
    }

    pub(crate) fn live(&self) -> Option<Epoch> {
        match &self.state().phase {
            Phase::Live(epoch) => Some(epoch.clone()),
            _ => None,
        }
    }

    pub(crate) fn is_live(&self, epoch: u64) -> bool {
        matches!(&self.state().phase, Phase::Live(live) if live.id == epoch)
    }

    pub(crate) fn fresh(&self, handle: NodeHandle) -> (Option<Reset>, u64) {
        let reset = self.end_epoch(ResetReason::ResumeRefused);
        let id = self.next_epoch.fetch_add(1, Ordering::Relaxed);
        self.state().phase = Phase::Live(Epoch {
            id,
            handle,
            caps: None,
            ended: CancellationToken::new(),
        });
        (reset, id)
    }

    pub(crate) fn resumed(&self) -> Option<(NodeHandle, Vec<RequestId>)> {
        let mut state = self.state();
        let Phase::Lapsed { .. } = state.phase else {
            return None;
        };
        let Phase::Lapsed {
            handle,
            caps,
            orphans,
        } = std::mem::replace(&mut state.phase, Phase::Detached)
        else {
            return None;
        };
        state.phase = Phase::Live(Epoch {
            id: self.next_epoch.fetch_add(1, Ordering::Relaxed),
            handle: handle.clone(),
            caps,
            ended: CancellationToken::new(),
        });
        Some((handle, orphans))
    }

    pub(crate) fn set_caps(&self, epoch: u64, caps: GatewayCapabilities) -> bool {
        match &mut self.state().phase {
            Phase::Live(live) if live.id == epoch => {
                live.caps = Some(caps);
                true
            }
            _ => false,
        }
    }

    pub(crate) fn end_epoch(&self, reason: ResetReason) -> Option<Reset> {
        let mut state = self.state();
        if !matches!(state.phase, Phase::Live(_)) {
            if reason != ResetReason::GraceExpired {
                state.phase = Phase::Detached;
            }
            return None;
        }
        let Phase::Live(epoch) = std::mem::replace(&mut state.phase, Phase::Detached) else {
            return None;
        };
        epoch.ended.cancel();
        let mut turns = Vec::new();
        let mut orphans = Vec::new();
        for (key, turn) in &state.turns {
            if turn.epoch == epoch.id && !turn.orphaned.is_cancelled() {
                turn.orphaned.cancel();
                turn.cancel.cancel();
                turns.push(*key);
                orphans.push(turn.stream.clone());
            }
        }
        let calls_denied = state.deny_where(|held| held.epoch == epoch.id, LINK_ENDED);
        state.dismiss_where(|pending| pending.epoch == epoch.id);
        if reason == ResetReason::GraceExpired {
            state.phase = Phase::Lapsed {
                handle: epoch.handle.clone(),
                caps: epoch.caps.clone(),
                orphans,
            };
        }
        Some(Reset {
            old_epoch: epoch.id,
            turns,
            calls_denied,
        })
    }

    pub(crate) fn stop(&self) -> (Vec<SessionKey>, usize) {
        let mut state = self.state();
        self.stopping.cancel();
        let mut turns = Vec::new();
        for (key, turn) in &state.turns {
            turn.cancel.cancel();
            turns.push(*key);
        }
        let denied = state.deny_where(|_| true, SHUTTING_DOWN);
        state.dismiss_where(|_| true);
        (turns, denied)
    }

    pub(crate) fn reserve_turn(&self, key: &SessionKey, stream: &RequestId, epoch: u64) -> Reserve {
        let mut state = self.state();
        match &state.phase {
            Phase::Live(live) if live.id == epoch && !self.stopping.is_cancelled() => {}
            _ => return Reserve::Stale,
        }
        if let Some(open) = state.turns.get(key) {
            return if open.orphaned.is_cancelled() {
                Reserve::Draining(open.finished.clone())
            } else {
                Reserve::Busy
            };
        }
        let turn = TurnEntry {
            stream: stream.clone(),
            epoch,
            cancel: CancellationToken::new(),
            orphaned: CancellationToken::new(),
            finished: CancellationToken::new(),
        };
        state.turns.insert(*key, turn.clone());
        Reserve::Reserved(turn)
    }

    pub(crate) fn cancel_turn(&self, key: &SessionKey) -> bool {
        let mut state = self.state();
        let Some(turn) = state.turns.get(key).cloned() else {
            return false;
        };
        turn.cancel.cancel();
        let stream = Some(turn.stream);
        state.deny_where(|held| held.stream == stream, INTERRUPTED);
        state.interrupt_where(|pending| pending.stream == stream);
        true
    }

    pub(crate) fn finish_turn(&self, key: &SessionKey, stream: &RequestId) {
        let mut state = self.state();
        if state
            .turns
            .get(key)
            .is_some_and(|turn| &turn.stream == stream)
            && let Some(turn) = state.turns.remove(key)
        {
            turn.finished.cancel();
        }
        let stream = Some(stream.clone());
        for held in state.held.values_mut().filter(|held| held.stream == stream) {
            held.stream = None;
        }
        state.dismiss_where(|pending| pending.stream == stream);
    }

    pub(crate) fn hold(
        &self,
        id: &ToolCallId,
        route: Option<(&SessionKey, &RequestId)>,
    ) -> HoldStart {
        let mut state = self.state();
        if self.stopping.is_cancelled() {
            return HoldStart::Refused(deny(SHUTTING_DOWN));
        }
        let stream = match route {
            Some((key, stream)) => match state.turns.get(key) {
                Some(turn) if &turn.stream == stream => {
                    if turn.orphaned.is_cancelled() {
                        return HoldStart::Refused(deny(LINK_ENDED));
                    }
                    if turn.cancel.is_cancelled() {
                        return HoldStart::Refused(deny(INTERRUPTED));
                    }
                    Some(stream.clone())
                }
                _ => return HoldStart::NoTurn,
            },
            None => None,
        };
        let Phase::Live(epoch) = &state.phase else {
            return HoldStart::Refused(deny(NOBODY));
        };
        let epoch = epoch.clone();
        if state.held.contains_key(id) {
            return HoldStart::Refused(deny(DUPLICATE));
        }
        let (decide, decision) = oneshot::channel();
        let serial = self.serial();
        state.held.insert(
            id.clone(),
            Held {
                serial,
                epoch: epoch.id,
                stream,
                decide,
            },
        );
        HoldStart::Registered(HoldTicket {
            id: id.clone(),
            serial,
            epoch,
            decision,
        })
    }

    pub(crate) fn release_hold(&self, id: &ToolCallId, serial: u64) {
        let mut state = self.state();
        if state.held.get(id).is_some_and(|held| held.serial == serial) {
            state.held.remove(id);
        }
    }

    pub(crate) fn verdict(&self, verdict: ToolVerdict) -> bool {
        match self.state().held.remove(&verdict.id) {
            Some(held) => {
                let _ = held.decide.send(verdict.decision);
                true
            }
            None => false,
        }
    }

    pub(crate) fn open_prompt<T>(
        &self,
        id: &PromptId,
        kind: PromptKind,
        route: PromptRoute<'_>,
        answerer: impl FnOnce() -> (Answerer, T),
    ) -> PromptStart<T> {
        let mut state = self.state();
        let stream = match route {
            PromptRoute::Turn { session, stream } => match state.turns.get(session) {
                Some(turn) if &turn.stream == stream => {
                    if turn.cancel.is_cancelled() {
                        return PromptStart::Refused(DisplayOutcome::Dismissed);
                    }
                    Some(stream.clone())
                }
                _ => return PromptStart::Refused(DisplayOutcome::NoConversation),
            },
            PromptRoute::Background => None,
        };
        if self.stopping.is_cancelled() {
            return PromptStart::Refused(DisplayOutcome::Dismissed);
        }
        let Phase::Live(epoch) = &state.phase else {
            return PromptStart::Refused(DisplayOutcome::Unavailable);
        };
        let caps = epoch.caps.clone().unwrap_or_default();
        let allowed = match kind {
            PromptKind::Question => caps.question,
            PromptKind::Plan => caps.plan,
            PromptKind::SignIn => caps.sign_in,
        };
        if !allowed {
            tracing::debug!(prompt = %id, "the gateway cannot draw this prompt; answering unavailable");
            return PromptStart::Refused(DisplayOutcome::Unavailable);
        }
        let epoch = epoch.clone();
        if state.prompts.contains_key(id) {
            return PromptStart::Refused(DisplayOutcome::Unavailable);
        }
        let (answer, answers) = answerer();
        let serial = self.serial();
        state.prompts.insert(
            id.clone(),
            Pending {
                serial,
                epoch: epoch.id,
                stream,
                answer,
            },
        );
        PromptStart::Registered {
            serial,
            epoch,
            answers,
        }
    }

    pub(crate) fn forget_prompt(&self, id: &PromptId, serial: Option<u64>) {
        let mut state = self.state();
        if state
            .prompts
            .get(id)
            .is_some_and(|pending| serial.is_none_or(|serial| pending.serial == serial))
        {
            state.prompts.remove(id);
        }
    }

    pub(crate) fn prompt_epoch(&self, id: &PromptId, stream: Option<&RequestId>) -> Option<u64> {
        self.state()
            .prompts
            .get(id)
            .filter(|pending| pending.stream.as_ref() == stream)
            .map(|pending| pending.epoch)
    }

    pub(crate) fn answer(&self, answer: DisplayAnswer) -> bool {
        let mut state = self.state();
        let Some(pending) = state.prompts.get(&answer.id) else {
            return false;
        };
        match &pending.answer {
            Answerer::Open(answers) => {
                if answers.try_send(answer).is_err() {
                    tracing::debug!(
                        "a sign-in answer arrived while two were still unread; dropped"
                    );
                }
            }
            Answerer::Once(_) => {
                if let Some(Pending {
                    answer: Answerer::Once(once),
                    ..
                }) = state.prompts.remove(&answer.id)
                {
                    let _ = once.send(answer);
                }
            }
        }
        true
    }
}

pub(crate) fn once() -> (Answerer, oneshot::Receiver<DisplayAnswer>) {
    let (sender, receiver) = oneshot::channel();
    (Answerer::Once(sender), receiver)
}

pub(crate) fn open() -> (Answerer, mpsc::Receiver<DisplayAnswer>) {
    let (sender, receiver) = mpsc::channel(2);
    (Answerer::Open(sender), receiver)
}

impl State {
    fn deny_where(&mut self, matches: impl Fn(&Held) -> bool, reason: &str) -> usize {
        let ids: Vec<ToolCallId> = self
            .held
            .iter()
            .filter(|(_, held)| matches(held))
            .map(|(id, _)| id.clone())
            .collect();
        for id in &ids {
            if let Some(held) = self.held.remove(id) {
                let _ = held.decide.send(deny(reason));
            }
        }
        ids.len()
    }

    fn dismiss_where(&mut self, matches: impl Fn(&Pending) -> bool) {
        self.dismiss(matches, false);
    }

    fn interrupt_where(&mut self, matches: impl Fn(&Pending) -> bool) {
        self.dismiss(matches, true);
    }

    fn dismiss(&mut self, matches: impl Fn(&Pending) -> bool, keep_open: bool) {
        let ids: Vec<PromptId> = self
            .prompts
            .iter()
            .filter(|(_, pending)| matches(pending))
            .map(|(id, _)| id.clone())
            .collect();
        for id in ids {
            let dismissed = outcome(&id, DisplayOutcome::Dismissed);
            if keep_open
                && let Some(Pending {
                    answer: Answerer::Open(answers),
                    ..
                }) = self.prompts.get(&id)
            {
                let _ = answers.try_send(dismissed);
                continue;
            }
            match self.prompts.remove(&id).map(|pending| pending.answer) {
                Some(Answerer::Once(once)) => {
                    let _ = once.send(dismissed);
                }
                Some(Answerer::Open(answers)) => {
                    let _ = answers.try_send(dismissed);
                }
                None => {}
            }
        }
    }
}
