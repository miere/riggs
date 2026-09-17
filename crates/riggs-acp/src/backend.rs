use std::collections::HashMap;
use std::future::Future;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard, OnceLock, PoisonError};

use async_trait::async_trait;
use rax::content::ContentBlock;
use rax::session::{SessionDurability, ToolGate as GateMode};
use rax::{ErrorKind, Open, Unhandled};
use riggs_node::{
    Backend, BackendError, BackendInfo, BackendRecord, HostHandles, NewSession, Opened, Restore,
    SessionKey, TurnHandle,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::agent::{self, AgentProcess};
use crate::config::AcpConfig;
use crate::error::AcpError;
use crate::map;
use crate::routes::Owner;

/// Runs one agent process per RAX session, so conversations never share an agent's context.
pub struct AcpBackend {
    config: AcpConfig,
    host: OnceLock<HostHandles>,
    sessions: Mutex<HashMap<SessionKey, Arc<Session>>>,
}

/// What a restore needs after a node restart; the agent keeps the conversation itself.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
struct Record {
    acp_session_id: String,
    load_session: bool,
}

struct Session {
    record: Record,
    context: Mutex<Vec<Value>>,
    live: Mutex<Option<Arc<AgentProcess>>>,
    relaunching: tokio::sync::Mutex<()>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

impl AcpBackend {
    pub fn new(config: AcpConfig) -> Self {
        Self {
            config,
            host: OnceLock::new(),
            sessions: Mutex::default(),
        }
    }

    fn workdir(&self) -> Result<PathBuf, AcpError> {
        let dir = self.config.workdir.clone().unwrap_or_else(|| ".".into());
        std::path::absolute(dir).map_err(|source| AcpError::Spawn {
            command: self.config.command.clone(),
            source,
        })
    }

    fn owner(&self, key: &SessionKey) -> Result<Owner, AcpError> {
        let host = self.host.get().cloned().ok_or_else(|| {
            AcpError::AgentGone("the ACP backend was used before it started".to_owned())
        })?;
        Ok(Owner { key: *key, host })
    }

    async fn starting<T>(
        &self,
        start: impl Future<Output = Result<T, AcpError>>,
    ) -> Result<T, AcpError> {
        tokio::time::timeout(self.config.startup_timeout, start)
            .await
            .map_err(|_| AcpError::StartupTimeout(self.config.startup_timeout))?
    }

    fn session(&self, key: &SessionKey) -> Option<Arc<Session>> {
        lock(&self.sessions).get(key).cloned()
    }

    fn insert(&self, key: SessionKey, record: Record, context: Vec<Value>, agent: AgentProcess) {
        let session = Session {
            record,
            context: Mutex::new(context),
            live: Mutex::new(Some(Arc::new(agent))),
            relaunching: tokio::sync::Mutex::new(()),
        };
        lock(&self.sessions).insert(key, Arc::new(session));
    }

    async fn reload(&self, key: &SessionKey, record: &Record) -> Result<AgentProcess, AcpError> {
        let workdir = self.workdir()?;
        let owner = self.owner(key)?;
        let lost = |reason: String| AcpError::SessionLost {
            session: record.acp_session_id.clone(),
            reason,
        };
        self.starting(async {
            let agent = agent::launch(&self.config, &workdir, Some(owner)).await?;
            if !agent.info.load_session {
                return Err(lost("the agent no longer offers session/load".to_owned()));
            }
            agent
                .load_session(&record.acp_session_id, &workdir)
                .await
                .map_err(|err| lost(err.to_string()))?;
            Ok(agent)
        })
        .await
    }

    async fn agent_for_turn(
        &self,
        key: &SessionKey,
        session: &Session,
        cancelled: &CancellationToken,
    ) -> Result<Arc<AgentProcess>, AcpError> {
        let _one_at_a_time = session.relaunching.lock().await;
        if let Some(agent) = lock(&session.live)
            .as_ref()
            .filter(|agent| agent.is_alive())
        {
            return Ok(agent.clone());
        }
        if !session.record.load_session {
            return Err(AcpError::SessionLost {
                session: session.record.acp_session_id.clone(),
                reason: "its agent process stopped and the agent cannot reload sessions".to_owned(),
            });
        }
        let agent = tokio::select! {
            agent = self.reload(key, &session.record) => Arc::new(agent?),
            () = cancelled.cancelled() => return Err(AcpError::Cancelled),
        };
        *lock(&session.live) = Some(agent.clone());
        Ok(agent)
    }

    fn live(&self, key: &SessionKey) -> Option<Arc<AgentProcess>> {
        self.session(key)
            .and_then(|session| lock(&session.live).clone())
    }
}

#[async_trait]
impl Backend for AcpBackend {
    async fn start(&self, host: HostHandles) -> Result<BackendInfo, BackendError> {
        let _ = self.host.set(host);
        let workdir = self.workdir().map_err(BackendError::new)?;
        let info = self
            .starting(async {
                let probe = agent::launch(&self.config, &workdir, None).await?;
                let info = probe.info.clone();
                probe.close().await;
                Ok(info)
            })
            .await
            .map_err(BackendError::new)?;
        Ok(BackendInfo {
            name: format!("acp:{}", self.config.command),
            interruptible: self.config.interruptible.unwrap_or(true),
            tool_gate: GateMode::PermissionPrompts,
            sessions: if info.load_session {
                SessionDurability::Durable
            } else {
                SessionDurability::Ephemeral
            },
            prompt: info.prompt,
            resource_schemes: Vec::new(),
        })
    }

    async fn new_session(&self, request: NewSession<'_>) -> Result<Opened, BackendError> {
        let key = *request.key;
        let opened = async {
            let workdir = self.workdir()?;
            let owner = self.owner(&key)?;
            self.starting(async {
                let agent = agent::launch(&self.config, &workdir, Some(owner)).await?;
                let session = agent.new_session(&workdir).await?;
                Ok((agent, session))
            })
            .await
        };
        let (agent, acp_session_id) = opened.await.map_err(BackendError::new)?;
        let (context, unhandled) = map::prompt_blocks(request.context, &agent.info.prompt);
        let record = Record {
            acp_session_id,
            load_session: agent.info.load_session,
        };
        let stored = serde_json::to_value(&record).map_err(BackendError::new)?;
        self.insert(key, record, context, agent);
        Ok(Opened {
            record: BackendRecord(stored),
            unhandled,
        })
    }

    async fn restore_session(
        &self,
        key: &SessionKey,
        record: &BackendRecord,
    ) -> Result<Restore, BackendError> {
        let Ok(record) = serde_json::from_value::<Record>(record.0.clone()) else {
            tracing::warn!(session_id = %key, "an ACP session record cannot be read");
            return Ok(Restore::Gone);
        };
        if !record.load_session {
            return Ok(Restore::Gone);
        }
        match self.reload(key, &record).await {
            Ok(agent) => {
                self.insert(*key, record, Vec::new(), agent);
                Ok(Restore::Restored)
            }
            Err(err @ AcpError::SessionLost { .. }) => {
                tracing::warn!(session_id = %key, error = %err, "the agent could not reload a session");
                Ok(Restore::Gone)
            }
            Err(err) => Err(BackendError::new(err)),
        }
    }

    async fn prompt(
        &self,
        key: &SessionKey,
        content: Vec<Open<ContentBlock>>,
        turn: TurnHandle,
    ) -> Result<Vec<Unhandled>, BackendError> {
        let session = self
            .session(key)
            .ok_or_else(|| BackendError::new(AcpError::UnknownSession(key.to_string())))?;
        let agent = match self.agent_for_turn(key, &session, &turn.cancelled).await {
            Ok(agent) => agent,
            Err(err @ AcpError::SessionLost { .. }) => {
                lock(&self.sessions).remove(key);
                return Err(BackendError::new(err));
            }
            Err(err) => return Err(BackendError::new(err)),
        };
        let (blocks, unhandled) = map::prompt_blocks(&content, &agent.info.prompt);
        let mut sent = std::mem::take(&mut *lock(&session.context));
        sent.extend(blocks);
        agent
            .prompt(turn, sent, self.config.cancel_grace_period)
            .map_err(BackendError::new)?;
        Ok(unhandled)
    }

    async fn cancel(&self, key: &SessionKey) -> Result<(), BackendError> {
        if let Some(agent) = self.live(key) {
            agent.cancel().await;
        }
        Ok(())
    }

    async fn close_session(&self, key: &SessionKey) {
        let session = lock(&self.sessions).remove(key);
        let agent = session.and_then(|session| lock(&session.live).take());
        if let Some(agent) = agent {
            agent.close().await;
        }
    }

    fn classify(&self, err: &BackendError) -> ErrorKind {
        err.downcast_ref::<AcpError>()
            .map_or(ErrorKind::Unknown, AcpError::kind)
    }

    async fn shutdown(&self) {
        let sessions: Vec<Arc<Session>> = lock(&self.sessions).drain().map(|(_, s)| s).collect();
        let closing = sessions
            .iter()
            .filter_map(|session| lock(&session.live).take())
            .map(|agent| async move { agent.close().await });
        futures::future::join_all(closing).await;
    }
}
