use std::fmt;

use async_trait::async_trait;
use rax::content::ContentBlock;
use rax::credential::CredentialRenewal;
use rax::event::StopReason;
use rax::id::{RequestId, SessionId};
use rax::interaction::SignInSettled;
use rax::session::{PromptCapabilities, SessionDurability, ToolGate as GateMode};
use rax::tool::{PlanEntry, ToolCallUpdate};
use rax::{ErrorKind, Open, Unhandled};
use serde::{Deserialize, Serialize};
use tokio::io::AsyncRead;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

pub use crate::host::HostHandles;
use crate::turn::{ToolGate, TurnPrompts, TurnSink};

/// What the agent can do, resolved once at `start` and declared to every gateway at `initialize`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BackendInfo {
    /// Stored with each session record, so a node switched to another agent never restores a
    /// session the new agent cannot read.
    pub name: String,
    pub interruptible: bool,
    pub tool_gate: GateMode,
    pub sessions: SessionDurability,
    pub prompt: PromptCapabilities,
    pub resource_schemes: Vec<String>,
}

/// A node-minted UUIDv4. Only canonical ids parse, so a gateway-supplied id can never alias a
/// session or name a path outside the store.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub struct SessionKey(Uuid);

impl SessionKey {
    pub(crate) fn mint() -> Self {
        Self(Uuid::new_v4())
    }

    pub fn parse(id: &SessionId) -> Option<Self> {
        let uuid = Uuid::parse_str(&id.0).ok()?;
        (uuid.hyphenated().to_string() == id.0).then_some(Self(uuid))
    }

    pub fn session_id(&self) -> SessionId {
        SessionId(self.to_string())
    }
}

impl fmt::Display for SessionKey {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.hyphenated().fmt(f)
    }
}

/// Opaque to the node server, which only persists it: each backend decides what it needs to
/// restore a conversation.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
#[serde(transparent)]
pub struct BackendRecord(pub serde_json::Value);

pub struct NewSession<'a> {
    pub key: &'a SessionKey,
    pub context: &'a [Open<ContentBlock>],
}

pub struct Opened {
    pub record: BackendRecord,
    pub unhandled: Vec<Unhandled>,
}

pub enum Restore {
    Restored,
    /// The agent's own record is gone; the gateway hears `unknown_session`, never a fresh
    /// conversation posing as the old one.
    Gone,
}

/// Keeps the backend's own error so `classify` can inspect it, and its full text for the wire.
#[derive(Debug)]
pub struct BackendError(Box<dyn std::error::Error + Send + Sync>);

impl BackendError {
    pub fn new(err: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> Self {
        Self(err.into())
    }

    pub fn downcast_ref<E: std::error::Error + 'static>(&self) -> Option<&E> {
        self.0.downcast_ref()
    }
}

impl fmt::Display for BackendError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(f)
    }
}

impl std::error::Error for BackendError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        self.0.source()
    }
}

/// A turn's output. Tool calls are absent on purpose: announcing one obliges the node to hold it,
/// so the only way to raise one is `ToolGate::hold`.
pub enum BackendEvent {
    Message(Open<ContentBlock>),
    Status(String),
    ToolCallUpdate(ToolCallUpdate),
    PlanUpdate(Vec<PlanEntry>),
    Attachment(AttachmentSource),
    /// Forgotten once terminal; dropped if the sign-in was never shown.
    SignInSettled(SignInSettled),
    Complete(Option<StopReason>),
    Error(BackendError),
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct AttachmentMeta {
    pub filename: Option<String>,
    pub title: Option<String>,
    pub comment: Option<String>,
    pub mimetype: Option<String>,
}

/// The size is declared up front because RAX announces it before the first byte is read.
pub struct AttachmentSource {
    pub meta: AttachmentMeta,
    pub size: u64,
    pub reader: Box<dyn AsyncRead + Send + Unpin>,
}

/// Everything one turn may reach. The gate and prompts stop routing to the turn once it ends.
pub struct TurnHandle {
    pub stream: RequestId,
    pub events: TurnSink,
    /// Fired by `cancel`, `session.close`, a link reset and shutdown. An interrupted turn still
    /// ends with `Complete(Some(Cancelled))`, so both backends end a cancel the same way.
    pub cancelled: CancellationToken,
    pub gate: ToolGate,
    pub prompts: TurnPrompts,
}

/// Object-safe so the binary picks Claude Code or ACP at runtime. Deny reasons and notes it
/// receives are prose for the model, never diagnostics.
#[async_trait]
pub trait Backend: Send + Sync + 'static {
    /// Called before the first reply that needs it; a failure is retried on the next request.
    async fn start(&self, host: HostHandles) -> Result<BackendInfo, BackendError>;

    async fn new_session(&self, request: NewSession<'_>) -> Result<Opened, BackendError>;

    /// Called only for durable sessions. Never silently start a fresh conversation.
    async fn restore_session(
        &self,
        key: &SessionKey,
        record: &BackendRecord,
    ) -> Result<Restore, BackendError>;

    /// Return once the turn is under way, so `cancel` can reach it. The node has already refused
    /// a second prompt on a session with an open turn.
    async fn prompt(
        &self,
        key: &SessionKey,
        content: Vec<Open<ContentBlock>>,
        turn: TurnHandle,
    ) -> Result<Vec<Unhandled>, BackendError>;

    /// Unknown keys are not an error.
    async fn cancel(&self, key: &SessionKey) -> Result<(), BackendError>;

    async fn close_session(&self, key: &SessionKey);

    async fn renew_credential(&self) -> CredentialRenewal {
        CredentialRenewal::NothingToRenew
    }

    fn classify(&self, err: &BackendError) -> ErrorKind;

    /// Kills every child process; the node bounds how long it waits.
    async fn shutdown(&self);
}
