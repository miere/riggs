use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use rax::content::ContentBlock;
use rax::id::{RequestId, SessionId};
use rax::session::{
    Initialize, Initialized, NewSession, NodeCapabilities, PROTOCOL_VERSION, Prompt,
    PromptAccepted, SessionCreated,
};
use rax::{ErrorKind, GatewayCall, GatewayReply, Open};
use rax_tokio::node::{NodeEvent, NodeEvents, NodeHandle};
use tokio::sync::{OnceCell, mpsc};
use tokio::time::{Instant, Sleep, interval_at, sleep, timeout};
use tokio_util::sync::CancellationToken;
use tokio_util::task::TaskTracker;

use crate::backend::{self, Backend, BackendInfo, HostHandles, SessionKey, TurnHandle};
use crate::files::Files;
use crate::host::push_health_snapshot;
use crate::sessions::{OpenError, Sessions};
use crate::state::{Reserve, Reset, ResetReason, Shared};
use crate::store::{Record, SessionStore, StoreError};
use crate::turn::{self, Failures, Pump, ToolGate, TurnFailed, TurnPrompts};

pub const LINK_GRACE: Duration = Duration::from_secs(120);
pub const CALL_TIMEOUT: Duration = Duration::from_secs(30);
pub const SHUTDOWN_GRACE: Duration = Duration::from_secs(5);
pub const SESSION_RETENTION: Duration = Duration::from_secs(30 * 24 * 60 * 60);
const PRUNE_INTERVAL: Duration = Duration::from_secs(24 * 60 * 60);
const DRAIN_TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Debug, Clone)]
pub enum SessionsConfig {
    Durable { dir: PathBuf, retain: Duration },
    Ephemeral,
}

#[derive(Clone)]
pub struct ServerConfig {
    /// How long held calls, prompts and turns wait for a socket to come back before the link is
    /// treated as ended.
    pub link_grace: Duration,
    pub sessions: SessionsConfig,
    pub turn_events_capacity: usize,
    /// Bounds `sign_in`, `sign_in.settled` and `credential.health`, so a silent gateway cannot
    /// leave a sign-in card drawn that the node gave up on.
    pub call_timeout: Duration,
    pub shutdown_grace: Duration,
    /// How long a prompt waits for a turn cancelled by a link reset to wind down; the new gateway
    /// never heard of that turn, so it cannot be told `session_busy` about it straight away.
    pub drain_timeout: Duration,
    pub turn_failed: Option<TurnFailed>,
    /// Where files the gateway links are saved for the agent to read. Unset means a folder in the
    /// system temp dir, emptied at start like any ephemeral state.
    pub files_dir: Option<PathBuf>,
}

impl ServerConfig {
    pub fn new(sessions: SessionsConfig) -> Self {
        Self {
            link_grace: LINK_GRACE,
            sessions,
            turn_events_capacity: 64,
            call_timeout: CALL_TIMEOUT,
            shutdown_grace: SHUTDOWN_GRACE,
            drain_timeout: DRAIN_TIMEOUT,
            turn_failed: None,
            files_dir: None,
        }
    }
}

/// Why `serve` returned, so the binary knows whether to redial, wait for a new token, or exit.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stopped {
    Shutdown,
    CredentialRejected,
    Refused {
        status: u16,
    },
    /// The gateway sent `close`; a new link may be dialled.
    Closed,
    LinkEnded,
}

#[derive(Debug, thiserror::Error)]
pub enum ServerError {
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// Sessions, the started backend and credential reports outlive any one link, so the same server
/// can `serve` a new link after a rejected token or a gateway `close`.
#[derive(Clone)]
pub struct NodeServer {
    inner: Arc<Inner>,
}

struct Inner {
    backend: Arc<dyn Backend>,
    shared: Arc<Shared>,
    sessions: Sessions,
    files: Files,
    config: ServerConfig,
    failures: Failures,
    info: OnceCell<BackendInfo>,
    shutdown: CancellationToken,
    backend_stopped: AtomicBool,
    tasks: TaskTracker,
}

impl NodeServer {
    pub fn new(backend: Arc<dyn Backend>, config: ServerConfig) -> Result<Self, ServerError> {
        let store = match &config.sessions {
            SessionsConfig::Durable { dir, retain } => Some(SessionStore::open(dir, *retain)?),
            SessionsConfig::Ephemeral => None,
        };
        let files = {
            let dir = config.files_dir.clone().unwrap_or_else(|| {
                std::env::temp_dir().join(format!("riggs-files-{}", uuid::Uuid::new_v4()))
            });
            let ephemeral = matches!(config.sessions, SessionsConfig::Ephemeral);
            Files::new(dir, ephemeral)
        };
        let failures = Failures {
            backend: backend.clone(),
            hook: config.turn_failed.clone(),
        };
        Ok(Self {
            inner: Arc::new(Inner {
                shared: Arc::new(Shared::new(config.call_timeout)),
                sessions: Sessions::new(store),
                files,
                backend,
                config,
                failures,
                info: OnceCell::new(),
                shutdown: CancellationToken::new(),
                backend_stopped: AtomicBool::new(false),
                tasks: TaskTracker::new(),
            }),
        })
    }

