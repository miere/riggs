use std::path::PathBuf;
use std::time::Duration;

use rax::transport::EndpointError;
use rax_tokio::node::{NodeConfig, NodeLink};
use riggs_node::{NodeServer, Stopped};
use tokio::time::{Instant, sleep};

use crate::token::{self, Fingerprint, NodeToken};

pub const BACKOFF_FLOOR: Duration = Duration::from_secs(1);
pub const BACKOFF_CEILING: Duration = Duration::from_secs(30);
/// A link that dies sooner than this keeps its backoff, so one that fails on arrival cannot
/// redial every second forever.
pub const HEALTHY_LINK: Duration = Duration::from_secs(30);
pub const TOKEN_POLL: Duration = Duration::from_secs(2);

#[derive(Debug, thiserror::Error)]
pub enum DialError {
    #[error(
        "the gateway refused this node with HTTP {status}, and dialling again will not change that"
    )]
    Refused { status: u16 },
    #[error(
        "the gateway rejected this node's metadata: {message}. Fix its metadata in the configuration and start riggs again"
    )]
    Rejected { message: String },
    #[error(transparent)]
    Endpoint(#[from] EndpointError),
}

/// Keeps one gateway's link up. A node runs one per gateway, each with its own token, backoff and
/// refusals: a gateway that refuses for good ends its own link only.
pub struct Dialer {
    pub server: NodeServer,
    pub gateway: String,
    pub endpoints: Vec<String>,
    pub token_file: PathBuf,
}

impl Dialer {
    /// Returns once the server has shut down, or with the refusal that ended dialling this
    /// gateway for good. The server keeps serving every other gateway either way.
    pub async fn run(self, first: NodeToken) -> Result<(), DialError> {
        let shutdown = self.server.shutdown_token();
        let mut backoff = Backoff::new(BACKOFF_FLOOR, BACKOFF_CEILING);
        let mut next = Some(first);
        let mut rejected: Option<Fingerprint> = None;
        loop {
            let token = match next.take() {
                Some(token) => token,
                None => match self.wait_for_token(rejected).await {
                    Some(token) => token,
                    None => return Ok(()),
                },
            };
            let (handle, events) = NodeLink::start(NodeConfig {
                endpoints: self.endpoints.clone(),
                token: token.expose().to_owned(),
                backoff_min: BACKOFF_FLOOR,
                backoff_max: BACKOFF_CEILING,
                ..NodeConfig::default()
            })?;
            let started = Instant::now();
            let gateway = self.gateway.as_str();
            match self.server.serve_gateway(gateway, handle, events).await {
                Stopped::Shutdown => return Ok(()),
                Stopped::CredentialRejected => {
                    tracing::error!(
                        %gateway,
                        urls = ?self.endpoints,
                        token_file = %self.token_file.display(),
                        "the gateway rejected this node's credential: it is unknown, revoked or expired. Write a new token to the token file; riggs picks it up without a restart"
                    );
                    rejected = Some(token.fingerprint());
                    continue;
                }
                Stopped::Refused { status } if (500..600).contains(&status) => {
                    tracing::warn!(%gateway, urls = ?self.endpoints, status, "the gateway is not serving nodes right now; dialling again");
                }
                Stopped::Refused { status } => {
                    tracing::error!(%gateway, urls = ?self.endpoints, status, "the gateway refused this node; not dialling it again");
                    return Err(DialError::Refused { status });
                }
                Stopped::Rejected(rejection) => {
                    tracing::error!(%gateway, urls = ?self.endpoints, key = ?rejection.key, reason = %rejection.message, "the gateway rejected this node's metadata; not dialling it again until its metadata is fixed");
                    return Err(DialError::Rejected {
                        message: rejection.message,
                    });
                }
                Stopped::Closed => {}
                Stopped::LinkEnded => {
                    tracing::warn!(%gateway, urls = ?self.endpoints, "the gateway link ended; dialling again");
                }
            }
            if started.elapsed() >= HEALTHY_LINK {
                backoff.reset();
            }
            tokio::select! {
                () = sleep(backoff.next()) => {}
                () = shutdown.cancelled() => return Ok(()),
            }
        }
    }

    async fn wait_for_token(&self, rejected: Option<Fingerprint>) -> Option<NodeToken> {
        let shutdown = self.server.shutdown_token();
        let mut reported: Option<String> = None;
        loop {
            match token::read(&self.token_file) {
                Ok(token) if Some(token.fingerprint()) != rejected => return Some(token),
                Ok(_) => reported = None,
                Err(err) => {
                    let message = err.to_string();
                    if reported.as_ref() != Some(&message) {
                        tracing::error!(gateway = %self.gateway, error = %message, "could not read this node's credential; trying again shortly");
                        reported = Some(message);
                    }
                }
            }
            tokio::select! {
                () = sleep(TOKEN_POLL) => {}
                () = shutdown.cancelled() => return None,
            }
        }
    }
}

/// Doubling with jitter in `[d/2, d]`, the first retry included, so a fleet does not redial in step.
#[derive(Debug)]
pub struct Backoff {
    floor: Duration,
    ceiling: Duration,
    current: Duration,
}

impl Backoff {
    pub fn new(floor: Duration, ceiling: Duration) -> Self {
        Self {
            floor,
            ceiling,
            current: floor,
        }
    }

    pub fn reset(&mut self) {
        self.current = self.floor;
    }

    pub fn next(&mut self) -> Duration {
        let base = self.current;
        self.current = base.saturating_mul(2).min(self.ceiling);
        let low = base / 2;
        let spread = u64::try_from((base - low).as_millis()).unwrap_or(u64::MAX);
        low + Duration::from_millis(rand::random_range(0..=spread))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_doubles_to_the_ceiling_with_jitter_in_the_upper_half() {
        let mut backoff = Backoff::new(BACKOFF_FLOOR, BACKOFF_CEILING);
        let mut expected = BACKOFF_FLOOR;
        for _ in 0..8 {
            let delay = backoff.next();
            assert!(
                delay >= expected / 2 && delay <= expected,
                "{delay:?} for {expected:?}"
            );
            expected = (expected * 2).min(BACKOFF_CEILING);
        }
        assert_eq!(expected, BACKOFF_CEILING);
        backoff.reset();
        assert!(backoff.next() <= BACKOFF_FLOOR);
    }
}
