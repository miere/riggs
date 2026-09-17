use std::time::Duration;

use rax::ErrorKind;
use rax::error::RpcFailure;

pub(crate) const AUTH_REQUIRED: i64 = -32000;
pub(crate) const REQUEST_CANCELLED: i64 = -32800;

/// Each variant maps to one RAX error kind, so the gateway can branch without parsing text.
#[derive(Debug, thiserror::Error)]
pub enum AcpError {
    #[error("start ACP agent {command}: {source}")]
    Spawn {
        command: String,
        #[source]
        source: std::io::Error,
    },
    #[error("the ACP agent did not finish starting within {0:?}")]
    StartupTimeout(Duration),
    #[error("the agent speaks ACP protocol version {0}; riggs speaks 1")]
    ProtocolVersion(String),
    #[error("ACP {method} error {code}: {message}")]
    Rpc {
        method: String,
        code: i64,
        message: String,
    },
    /// Riggs never runs an ACP sign-in itself, so the owner has to sign in on the node.
    #[error(
        "the agent needs a sign-in ({message}); sign in to the agent on this node with one of: {methods}"
    )]
    AuthRequired { message: String, methods: String },
    #[error("the agent answered {method} with something riggs could not read: {detail}")]
    Unreadable { method: String, detail: String },
    #[error("{0}")]
    AgentGone(String),
    #[error("this node has no ACP session {0}")]
    UnknownSession(String),
    #[error("the ACP agent lost session {session}: {reason}")]
    SessionLost { session: String, reason: String },
    #[error("the turn was cancelled before the agent was ready")]
    Cancelled,
    #[error("the session was closed while the turn was running")]
    Closed,
    #[error("this session already has a turn open")]
    Busy,
}

impl AcpError {
    pub fn kind(&self) -> ErrorKind {
        match self {
            Self::Spawn { .. }
            | Self::StartupTimeout(_)
            | Self::Unreadable { .. }
            | Self::AgentGone(_) => ErrorKind::Unknown,
            Self::ProtocolVersion(_) => ErrorKind::Unsupported,
            Self::Rpc { method, code, .. } => ErrorKind::Rpc {
                rpc: RpcFailure {
                    method: Some(method.clone()),
                    code: *code,
                },
            },
            Self::AuthRequired { .. } => ErrorKind::Credential,
            Self::UnknownSession(_) | Self::SessionLost { .. } => ErrorKind::UnknownSession,
            Self::Cancelled | Self::Closed => ErrorKind::Cancelled,
            Self::Busy => ErrorKind::SessionBusy,
        }
    }

    pub(crate) fn rpc(method: &str, err: &agent_client_protocol::Error, auth: &[String]) -> Self {
        let code = i64::from(i32::from(err.code));
        let mut message = err.message.clone();
        if let Some(data) = &err.data {
            message = format!("{message} {data}");
        }
        if code == AUTH_REQUIRED {
            let methods = if auth.is_empty() {
                "none advertised".to_owned()
            } else {
                auth.join(", ")
            };
            return Self::AuthRequired { message, methods };
        }
        Self::Rpc {
            method: method.to_owned(),
            code,
            message,
        }
    }
}
