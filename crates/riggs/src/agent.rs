use std::sync::Arc;

use riggs_acp::AcpBackend;
use riggs_claude_code::ClaudeCode;
use riggs_node::{Backend, NodeServer, ServerConfig, ServerError, SessionsConfig, TurnFailed};

use crate::config::AgentConfig;

/// Turn failure hooks are chosen per backend, because only Claude Code has a sign-in to repair.
pub fn server(agent: &AgentConfig, sessions: SessionsConfig) -> Result<NodeServer, ServerError> {
    let (backend, turn_failed): (Arc<dyn Backend>, Option<TurnFailed>) = match agent {
        AgentConfig::ClaudeCode(config) => {
            let claude = Arc::new(ClaudeCode::new(config.clone()));
            let repair = claude.turn_failed();
            (claude, Some(repair))
        }
        AgentConfig::Acp(config) => (Arc::new(AcpBackend::new(config.clone())), None),
    };
    let mut config = ServerConfig::new(sessions);
    config.turn_failed = turn_failed;
    NodeServer::new(backend, config)
}
