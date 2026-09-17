use std::fmt;
use std::io;

use rax::ErrorKind;
use rax::error::ProviderFailure;
use riggs_node::BackendError;

const AUTH_MARKERS: &[&str] = &[
    "please run /login",
    "run /login",
    "oauth 401",
    "invalid_grant",
    "not logged in",
    "invalid api key",
    "authentication_error",
    "authentication_failed",
];

/// The last few KiB Claude Code wrote to stderr, which is where it explains a failed start.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct StderrTail(pub String);

impl fmt::Display for StderrTail {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let tail = self.0.trim();
        if tail.is_empty() {
            Ok(())
        } else {
            write!(f, "; stderr: {tail}")
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ClaudeCodeError {
    #[error("riggs-claude-code: the working directory {path} is not usable: {source}")]
    Workdir {
        path: String,
        #[source]
        source: io::Error,
    },
    #[error("riggs-claude-code: could not start {command}: {source}")]
    Spawn {
        command: String,
        #[source]
        source: io::Error,
    },
    #[error("riggs-claude-code: Claude Code did not become ready within {secs} s{stderr}")]
    HandshakeTimeout { secs: u64, stderr: StderrTail },
    #[error("riggs-claude-code: Claude Code refused to initialize: {0}")]
    InitializeRejected(String),
    #[error("riggs-claude-code: Claude Code exited before it was ready ({status}){stderr}")]
    ExitedEarly { status: String, stderr: StderrTail },
    /// Never answered by starting a fresh conversation under the same id.
    #[error("riggs-claude-code: Claude Code has no conversation {0} to resume")]
    ResumeMiss(String),
    #[error("riggs-claude-code: session {0} is not open on this node")]
    UnknownSession(String),
    #[error("riggs-claude-code: the turn was cancelled before it started")]
    Cancelled,
    #[error("riggs-claude-code: session {0} already has a turn running")]
    Busy(String),
    #[error("riggs-claude-code: the prompt has nothing Claude Code can read")]
    EmptyPrompt,
    #[error("riggs-claude-code: Claude Code is no longer reading its input")]
    StdinClosed,
    #[error("riggs-claude-code: Claude Code exited mid-turn ({status}){stderr}")]
    StreamClosed { status: String, stderr: StderrTail },
    #[error(
        "riggs-claude-code: Claude Code wrote a line over {limit} bytes; its process was restarted"
    )]
    LineTooLong { limit: usize },
    #[error("riggs-claude-code: the turn stopped mid-execution ({subtype}){}", details(.errors))]
    Aborted {
        subtype: String,
        errors: Vec<String>,
    },
    #[error("riggs-claude-code: the model API failed{}: {message}", status.map(|status| format!(" with {status}")).unwrap_or_default())]
    Api {
        status: Option<u16>,
        error: Option<String>,
        message: String,
    },
}

fn details(errors: &[String]) -> String {
    if errors.is_empty() {
        String::new()
    } else {
        format!(": {}", errors.join("; "))
    }
}

impl ClaudeCodeError {
    pub(crate) fn is_credential(&self) -> bool {
        kind(self) == ErrorKind::Credential
    }
}

pub(crate) fn classify(err: &BackendError) -> ErrorKind {
    match err.downcast_ref::<ClaudeCodeError>() {
        Some(err) => kind(err),
        None if is_auth_failure(&err.to_string()) => ErrorKind::Credential,
        None => ErrorKind::Unknown,
    }
}

pub(crate) fn kind(err: &ClaudeCodeError) -> ErrorKind {
    match err {
        ClaudeCodeError::Cancelled => ErrorKind::Cancelled,
        ClaudeCodeError::Busy(_) => ErrorKind::SessionBusy,
        ClaudeCodeError::ResumeMiss(_) | ClaudeCodeError::UnknownSession(_) => {
            ErrorKind::UnknownSession
        }
        ClaudeCodeError::Api {
            status,
            error,
            message,
        } => api_kind(*status, error.as_deref(), message),
        other if is_auth_failure(&other.to_string()) => ErrorKind::Credential,
        _ => ErrorKind::Unknown,
    }
}

fn api_kind(status: Option<u16>, error: Option<&str>, message: &str) -> ErrorKind {
    if status == Some(401) || error == Some("authentication_failed") || is_auth_failure(message) {
        return ErrorKind::Credential;
    }
    let (kind, retryable) = match status {
        Some(429) => ("rate_limit", true),
        Some(529) => ("overloaded", true),
        Some(403) => ("forbidden", false),
        Some(400) if message.to_lowercase().contains("prompt is too long") => {
            ("context_overflow", false)
        }
        Some(status) if status >= 500 => ("server_error", true),
        _ => ("api_error", false),
    };
    ErrorKind::Provider {
        provider: ProviderFailure {
            kind: kind.to_owned(),
            provider: Some("anthropic".to_owned()),
            status_code: status,
            message: Some(message.to_owned()),
            retryable,
        },
    }
}

fn is_auth_failure(text: &str) -> bool {
    let text = text.to_lowercase();
    AUTH_MARKERS.iter().any(|marker| text.contains(marker))
}

#[cfg(test)]
mod tests {
    #![allow(clippy::panic)]

    use super::*;

    fn api(status: u16, error: Option<&str>, message: &str) -> ErrorKind {
        kind(&ClaudeCodeError::Api {
            status: Some(status),
            error: error.map(str::to_owned),
            message: message.to_owned(),
        })
    }

    #[test]
    fn api_failures_are_classified_by_status() {
        assert_eq!(
            api(
                401,
                Some("authentication_failed"),
                "Invalid API key · Fix external API key"
            ),
            ErrorKind::Credential
        );
        assert_eq!(api(403, None, "Please run /login"), ErrorKind::Credential);
        let retryable = |kind: ErrorKind| match kind {
            ErrorKind::Provider { provider } => (provider.kind, provider.retryable),
            other => panic!("expected a provider error, got {other:?}"),
        };
        assert_eq!(
            retryable(api(
                429,
                Some("rate_limit"),
                "API Error: Request rejected (429)"
            )),
            ("rate_limit".to_owned(), true)
        );
        assert_eq!(
            retryable(api(529, Some("server_error"), "API Error: 529 Overloaded.")),
            ("overloaded".to_owned(), true)
        );
        assert_eq!(
            retryable(api(400, None, "Prompt is too long")),
            ("context_overflow".to_owned(), false)
        );
    }

    #[test]
    fn only_real_sign_in_failures_are_credential_errors() {
        let exited = |stderr: &str| ClaudeCodeError::ExitedEarly {
            status: "exit status: 1".to_owned(),
            stderr: StderrTail(stderr.to_owned()),
        };
        assert_eq!(
            kind(&exited("Invalid API key · Please run /login")),
            ErrorKind::Credential
        );
        for near_miss in [
            "API Error: 429 rate limited",
            "529 overloaded",
            "the vaultre MCP server returned 401 for /properties",
        ] {
            assert_eq!(kind(&exited(near_miss)), ErrorKind::Unknown, "{near_miss}");
        }
    }
}
