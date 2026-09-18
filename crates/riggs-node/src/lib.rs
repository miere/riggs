//! Serves RAX sessions onto one agent backend. The binary owns configuration, the token and
//! dialling; this crate owns everything that happens on a link once it is up.

mod backend;
mod files;
mod host;
mod server;
mod sessions;
mod state;
mod store;
mod turn;

pub use backend::{
    AttachmentMeta, AttachmentSource, Backend, BackendError, BackendEvent, BackendInfo,
    BackendRecord, NewSession, Opened, Restore, SessionKey, TurnHandle,
};
pub use host::{
    BackgroundGate, BackgroundSink, CredentialReporter, HostHandles, NotDelivered, SignInRefused,
    SignIns,
};
pub use server::{
    CALL_TIMEOUT, LINK_GRACE, NodeServer, SESSION_RETENTION, SHUTDOWN_GRACE, ServerConfig,
    ServerError, SessionsConfig, Stopped,
};
pub use store::StoreError;
pub use turn::{SignInPrompt, ToolGate, TurnClosed, TurnFailed, TurnPrompts, TurnSink};
