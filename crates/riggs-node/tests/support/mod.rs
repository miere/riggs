#![allow(dead_code, clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::future::Future;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use rax::content::ContentBlock;
use rax::credential::{CredentialHealth, CredentialRenewal};
use rax::id::SessionId;
use rax::open::{Subject, UnhandledReason};
use rax::session::{
    GatewayCapabilities, Initialize, NewSession as NewSessionCall, Prompt, PromptCapabilities,
    SessionDurability, SessionRef, ToolGate as GateMode,
};
use rax::{ErrorKind, Event, GatewayCall, GatewayReply, Open, Unhandled};
use rax_tokio::CallError;
use rax_tokio::gateway::{
    GatewayConfig, GatewayLink, GatewayServer, LinkEvent, LinkEvents, NewLink, NewLinks,
    NodeIdentity, PendingCall, StreamEvents,
};
use rax_tokio::node::{NodeConfig, NodeHandle, NodeLink};
use riggs_node::{
    Backend, BackendError, BackendInfo, BackendRecord, HostHandles, NewSession, NodeServer, Opened,
    Restore, ServerConfig, SessionKey, SessionsConfig, Stopped, TurnHandle,
};
use serde_json::json;
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::{Notify, mpsc, oneshot, watch};
use tokio::task::JoinHandle;

pub const TOKEN: &str = "node-token";

pub async fn within<F: Future>(future: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(30), future)
        .await
        .expect("test guard timed out")
}

