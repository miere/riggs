use std::path::PathBuf;
use std::sync::Arc;

use riggs_acp::AcpBackend;
use riggs_claude_code::ClaudeCode;
use riggs_node::{Backend, NodeServer, ServerConfig, ServerError, SessionsConfig, TurnFailed};

use crate::config::{AgentConfig, Config};

/// Turn failure hooks are chosen per backend, because only Claude Code has a sign-in to repair.
pub fn server(config: &Config, files_dir: PathBuf) -> Result<NodeServer, ServerError> {
    let agent = &config.agent;
    let sessions: SessionsConfig = config.sessions.clone();
    let (backend, turn_failed): (Arc<dyn Backend>, Option<TurnFailed>) = match agent {
        AgentConfig::ClaudeCode(config) => {
            let claude = Arc::new(ClaudeCode::new(*config.clone()));
            let repair = claude.turn_failed();
            (claude, Some(repair))
        }
        AgentConfig::Acp(config) => (Arc::new(AcpBackend::new(config.clone())), None),
    };
    let mut server = ServerConfig::new(sessions);
    server.turn_failed = turn_failed;
    server.files_dir = Some(files_dir);
    server.metadata = config.metadata.clone();
    server.gateway_metadata = config.gateway_metadata();
    server.primary = config.primary().name.clone();
    NodeServer::new(backend, server)
}
