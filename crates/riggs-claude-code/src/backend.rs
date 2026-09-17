use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock, PoisonError};

use async_trait::async_trait;
use rax::content::ContentBlock;
use rax::credential::CredentialRenewal;
use rax::session::{PromptCapabilities, SessionDurability, ToolGate as GateMode};
use rax::{ErrorKind, Open, Unhandled};
use riggs_node::{
    Backend, BackendError, BackendInfo, BackendRecord, HostHandles, NewSession, Opened, Restore,
    SessionKey, TurnFailed, TurnHandle,
};
use serde::{Deserialize, Serialize};

use crate::args::Launch;
use crate::config::ClaudeCodeConfig;
use crate::content;
use crate::error::{self, ClaudeCodeError};
use crate::health::{self, Health};
use crate::process::{self, Ctx, Proc};
use crate::repair::Repair;

/// Stored with every session record, so a node moved to another agent never resumes these.
pub const BACKEND_NAME: &str = "claude-code";

/// What a durable session keeps: Claude Code's own session id is the RAX session id.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ClaudeCodeRecord {
    pub claude_session_id: String,
    pub workdir: String,
}

#[derive(Default)]
struct Slot {
    spawning: tokio::sync::Mutex<()>,
    proc: Mutex<Option<Arc<Proc>>>,
    unused: AtomicBool,
    context: Mutex<Option<String>>,
}

/// One Claude Code process per session, started at its first prompt or restore and kept for
/// later turns, so background subagents can still report after a turn ends.
pub struct ClaudeCode {
    ctx: Arc<Ctx>,
    slots: Mutex<HashMap<SessionKey, Arc<Slot>>>,
    repair: Arc<Repair>,
    probe: Mutex<Option<tokio::task::AbortHandle>>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

impl ClaudeCode {
    pub fn new(config: ClaudeCodeConfig) -> Self {
        let ctx = Arc::new(Ctx {
            health: Health::new(&config),
            config,
            host: OnceLock::new(),
        });
        Self {
            repair: Arc::new(Repair::new(ctx.clone())),
            ctx,
            slots: Mutex::new(HashMap::new()),
            probe: Mutex::new(None),
        }
    }

    /// For `ServerConfig::turn_failed` of the node serving this backend: the gateway hears `credential`
    /// only once a sign-in is in front of the owner. On any other node it logs the misuse instead.
    pub fn turn_failed(&self) -> TurnFailed {
        let repair = self.repair.clone();
        Arc::new(move |error| Box::pin(repair.clone().failed(error)))
    }

    fn failed(&self, err: ClaudeCodeError) -> BackendError {
        if err.is_credential() {
            self.ctx.health.degraded(&err.to_string());
        }
        BackendError::new(err)
    }

    fn slot(&self, key: &SessionKey) -> Option<Arc<Slot>> {
        lock(&self.slots).get(key).cloned()
    }

    fn live(slot: &Slot) -> Option<Arc<Proc>> {
        lock(&slot.proc).clone().filter(|proc| proc.is_alive())
    }

    async fn running(&self, key: &SessionKey, slot: &Slot) -> Result<Arc<Proc>, ClaudeCodeError> {
        let _spawning = slot.spawning.lock().await;
        if let Some(proc) = Self::live(slot) {
            return Ok(proc);
        }
        let launch = if slot.unused.load(Ordering::SeqCst) {
            Launch::Create
        } else {
            Launch::Resume
        };
        let proc = process::spawn(self.ctx.clone(), *key, launch).await?;
        *lock(&slot.proc) = Some(proc.clone());
        Ok(proc)
    }

    fn record(&self, key: &SessionKey) -> BackendRecord {
        let record = ClaudeCodeRecord {
            claude_session_id: key.to_string(),
            workdir: self.ctx.config.workdir.display().to_string(),
        };
        BackendRecord(serde_json::to_value(record).unwrap_or_default())
    }
}

#[async_trait]
impl Backend for ClaudeCode {
    async fn start(&self, host: HostHandles) -> Result<BackendInfo, BackendError> {
        let workdir = &self.ctx.config.workdir;
        match tokio::fs::metadata(workdir).await {
            Ok(meta) if meta.is_dir() => {}
            Ok(_) => {
                return Err(BackendError::new(ClaudeCodeError::Workdir {
                    path: workdir.display().to_string(),
                    source: std::io::Error::other("it is not a directory"),
                }));
            }
            Err(source) => {
                return Err(BackendError::new(ClaudeCodeError::Workdir {
                    path: workdir.display().to_string(),
                    source,
                }));
            }
        }
        if self.ctx.host.set(host.clone()).is_ok() {
            self.ctx.health.start(host.credentials);
            let ctx = self.ctx.clone();
            let probe = tokio::spawn(async move {
                let result = health::probe(&ctx.config).await;
                ctx.health.probed(result);
            });
            *lock(&self.probe) = Some(probe.abort_handle());
        }
        Ok(BackendInfo {
            name: BACKEND_NAME.to_owned(),
            interruptible: true,
            tool_gate: GateMode::EveryCall,
            sessions: SessionDurability::Durable,
            prompt: PromptCapabilities {
                image: true,
                audio: false,
                embedded_resource: false,
            },
            resource_schemes: Vec::new(),
        })
    }

