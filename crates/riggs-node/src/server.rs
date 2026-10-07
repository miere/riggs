use std::collections::HashMap;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use rax::content::ContentBlock;
use rax::id::{RequestId, SessionId};
use rax::open::{Subject, UnhandledReason};
use rax::session::{
    Initialize, Initialized, NewSession, NodeCapabilities, PROTOCOL_VERSION, Prompt,
    PromptAccepted, SessionCreated,
};
use rax::tool::{MAX_NAMESPACE_LEN, ToolGroup};
use rax::{ErrorKind, GatewayCall, GatewayReply, Metadata, Open, Rejection, Unhandled};
use rax_tokio::node::{NodeEvent, NodeEvents, NodeHandle};
use tokio::sync::{OnceCell, mpsc};
use tokio::time::{Instant, Sleep, interval_at, sleep, timeout};
use tokio_util::sync::CancellationToken;
use tokio_util::task::TaskTracker;

use crate::backend::{self, Backend, BackendInfo, HostHandles, SessionKey, TurnHandle};
use crate::files::Files;
use crate::host::{push_health_snapshot, push_metadata};
use crate::sessions::{OpenError, Sessions, Slot};
use crate::state::{GatewayName, Reserve, Reset, ResetReason, Shared, lock};
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
    /// Forwarded to every gateway unread; [`NodeServer::set_metadata`] replaces it later.
    pub metadata: Metadata,
    /// Per gateway, merged over `metadata` for that gateway only.
    pub gateway_metadata: HashMap<GatewayName, Metadata>,
    /// The gateway that receives sign-ins raised outside any session, and owns session records
    /// written before sessions recorded their gateway.
    pub primary: GatewayName,
}

/// The name a node with a single, unnamed gateway gives it.
pub const DEFAULT_GATEWAY: &str = "default";

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
            metadata: Metadata::new(),
            gateway_metadata: HashMap::new(),
            primary: DEFAULT_GATEWAY.to_owned(),
        }
    }
}

/// Why `serve` returned for one gateway, so the binary knows whether to redial, wait for a new
/// token, or give up on that gateway.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Stopped {
    Shutdown,
    CredentialRejected,
    Refused {
        status: u16,
    },
    /// The gateway sent `close`; a new link may be dialled.
    Closed,
    /// The gateway refused the metadata and will again until its owner changes it.
    Rejected(Rejection),
    LinkEnded,
}