pub async fn quiet<F: Future>(future: F) -> bool {
    tokio::time::timeout(Duration::from_millis(300), future)
        .await
        .is_err()
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Seen {
    Started,
    NewSession(SessionKey),
    Restore(SessionKey),
    Prompt(SessionKey, String),
    Cancel(SessionKey),
    Close(SessionKey),
    Shutdown,
}

pub struct Turn {
    pub key: SessionKey,
    pub text: String,
    pub handle: TurnHandle,
}

pub type Script = Arc<dyn Fn(Turn) -> Pin<Box<dyn Future<Output = ()> + Send>> + Send + Sync>;

pub fn script<F, Fut>(f: F) -> Script
where
    F: Fn(Turn) -> Fut + Send + Sync + 'static,
    Fut: Future<Output = ()> + Send + 'static,
{
    Arc::new(move |turn| Box::pin(f(turn)))
}

#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct Cancelled(pub String);

pub struct Fake {
    pub info: BackendInfo,
    seen: mpsc::UnboundedSender<Seen>,
    seen_rx: Mutex<Option<mpsc::UnboundedReceiver<Seen>>>,
    host: watch::Sender<Option<HostHandles>>,
    script: Mutex<Script>,
    restore: Mutex<bool>,
    block_prompt: Mutex<bool>,
    health: Mutex<Option<CredentialHealth>>,
    cancelled: Notify,
}

impl Fake {
    pub fn new() -> Arc<Self> {
        let (seen, seen_rx) = mpsc::unbounded_channel();
        Arc::new(Self {
            info: BackendInfo {
                name: "fake".into(),
                interruptible: true,
                tool_gate: GateMode::EveryCall,
                sessions: SessionDurability::Durable,
                prompt: PromptCapabilities {
                    image: true,
                    audio: false,
                    embedded_resource: false,
                },
                resource_schemes: vec!["chat".into()],
            },
            seen,
            seen_rx: Mutex::new(Some(seen_rx)),
            host: watch::channel(None).0,
            script: Mutex::new(script(|turn: Turn| async move {
                complete(&turn).await;
            })),
            restore: Mutex::new(true),
            block_prompt: Mutex::new(false),
            health: Mutex::new(None),
            cancelled: Notify::new(),
        })
    }

    pub fn seen(&self) -> Watcher {
        Watcher {
            receiver: self
                .seen_rx
                .lock()
                .unwrap()
                .take()
                .expect("seen taken twice"),
        }
    }

    pub fn on_prompt(&self, script: Script) {
        *self.script.lock().unwrap() = script;
    }

    pub fn restores(&self, restores: bool) {
        *self.restore.lock().unwrap() = restores;
    }

    pub fn block_prompt_until_cancel(&self) {
        *self.block_prompt.lock().unwrap() = true;
    }

    /// Reported from `start`, as a backend that checks its credential when it comes up does.
    pub fn reports_health(&self, credential: &str) {
        *self.health.lock().unwrap() = Some(CredentialHealth {
            credential: credential.to_owned(),
            degraded: false,
            reason: None,
            since: None,
            expires_at: None,
        });
    }

    pub async fn host(&self) -> HostHandles {
        let mut host = self.host.subscribe();
        within(host.wait_for(Option::is_some))
            .await
            .unwrap()
            .clone()
            .unwrap()
    }
}

pub struct Watcher {
    receiver: mpsc::UnboundedReceiver<Seen>,
}

impl Watcher {
    pub async fn wait_for(&mut self, wanted: impl Fn(&Seen) -> bool) -> Seen {
        loop {
            let seen = within(self.receiver.recv()).await.expect("fake dropped");
            if wanted(&seen) {
                return seen;
            }
        }
    }
}

pub async fn complete(turn: &Turn) {
    let stop = Some(rax::event::StopReason::EndTurn);
    let _ = turn
        .handle
        .events
        .send(riggs_node::BackendEvent::Complete(stop))
        .await;
}

#[async_trait]
impl Backend for Fake {
    async fn start(&self, host: HostHandles) -> Result<BackendInfo, BackendError> {
        self.host.send_replace(Some(host.clone()));
        let _ = self.seen.send(Seen::Started);
        let health = self.health.lock().unwrap().clone();
        if let Some(health) = health {
            host.credentials.report(health).await;
        }
        Ok(self.info.clone())
    }

    async fn new_session(&self, request: NewSession<'_>) -> Result<Opened, BackendError> {
        let _ = self.seen.send(Seen::NewSession(*request.key));
        let unhandled = request
            .context
            .iter()
            .enumerate()
            .filter(|(_, block)| {
                matches!(block, Open::Known(ContentBlock::ResourceLink { uri, .. }) if uri.starts_with("file:"))
            })
            .map(|(index, _)| Unhandled {
                subject: Subject::Block { index },
                reason: UnhandledReason::UnsupportedScheme,
                message: None,
            })
            .collect();
        Ok(Opened {
            record: BackendRecord(json!({"conversation": request.key.to_string()})),
            unhandled,
        })
    }

    async fn restore_session(
        &self,
        key: &SessionKey,
        record: &BackendRecord,
    ) -> Result<Restore, BackendError> {
        assert_eq!(record.0, json!({"conversation": key.to_string()}));
        let _ = self.seen.send(Seen::Restore(*key));
        Ok(if *self.restore.lock().unwrap() {
            Restore::Restored
        } else {
            Restore::Gone
        })
    }

    async fn prompt(
        &self,
        key: &SessionKey,
        content: Vec<Open<ContentBlock>>,
        turn: TurnHandle,
    ) -> Result<Vec<rax::Unhandled>, BackendError> {
        let text = content
            .iter()
            .filter_map(|block| match block {
                Open::Known(ContentBlock::Text { text }) => Some(text.clone()),
                _ => None,
            })
            .collect::<Vec<_>>()
            .join("\n");
        let _ = self.seen.send(Seen::Prompt(*key, text.clone()));
        let block = *self.block_prompt.lock().unwrap();
        if block {
            self.cancelled.notified().await;
            return Err(BackendError::new(Cancelled(
                "the turn was cancelled before it started".into(),
            )));
        }
        let script = self.script.lock().unwrap().clone();
        tokio::spawn(script(Turn {
            key: *key,
            text,
            handle: turn,
        }));
        Ok(Vec::new())
    }

    async fn cancel(&self, key: &SessionKey) -> Result<(), BackendError> {
        let _ = self.seen.send(Seen::Cancel(*key));
        self.cancelled.notify_one();
        Ok(())
    }

    async fn close_session(&self, key: &SessionKey) {
        let _ = self.seen.send(Seen::Close(*key));
    }

    async fn renew_credential(&self) -> CredentialRenewal {
        CredentialRenewal::Started
    }

    fn classify(&self, err: &BackendError) -> ErrorKind {
        if err.downcast_ref::<Cancelled>().is_some() {
            ErrorKind::Cancelled
        } else {
            ErrorKind::Unknown
        }
    }

    async fn shutdown(&self) {
        let _ = self.seen.send(Seen::Shutdown);
    }
}

pub fn gateway_config() -> GatewayConfig {
    GatewayConfig {
        keepalive: Duration::from_secs(1),
        handshake_timeout: Duration::from_secs(5),
        ..Default::default()
    }
}

pub async fn gateway(config: GatewayConfig) -> (GatewayServer, NewLinks) {
    let authenticate = |token: &str| (token == TOKEN).then(|| NodeIdentity("node-1".to_owned()));
    GatewayServer::bind("127.0.0.1:0", authenticate, config)
        .await
        .unwrap()
}

pub fn node_config(addr: SocketAddr) -> NodeConfig {
    NodeConfig {
        endpoints: vec![format!("ws://{addr}")],
        token: TOKEN.to_owned(),
        keepalive: Duration::from_secs(1),
        backoff_min: Duration::from_millis(5),
        backoff_max: Duration::from_millis(50),
        handshake_timeout: Duration::from_secs(5),
        ..Default::default()
    }
}

pub fn ephemeral() -> ServerConfig {
    ServerConfig::new(SessionsConfig::Ephemeral)
}

pub fn durable(dir: &std::path::Path) -> ServerConfig {
    ServerConfig::new(SessionsConfig::Durable {
        dir: dir.to_path_buf(),
        retain: riggs_node::SESSION_RETENTION,
    })
}

pub struct Node {
    pub server: NodeServer,
    pub handle: NodeHandle,
    pub serving: JoinHandle<Stopped>,
}

pub fn run_node(fake: Arc<Fake>, config: ServerConfig, node: NodeConfig) -> Node {
    let server = NodeServer::new(fake, config).unwrap();
    let (handle, events) = NodeLink::start(node).unwrap();
    let serving = tokio::spawn({
        let (server, handle) = (server.clone(), handle.clone());
        async move { server.serve(handle, events).await }
    });
    Node {
        server,
        handle,
        serving,
    }
}

impl Node {
    pub async fn shut_down(self) -> Stopped {
        self.server.shutdown_token().cancel();
        within(self.serving).await.unwrap()
    }
}

pub struct Gateway {
    pub link: GatewayLink,
    pub events: LinkEvents,
}

pub async fn accept(new_links: &mut NewLinks) -> Gateway {
    let NewLink { link, events } = within(new_links.recv()).await.unwrap();
    Gateway { link, events }
}

pub struct Harness {
    pub fake: Arc<Fake>,
    pub node: Node,
    pub server: GatewayServer,
    pub new_links: NewLinks,
    pub gateway: Gateway,
    pub proxy: Option<Proxy>,
}

pub async fn harness(fake: Arc<Fake>, config: ServerConfig) -> Harness {
    start(fake, config, false, |_| {}).await
}

pub async fn harness_behind_proxy(fake: Arc<Fake>, config: ServerConfig) -> Harness {
    start(fake, config, true, |_| {}).await
}

pub async fn start(
    fake: Arc<Fake>,
    config: ServerConfig,
    proxied: bool,
    tweak: impl FnOnce(&mut NodeConfig),
) -> Harness {
    start_with(fake, config, proxied, gateway_config(), tweak).await
}

pub async fn start_with(
    fake: Arc<Fake>,
    config: ServerConfig,
    proxied: bool,
    gateway_config: GatewayConfig,
    tweak: impl FnOnce(&mut NodeConfig),
) -> Harness {
    let (server, mut new_links) = gateway(gateway_config).await;
    let proxy = if proxied {
        Some(Proxy::start(server.local_addr()).await)
    } else {
        None
    };
    let addr = proxy
        .as_ref()
        .map_or(server.local_addr(), |proxy| proxy.addr());
    let mut node_config = node_config(addr);
    tweak(&mut node_config);
    let node = run_node(fake.clone(), config, node_config);
    let gateway = accept(&mut new_links).await;
    Harness {
        fake,
        node,
        server,
        new_links,
        gateway,
        proxy,
    }
}

pub fn all_caps() -> GatewayCapabilities {
    GatewayCapabilities {
        question: true,
        plan: true,
        sign_in: true,
        resource_schemes: vec![],
        readable_schemes: vec![],
    }
}

pub async fn call(link: &GatewayLink, call: GatewayCall) -> Result<GatewayReply, CallError> {
    let pending = within(link.call(call)).await?;
    within(pending.reply).await
}

pub async fn initialize(
    link: &GatewayLink,
    capabilities: GatewayCapabilities,
) -> rax::session::Initialized {
    let offer = Initialize {
        protocol_version: rax::PROTOCOL_VERSION,
        capabilities,
    };
    match call(link, GatewayCall::Initialize(offer)).await {
        Ok(GatewayReply::Initialize(initialized)) => initialized,
        other => panic!("expected initialize, got {other:?}"),
    }
}

pub async fn new_session(link: &GatewayLink) -> SessionId {
    let request = GatewayCall::NewSession(NewSessionCall { context: vec![] });
    match call(link, request).await {
        Ok(GatewayReply::NewSession(created)) => created.session_id,
        other => panic!("expected session.new, got {other:?}"),
    }
}

pub async fn prompt(link: &GatewayLink, session_id: &SessionId, text: &str) -> PendingCall {
    let request = GatewayCall::Prompt(Prompt {
        session_id: session_id.clone(),
        content: vec![ContentBlock::text(text).into()],
    });
    within(link.call(request)).await.unwrap()
}

pub async fn accepted(link: &GatewayLink, session_id: &SessionId, text: &str) -> StreamEvents {
    let pending = prompt(link, session_id, text).await;
    match within(pending.reply).await {
        Ok(GatewayReply::Prompt(_)) => pending.events,
        other => panic!("expected the prompt to be accepted, got {other:?}"),
    }
}

pub async fn faulted(pending: PendingCall) -> rax::Error {
    match within(pending.reply).await {
        Err(CallError::Fault(error)) => error,
        other => panic!("expected a fault, got {other:?}"),
    }
}

pub async fn cancel(link: &GatewayLink, session_id: &SessionId) {
    let request = GatewayCall::Cancel(SessionRef {
        session_id: session_id.clone(),
    });
    assert!(matches!(
        call(link, request).await,
        Ok(GatewayReply::Cancel)
    ));
}

pub async fn next_event(events: &mut StreamEvents) -> Event {
    match within(events.recv()).await {
        Some(Open::Known(event)) => event,
        other => panic!("expected an event, got {other:?}"),
    }
}

pub async fn ended(events: &mut StreamEvents) {
    assert_eq!(
        within(events.recv()).await,
        None,
        "expected the stream to end"
    );
}

pub async fn next_link(events: &mut LinkEvents) -> LinkEvent {
    within(events.recv()).await.expect("link events ended")
}

pub struct Proxy {
    addr: SocketAddr,
    upstream: Arc<Mutex<SocketAddr>>,
    connections: Arc<Mutex<Vec<JoinHandle<()>>>>,
    task: JoinHandle<()>,
}

impl Proxy {
    pub async fn start(upstream: SocketAddr) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let upstream = Arc::new(Mutex::new(upstream));
        let connections = Arc::new(Mutex::new(Vec::new()));
        let task = tokio::spawn({
            let (upstream, connections) = (upstream.clone(), connections.clone());
            async move {
                while let Ok((mut down, _)) = listener.accept().await {
                    let target = *upstream.lock().unwrap();
                    let (go, started) = oneshot::channel::<()>();
                    let relay = tokio::spawn(async move {
                        if started.await.is_err() {
                            return;
                        }
                        let Ok(mut up) = TcpStream::connect(target).await else {
                            return;
                        };
                        let _ = tokio::io::copy_bidirectional(&mut down, &mut up).await;
                    });
                    connections.lock().unwrap().push(relay);
                    let _ = go.send(());
                }
            }
        });
        Self {
            addr,
            upstream,
            connections,
            task,
        }
    }

    pub fn addr(&self) -> SocketAddr {
        self.addr
    }

    pub fn retarget(&self, upstream: SocketAddr) {
        *self.upstream.lock().unwrap() = upstream;
    }

    pub fn sever(&self) {
        for relay in self.connections.lock().unwrap().drain(..) {
            relay.abort();
        }
    }
}

impl Drop for Proxy {
    fn drop(&mut self) {
        self.sever();
        self.task.abort();
    }
}

pub async fn closed_port() -> SocketAddr {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    listener.local_addr().unwrap()
}