    /// Cancelling it makes `serve` cancel turns, deny held calls, stop the agent, release the
    /// session store and close the link, so a new node can open the store as soon as it returns.
    pub fn shutdown_token(&self) -> CancellationToken {
        self.inner.shutdown.clone()
    }

    /// For a shutdown that arrives while no link is being served.
    pub async fn stop(&self) {
        self.inner.shutdown.cancel();
        self.inner.stop().await;
    }

    pub async fn serve(&self, handle: NodeHandle, mut events: NodeEvents) -> Stopped {
        let inner = &self.inner;
        let (closing, mut close_requested) = mpsc::channel::<()>(1);
        let mut grace: Option<Pin<Box<Sleep>>> = None;
        let mut prune = interval_at(Instant::now() + PRUNE_INTERVAL, PRUNE_INTERVAL);
        loop {
            tokio::select! {
                biased;
                () = inner.shutdown.cancelled() => {
                    inner.stop().await;
                    inner.reset(ResetReason::Closed);
                    handle.close().await;
                    return Stopped::Shutdown;
                }
                Some(()) = close_requested.recv() => {
                    inner.reset(ResetReason::Closed);
                    handle.close().await;
                    tracing::info!("gateway connection closed");
                    return Stopped::Closed;
                }
                () = expiry(&mut grace) => {
                    grace = None;
                    inner.reset(ResetReason::GraceExpired);
                }
                _ = prune.tick() => {
                    let pruning = inner.clone();
                    inner.tasks.spawn(async move {
                        pruning.sessions.prune().await;
                        for key in pruning.files.sessions() {
                            if !pruning.sessions.exists(&key).await {
                                pruning.files.discard(&key);
                            }
                        }
                    });
                }
                event = events.recv() => {
                    let Some(event) = event else {
                        inner.reset(ResetReason::LinkEnded);
                        return Stopped::LinkEnded;
                    };
                    match event {
                        NodeEvent::Fresh => {
                            grace = None;
                            let (reset, epoch) = inner.shared.fresh(handle.clone());
                            if let Some(reset) = reset {
                                inner.after_reset(reset, ResetReason::ResumeRefused);
                            }
                            tracing::info!(epoch, "attached to gateway");
                        }
                        NodeEvent::Resumed => {
                            grace = None;
                            inner.resumed();
                        }
                        NodeEvent::Disconnected { reason } => {
                            tracing::info!(%reason, "gateway connection dropped; waiting for it to resume");
                            if grace.is_none() {
                                grace = Some(Box::pin(sleep(inner.config.link_grace)));
                            }
                        }
                        NodeEvent::CredentialRejected => {
                            inner.reset(ResetReason::CredentialRejected);
                            return Stopped::CredentialRejected;
                        }
                        NodeEvent::Refused { status } => {
                            inner.reset(ResetReason::LinkEnded);
                            return Stopped::Refused { status };
                        }
                        NodeEvent::Request { id, call } => {
                            let epoch = inner.shared.live().map(|epoch| epoch.id);
                            let request = inner.clone().request(handle.clone(), epoch, id, call, closing.clone());
                            inner.tasks.spawn(request);
                        }
                        NodeEvent::Verdict(verdict) => {
                            let id = verdict.id.clone();
                            if !inner.shared.verdict(verdict) {
                                tracing::debug!(tool_call_id = %id, "verdict for a tool call nobody is holding");
                            }
                        }
                        NodeEvent::Answer(answer) => {
                            let id = answer.id.clone();
                            if !inner.shared.answer(answer) {
                                tracing::debug!(prompt = %id, "answer for a prompt nobody is waiting on");
                            }
                        }
                        NodeEvent::Unhandled { stream, body } => {
                            tracing::warn!(stream = ?stream, subject = ?body.subject, reason = ?body.reason, "the gateway could not handle something this node sent");
                        }
                    }
                }
            }
        }
    }
}

async fn expiry(grace: &mut Option<Pin<Box<Sleep>>>) {
    match grace {
        Some(sleeping) => sleeping.await,
        None => std::future::pending().await,
    }
}

fn unknown_session(session_id: &SessionId) -> rax::Error {
    rax::Error::new(
        ErrorKind::UnknownSession,
        format!("this node has no session {session_id}"),
    )
}

fn prompt_text(content: &[Open<ContentBlock>]) -> String {
    content
        .iter()
        .filter_map(|block| match block {
            Open::Known(ContentBlock::Text { text }) => Some(text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

impl Inner {
    async fn started(&self) -> Result<BackendInfo, rax::Error> {
        self.info
            .get_or_try_init(|| async {
                let host = HostHandles::new(self.shared.clone());
                let info = self.backend.start(host).await.map_err(|err| {
                    rax::Error::new(
                        self.backend.classify(&err),
                        format!("the agent could not start: {err}"),
                    )
                })?;
                tracing::info!(agent = %info.name, interruptible = info.interruptible, "node serving one agent");
                Ok(info)
            })
            .await
            .cloned()
    }

    fn reset(self: &Arc<Self>, reason: ResetReason) {
        if let Some(reset) = self.shared.end_epoch(reason) {
            self.after_reset(reset, reason);
        }
    }

    fn after_reset(self: &Arc<Self>, reset: Reset, reason: ResetReason) {
        let turns_failed = reset.turns.len();
        if turns_failed > 0 || reset.calls_denied > 0 || reason != ResetReason::Closed {
            tracing::warn!(old_epoch = reset.old_epoch, reason = ?reason, turns_failed, calls_denied = reset.calls_denied, "link reset");
        }
        for key in reset.turns {
            let inner = self.clone();
            self.tasks
                .spawn(async move { inner.cancel_at_backend(&key).await });
        }
    }

    fn resumed(self: &Arc<Self>) {
        let Some((handle, orphans)) = self.shared.resumed() else {
            tracing::info!("link resumed");
            return;
        };
        tracing::info!(
            ended = orphans.len(),
            "link resumed after its grace ran out"
        );
        self.tasks.spawn(async move {
            for stream in orphans {
                if let Err(err) = handle.end(stream).await {
                    tracing::debug!(error = %err, "could not end a stream the node gave up on");
                }
            }
        });
    }

    async fn cancel_at_backend(&self, key: &SessionKey) {
        if let Err(err) = self.backend.cancel(key).await {
            tracing::warn!(session_id = %key, error = %err, "the agent could not cancel its turn");
        }
    }

    async fn stop(self: &Arc<Self>) {
        let (turns, calls_denied) = self.shared.stop();
        tracing::info!(turns = turns.len(), calls_denied, "shutting down");
        for key in turns {
            let inner = self.clone();
            self.tasks
                .spawn(async move { inner.cancel_at_backend(&key).await });
        }
        self.tasks.close();
        let grace = self.config.shutdown_grace;
        if timeout(grace, self.tasks.wait()).await.is_err() {
            tracing::warn!("turns were still running when the shutdown grace ran out");
        }
        if self.info.get().is_some()
            && !self.backend_stopped.swap(true, Ordering::SeqCst)
            && timeout(grace, self.backend.shutdown()).await.is_err()
        {
            tracing::warn!("the agent did not stop within the shutdown grace");
        }
        self.sessions.release_store().await;
    }

    async fn request(
        self: Arc<Self>,
        handle: NodeHandle,
        epoch: Option<u64>,
        id: RequestId,
        call: GatewayCall,
        closing: mpsc::Sender<()>,
    ) {
        let Some(epoch) = epoch else {
            return;
        };
        let answer = match call {
            GatewayCall::Initialize(offer) => self
                .initialize(epoch, offer)
                .await
                .map(GatewayReply::Initialize),
            GatewayCall::NewSession(request) => self
                .new_session(&handle, epoch, request)
                .await
                .map(GatewayReply::NewSession),
            GatewayCall::Prompt(prompt) => return self.prompt(handle, epoch, id, prompt).await,
            GatewayCall::Cancel(session) => {
                self.cancel(&session.session_id).await;
                Ok(GatewayReply::Cancel)
            }
            GatewayCall::CloseSession(session) => {
                self.close_session(&session.session_id).await;
                Ok(GatewayReply::CloseSession)
            }
            GatewayCall::RenewCredential => Ok(GatewayReply::RenewCredential(
                self.backend.renew_credential().await,
            )),
            GatewayCall::Close => {
                self.answer(&handle, epoch, id, Ok(GatewayReply::Close))
                    .await;
                let _ = closing.send(()).await;
                return;
            }
        };
        let initialized = matches!(answer, Ok(GatewayReply::Initialize(_)));
        self.answer(&handle, epoch, id, answer).await;
        if initialized {
            push_health_snapshot(&self.shared, epoch).await;
        }
    }

    async fn answer(
        &self,
        handle: &NodeHandle,
        epoch: u64,
        id: RequestId,
        answer: Result<GatewayReply, rax::Error>,
    ) {
        if !self.shared.is_live(epoch) {
            tracing::debug!(%id, "dropping an answer for a link that has ended");
            return;
        }
        let sent = match answer {
            Ok(reply) => handle.reply(id.clone(), reply).await,
            Err(error) => {
                tracing::warn!(%id, kind = ?error.kind, error = %error, "request failed");
                handle.fault(id.clone(), error).await
            }
        };
        if let Err(err) = sent {
            tracing::debug!(%id, error = %err, "could not answer a request");
        }
    }

    async fn initialize(&self, epoch: u64, offer: Initialize) -> Result<Initialized, rax::Error> {
        let info = self.started().await?;
        let mut resource_schemes = info.resource_schemes.clone();
        for scheme in &offer.capabilities.readable_schemes {
            if !resource_schemes.contains(scheme) {
                resource_schemes.push(scheme.clone());
            }
        }
        self.shared.set_caps(epoch, offer.capabilities);
        Ok(Initialized {
            protocol_version: PROTOCOL_VERSION,
            capabilities: NodeCapabilities {
                interruptible: Some(info.interruptible),
                prompt: info.prompt.clone(),
                resource_schemes,
                tool_gate: info.tool_gate,
                sessions: self.sessions.durability(&info),
            },
        })
    }

    fn readable_schemes(&self, epoch: u64) -> Vec<String> {
        self.shared
            .live()
            .filter(|live| live.id == epoch)
            .and_then(|live| live.caps)
            .map(|caps| caps.readable_schemes)
            .unwrap_or_default()
    }

    async fn new_session(
        &self,
        handle: &NodeHandle,
        epoch: u64,
        request: NewSession,
    ) -> Result<SessionCreated, rax::Error> {
        let info = self.started().await?;
        let key = SessionKey::mint();
        let readable = self.readable_schemes(epoch);
        let fetched = self
            .files
            .fetch(
                handle,
                &readable,
                &key,
                request.context,
                &CancellationToken::new(),
            )
            .await;
        let opening = backend::NewSession {
            key: &key,
            context: &fetched.content,
        };
        let opened = self
            .backend
            .new_session(opening)
            .await
            .map_err(|err| rax::Error::new(self.backend.classify(&err), err.to_string()))?;
        let record = Record::new(&key, info.name.clone(), opened.record, fetched.content);
        if let Err(err) = self.sessions.create(key, record, &info).await {
            self.backend.close_session(&key).await;
            self.files.discard(&key);
            return Err(rax::Error::new(
                ErrorKind::Unknown,
                format!("this node could not save the new session: {err}"),
            ));
        }
        tracing::debug!(session_id = %key, "session created");
        let mut unhandled = fetched.unhandled;
        unhandled.extend(opened.unhandled);
        Ok(SessionCreated {
            session_id: key.session_id(),
            unhandled,
        })
    }

    async fn prompt(
        self: Arc<Self>,
        handle: NodeHandle,
        epoch: u64,
        id: RequestId,
        prompt: Prompt,
    ) {
        let Prompt {
            session_id,
            content,
        } = prompt;
        let Some(key) = SessionKey::parse(&session_id) else {
            let refusal = Err(unknown_session(&session_id));
            return self.answer(&handle, epoch, id, refusal).await;
        };
        let info = match self.started().await {
            Ok(info) => info,
            Err(error) => return self.answer(&handle, epoch, id, Err(error)).await,
        };
        let turn = loop {
            match self.shared.reserve_turn(&key, &id, epoch) {
                Reserve::Reserved(turn) => break turn,
                Reserve::Stale => return,
                Reserve::Busy => {
                    let busy = rax::Error::new(
                        ErrorKind::SessionBusy,
                        format!("session {key} already has a turn open; cancel it first"),
                    );
                    return self.answer(&handle, epoch, id, Err(busy)).await;
                }
                Reserve::Draining(finished) => {
                    if timeout(self.config.drain_timeout, finished.cancelled())
                        .await
                        .is_err()
                    {
                        let busy = rax::Error::new(
                            ErrorKind::SessionBusy,
                            format!("session {key} is still stopping a turn from an earlier link"),
                        );
                        return self.answer(&handle, epoch, id, Err(busy)).await;
                    }
                }
            }
        };
        let slot = match self.sessions.open(key, &*self.backend, &info).await {
            Ok(slot) => slot,
            Err(err) => {
                self.shared.finish_turn(&key, &id);
                let error = match err {
                    OpenError::Unknown => unknown_session(&session_id),
                    OpenError::Backend(err) => self.failures.map(err).await,
                    OpenError::Store(err) => rax::Error::new(ErrorKind::Unknown, err.to_string()),
                };
                return self.answer(&handle, epoch, id, Err(error)).await;
            }
        };
        tracing::debug!(session_id = %key, stream = %id, text = %prompt_text(&content), "turn started");
        let (sender, items) = turn::channel(self.config.turn_events_capacity);
        let turn_handle = TurnHandle {
            stream: id.clone(),
            events: turn::sink(sender.clone()),
            cancelled: turn.cancel.clone(),
            gate: ToolGate {
                shared: self.shared.clone(),
                session: key,
                stream: id.clone(),
                outbox: sender.downgrade(),
            },
            prompts: TurnPrompts {
                shared: self.shared.clone(),
                session: key,
                stream: id.clone(),
                outbox: sender.downgrade(),
            },
        };
        drop(sender);
        let readable = self.readable_schemes(epoch);
        let fetched = self
            .files
            .fetch(&handle, &readable, &key, content, &turn.cancel)
            .await;
        let accepted = self
            .backend
            .prompt(&key, fetched.content, turn_handle)
            .await;
        drop(slot);
        let unhandled = match accepted {
            Ok(mut unhandled) => {
                unhandled.extend(fetched.unhandled);
                unhandled
            }
            Err(err) => {
                drop(items);
                self.shared.finish_turn(&key, &id);
                let error = self.failures.map(err).await;
                return self.answer(&handle, epoch, id, Err(error)).await;
            }
        };
        let reply = GatewayReply::Prompt(PromptAccepted { unhandled });
        let healthy = self.shared.is_live(epoch)
            && tokio::select! {
                sent = handle.reply(id.clone(), reply) => sent.is_ok(),
                () = turn.orphaned.cancelled() => false,
            };
        let pump = Pump {
            shared: self.shared.clone(),
            handle,
            key,
            turn,
            failures: self.failures.clone(),
        };
        pump.run(items, healthy).await;
        tracing::debug!(session_id = %key, stream = %id, "turn ended");
        self.sessions.touch(key, &info).await;
    }

    async fn cancel(&self, session_id: &SessionId) {
        let Some(key) = SessionKey::parse(session_id) else {
            return;
        };
        if self.shared.cancel_turn(&key) || self.sessions.is_known(&key) {
            self.cancel_at_backend(&key).await;
        }
    }

    async fn close_session(&self, session_id: &SessionId) {
        let Some(key) = SessionKey::parse(session_id) else {
            return;
        };
        if self.shared.cancel_turn(&key) {
            self.cancel_at_backend(&key).await;
        }
        self.sessions.close(key, &*self.backend).await;
        self.files.discard(&key);
        tracing::debug!(session_id = %key, "session closed");
    }
}