    async fn new_session(&self, request: NewSession<'_>) -> Result<Opened, BackendError> {
        let (context, unhandled) = content::context(request.context);
        let slot = Slot {
            unused: AtomicBool::new(true),
            context: Mutex::new(context),
            ..Slot::default()
        };
        lock(&self.slots).insert(*request.key, Arc::new(slot));
        Ok(Opened {
            record: self.record(request.key),
            unhandled,
        })
    }

    async fn restore_session(
        &self,
        key: &SessionKey,
        record: &BackendRecord,
    ) -> Result<Restore, BackendError> {
        let parsed = serde_json::from_value::<ClaudeCodeRecord>(record.0.clone());
        let Some(saved) = parsed
            .ok()
            .filter(|saved| saved.claude_session_id == key.to_string())
        else {
            tracing::warn!(session_id = %key, "a session record does not name its Claude Code session");
            return Ok(Restore::Gone);
        };
        if saved.workdir != self.ctx.config.workdir.display().to_string() {
            tracing::warn!(session_id = %key, was = %saved.workdir, "resuming a session that ran in another working directory");
        }
        let slot = Arc::new(Slot::default());
        lock(&self.slots).insert(*key, slot.clone());
        match self.running(key, &slot).await {
            Ok(_) => Ok(Restore::Restored),
            Err(ClaudeCodeError::ResumeMiss(_)) => {
                lock(&self.slots).remove(key);
                Ok(Restore::Gone)
            }
            Err(err) => {
                lock(&self.slots).remove(key);
                Err(self.failed(err))
            }
        }
    }

    async fn prompt(
        &self,
        key: &SessionKey,
        content: Vec<Open<ContentBlock>>,
        turn: TurnHandle,
    ) -> Result<Vec<Unhandled>, BackendError> {
        let slot = self
            .slot(key)
            .ok_or_else(|| ClaudeCodeError::UnknownSession(key.to_string()))?;
        let proc = self
            .running(key, &slot)
            .await
            .map_err(|err| self.failed(err))?;
        if turn.cancelled.is_cancelled() {
            return Err(BackendError::new(ClaudeCodeError::Cancelled));
        }
        let converted = content::prompt(content);
        let context = lock(&slot.context).clone();
        let mut blocks: Vec<_> = context
            .iter()
            .map(|text| content::text_block(text))
            .collect();
        if converted.blocks.is_empty() {
            return Err(BackendError::new(ClaudeCodeError::EmptyPrompt));
        }
        blocks.extend(converted.blocks);
        proc.begin_turn(turn, blocks)?;
        slot.unused.store(false, Ordering::SeqCst);
        lock(&slot.context).take();
        Ok(converted.unhandled)
    }

    async fn cancel(&self, key: &SessionKey) -> Result<(), BackendError> {
        if let Some(proc) = self.slot(key).as_deref().and_then(Self::live) {
            proc.interrupt();
        }
        Ok(())
    }

    async fn close_session(&self, key: &SessionKey) {
        let Some(slot) = lock(&self.slots).remove(key) else {
            return;
        };
        let proc = lock(&slot.proc).take();
        if let Some(proc) = proc {
            proc.terminate().await;
        }
    }

    async fn renew_credential(&self) -> CredentialRenewal {
        self.repair.clone().renew().await
    }

    fn classify(&self, err: &BackendError) -> ErrorKind {
        error::classify(err)
    }

    async fn shutdown(&self) {
        if let Some(probe) = lock(&self.probe).take() {
            probe.abort();
        }
        self.repair.stop().await;
        let slots: Vec<Arc<Slot>> = lock(&self.slots).drain().map(|(_, slot)| slot).collect();
        let procs: Vec<Arc<Proc>> = slots
            .iter()
            .filter_map(|slot| lock(&slot.proc).take())
            .collect();
        for proc in &procs {
            proc.kill();
        }
        for proc in procs {
            proc.terminate().await;
        }
    }
}

impl From<ClaudeCodeError> for BackendError {
    fn from(err: ClaudeCodeError) -> Self {
        BackendError::new(err)
    }
}