#[derive(Debug, thiserror::Error)]
pub enum ServerError {
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// Sessions, the started backend and credential reports outlive any one link, so the same server
/// can `serve` a new link after a rejected token or a gateway `close`, and serve one link per
/// gateway at once.
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
        let shared = Arc::new(Shared::new(config.call_timeout, config.primary.clone()));
        *lock(&shared.metadata) = config.metadata.clone();
        *lock(&shared.gateway_metadata) = config.gateway_metadata.clone();
        Ok(Self {
            inner: Arc::new(Inner {
                shared,
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

    /// The owner changed the metadata: every attached gateway hears it now, and every later
    /// `initialize` declares it, so no restart is needed.
    pub fn set_metadata(&self, metadata: Metadata) {
        let per_gateway = lock(&self.inner.shared.gateway_metadata).clone();
        self.set_all_metadata(metadata, per_gateway);
    }

    /// Replaces the top-level table and every gateway's own at once, so no gateway hears a mix.
    pub fn set_all_metadata(
        &self,
        metadata: Metadata,
        per_gateway: HashMap<GatewayName, Metadata>,
    ) {
        *lock(&self.inner.shared.metadata) = metadata;
        *lock(&self.inner.shared.gateway_metadata) = per_gateway;
        let shared = self.inner.shared.clone();
        self.inner
            .tasks
            .spawn(async move { push_metadata(&shared).await });
    }

    /// For a shutdown that arrives while no link is being served.
    pub async fn stop(&self) {
        self.inner.shutdown.cancel();
        self.inner.stop().await;
    }

    /// Serves the primary gateway's link.
    pub async fn serve(&self, handle: NodeHandle, events: NodeEvents) -> Stopped {
        let primary = self.inner.config.primary.clone();
        self.serve_gateway(&primary, handle, events).await
    }

    /// Serves one gateway's link. Call it once per gateway, concurrently: each link resets,
    /// expires and stops on its own, and only shutdown is node-wide.
    pub async fn serve_gateway(
        &self,
        gateway: &str,
        handle: NodeHandle,
        mut events: NodeEvents,
    ) -> Stopped {
        let inner = &self.inner;
        let (closing, mut close_requested) = mpsc::channel::<()>(1);
        let mut grace: Option<Pin<Box<Sleep>>> = None;
        let mut prune = interval_at(Instant::now() + PRUNE_INTERVAL, PRUNE_INTERVAL);
        loop {
            tokio::select! {
                biased;
                () = inner.shutdown.cancelled() => {
                    inner.stop().await;
                    inner.reset(gateway, ResetReason::Closed);
                    handle.close().await;
                    return Stopped::Shutdown;
                }
                Some(()) = close_requested.recv() => {
                    inner.reset(gateway, ResetReason::Closed);
                    handle.close().await;
                    tracing::info!(%gateway, "gateway connection closed");
                    return Stopped::Closed;
                }
                () = expiry(&mut grace) => {
                    grace = None;
                    inner.reset(gateway, ResetReason::GraceExpired);
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
                        inner.reset(gateway, ResetReason::LinkEnded);
                        return Stopped::LinkEnded;
                    };
                    match event {
                        NodeEvent::Fresh => {
                            grace = None;
                            let (reset, epoch) = inner.shared.fresh(gateway, handle.clone());
                            if let Some(reset) = reset {
                                inner.after_reset(reset, ResetReason::ResumeRefused);
                            }
                            tracing::info!(%gateway, epoch, "attached to gateway");
                        }
                        NodeEvent::Resumed => {
                            grace = None;
                            inner.resumed(gateway);
                        }
                        NodeEvent::Disconnected { reason } => {
                            tracing::info!(%gateway, %reason, "gateway connection dropped; waiting for it to resume");
                            if grace.is_none() {
                                grace = Some(Box::pin(sleep(inner.config.link_grace)));
                            }
                        }
                        NodeEvent::CredentialRejected => {
                            inner.reset(gateway, ResetReason::CredentialRejected);
                            return Stopped::CredentialRejected;
                        }
                        NodeEvent::Refused { status } => {
                            inner.reset(gateway, ResetReason::LinkEnded);
                            return Stopped::Refused { status };
                        }
                        NodeEvent::Rejected(rejection) => {
                            inner.reset(gateway, ResetReason::LinkEnded);
                            return Stopped::Rejected(rejection);
                        }
                        NodeEvent::Unhandled { body: rax::Unhandled { subject: Subject::MetadataKey { key }, message, .. }, .. } => {
                            tracing::warn!(%gateway, %key, reason = ?message, "the gateway ignores this metadata key");
                        }
                        NodeEvent::Request { id, call } => {
                            let epoch = inner.shared.live(gateway).map(|epoch| epoch.id);
                            let request = inner.clone().request(gateway.to_owned(), handle.clone(), epoch, id, call, closing.clone());
                            inner.tasks.spawn(request);
                        }
                        NodeEvent::Verdict(verdict) => {
                            let id = verdict.id.clone();
                            if !inner.shared.verdict(gateway, verdict) {
                                tracing::debug!(%gateway, tool_call_id = %id, "verdict for a tool call this link is not holding");
                            }
                        }
                        NodeEvent::Answer(answer) => {
                            let id = answer.id.clone();
                            if !inner.shared.answer(gateway, answer) {
                                tracing::debug!(%gateway, prompt = %id, "answer for a prompt this link did not show");
                            }
                        }
                        NodeEvent::Receipt(receipt) => {
                            let id = receipt.transfer_id.clone();
                            if !inner.shared.receipt(gateway, receipt) {
                                tracing::debug!(%gateway, transfer_id = %id, "receipt for an attachment nobody is waiting on");
                            }
                        }
                        NodeEvent::Unhandled { stream, body } => {
                            tracing::warn!(%gateway, stream = ?stream, subject = ?body.subject, reason = ?body.reason, "the gateway could not handle something this node sent");
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

    fn reset(self: &Arc<Self>, gateway: &str, reason: ResetReason) {
        if let Some(reset) = self.shared.end_epoch(gateway, reason) {
            self.after_reset(reset, reason);
        }
    }

    fn after_reset(self: &Arc<Self>, reset: Reset, reason: ResetReason) {
        let turns_failed = reset.turns.len();
        if turns_failed > 0 || reset.calls_denied > 0 || reason != ResetReason::Closed {
            tracing::warn!(gateway = %reset.gateway, old_epoch = reset.old_epoch, reason = ?reason, turns_failed, calls_denied = reset.calls_denied, "link reset");
        }
        for key in reset.turns {
            let inner = self.clone();
            self.tasks
                .spawn(async move { inner.cancel_at_backend(&key).await });
        }
    }

    fn resumed(self: &Arc<Self>, gateway: &str) {
        let Some((handle, orphans)) = self.shared.resumed(gateway) else {
            tracing::info!(%gateway, "link resumed");
            return;
        };
        tracing::info!(
            %gateway,
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
        gateway: GatewayName,
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
                .initialize(&gateway, epoch, offer)
                .await
                .map(GatewayReply::Initialize),
            GatewayCall::NewSession(request) => self
                .new_session(&gateway, &handle, epoch, request)
                .await
                .map(GatewayReply::NewSession),
            GatewayCall::Prompt(prompt) => {
                return self.prompt(&gateway, handle, epoch, id, prompt).await;
            }
            GatewayCall::Cancel(session) => self
                .cancel(&gateway, &session.session_id)
                .await
                .map(|()| GatewayReply::Cancel),
            GatewayCall::CloseSession(session) => self
                .close_session(&gateway, &session.session_id)
                .await
                .map(|()| GatewayReply::CloseSession),
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

    async fn initialize(
        &self,
        gateway: &str,
        epoch: u64,
        offer: Initialize,
    ) -> Result<Initialized, rax::Error> {
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
                tool_groups: true,
                attachment_receipts: true,
            },
            metadata: self.shared.metadata_for(gateway),
        })
    }

    fn readable_schemes(&self, epoch: u64) -> Vec<String> {
        self.shared
            .epoch(epoch)
            .and_then(|live| live.caps)
            .map(|caps| caps.readable_schemes)
            .unwrap_or_default()
    }

    async fn new_session(
        &self,
        gateway: &str,
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
        let (tool_groups, refused) = admit_groups(request.tool_groups);
        let opening = backend::NewSession {
            key: &key,
            context: &fetched.content,
            tool_groups: &tool_groups,
        };
        let opened = self
            .backend
            .new_session(opening)
            .await
            .map_err(|err| rax::Error::new(self.backend.classify(&err), err.to_string()))?;
        let record = Record::new(
            &key,
            gateway.to_owned(),
            info.name.clone(),
            opened.record,
            fetched.content,
        );
        self.shared.set_owner(key, gateway.to_owned());
        if let Err(err) = self.sessions.create(key, record, &info).await {
            self.shared.forget_owner(&key);
            self.backend.close_session(&key).await;
            self.files.discard(&key);
            return Err(rax::Error::new(
                ErrorKind::Unknown,
                format!("this node could not save the new session: {err}"),
            ));
        }
        tracing::debug!(%gateway, session_id = %key, "session created");
        let mut unhandled = fetched.unhandled;
        unhandled.extend(refused);
        unhandled.extend(opened.unhandled);
        Ok(SessionCreated {
            session_id: key.session_id(),
            unhandled,
        })
    }

    async fn prompt(
        self: Arc<Self>,
        gateway: &str,
        handle: NodeHandle,
        epoch: u64,
        id: RequestId,
        prompt: Prompt,
    ) {
        let Prompt {
            session_id,
            content,
        } = prompt;
        let Some(key) = self.owned(gateway, &session_id).await else {
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
            Ok(mut slot) => {
                // A record from before sessions named their gateway belongs to the primary, and
                // says so from its next save on.
                if let Slot::Live(record) = &mut *slot {
                    let owner = record
                        .gateway
                        .get_or_insert_with(|| self.config.primary.clone());
                    self.shared.set_owner(key, owner.clone());
                }
                slot
            }
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

    /// The session's key, if `gateway` may drive it: the gateway that opened it, or any gateway
    /// for an id this node never heard of, which then fails as it always did. Another gateway's
    /// session is as unknown to this one as a session that never existed.
    async fn owned(&self, gateway: &str, session_id: &SessionId) -> Option<SessionKey> {
        let key = SessionKey::parse(session_id)?;
        let owner = match self.shared.owner(&key) {
            Some(owner) => Some(owner),
            None => self
                .sessions
                .recorded_owner(&key)
                .await
                .map(|owner| owner.unwrap_or_else(|| self.config.primary.clone())),
        };
        match owner {
            Some(owner) if owner != gateway => {
                tracing::warn!(%gateway, %owner, session_id = %key, "refused a call for another gateway's session");
                None
            }
            _ => Some(key),
        }
    }

    /// Succeeds for a session this node does not have, as the protocol asks, but not for one
    /// another gateway opened.
    async fn cancel(&self, gateway: &str, session_id: &SessionId) -> Result<(), rax::Error> {
        if SessionKey::parse(session_id).is_none() {
            return Ok(());
        }
        let key = self
            .owned(gateway, session_id)
            .await
            .ok_or_else(|| unknown_session(session_id))?;
        if self.shared.cancel_turn(&key) || self.sessions.is_known(&key) {
            self.cancel_at_backend(&key).await;
        }
        Ok(())
    }

    async fn close_session(&self, gateway: &str, session_id: &SessionId) -> Result<(), rax::Error> {
        if SessionKey::parse(session_id).is_none() {
            return Ok(());
        }
        let key = self
            .owned(gateway, session_id)
            .await
            .ok_or_else(|| unknown_session(session_id))?;
        if self.shared.cancel_turn(&key) {
            self.cancel_at_backend(&key).await;
        }
        self.sessions.close(key, &*self.backend).await;
        self.shared.forget_owner(&key);
        self.files.discard(&key);
        tracing::debug!(%gateway, session_id = %key, "session closed");
        Ok(())
    }
}

/// Keeps the groups a backend could publish as they are. A namespace that would not survive as a
/// tool name, or a second group under one already taken, is refused here rather than by every
/// backend, and the session opens without it.
fn admit_groups(groups: Vec<ToolGroup>) -> (Vec<ToolGroup>, Vec<Unhandled>) {
    let mut admitted: Vec<ToolGroup> = Vec::with_capacity(groups.len());
    let mut refused = Vec::new();
    for group in groups {
        let problem = if !ToolGroup::is_valid_namespace(&group.namespace) {
            Some(format!(
                "`{}` is not a namespace this node can publish: it must be [a-z0-9_] and at most {MAX_NAMESPACE_LEN} characters",
                group.namespace
            ))
        } else if admitted
            .iter()
            .any(|kept| kept.namespace == group.namespace)
        {
            Some(format!(
                "the session was already given a group named `{}`",
                group.namespace
            ))
        } else {
            None
        };
        match problem {
            Some(message) => refused.push(Unhandled {
                subject: Subject::ToolGroup {
                    namespace: group.namespace,
                },
                reason: UnhandledReason::Other,
                message: Some(message),
            }),
            None => admitted.push(group),
        }
    }
    (admitted, refused)
}
