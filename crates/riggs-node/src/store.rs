use std::collections::HashMap;
use std::fs::{self, File, OpenOptions, TryLockError};
use std::io::{self, Read, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, SystemTime};

use rax::Open;
use rax::content::ContentBlock;
use rax::id::SessionId;
use serde::{Deserialize, Serialize};
use time::OffsetDateTime;
use tokio::time::Instant;

use crate::backend::{BackendRecord, SessionKey};
use crate::state::lock;

const RECORD_VERSION: u32 = 1;
const LOCK_FILE: &str = "riggs.lock";
/// A child this process starts holds a copy of the lock file from the fork until it execs, so a
/// single try can be refused while no other node is serving the store.
const LOCK_PATIENCE: Duration = Duration::from_secs(1);
const LOCK_RETRY: Duration = Duration::from_millis(5);

const TOUCH_DEBOUNCE: Duration = Duration::from_secs(60);

#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// Two nodes on one store would restore, prune and delete each other's sessions, and fight
    /// over one token at the gateway.
    #[error("another riggs is already serving {dir}{}", pid.map(|pid| format!(" (pid {pid})")).unwrap_or_default())]
    Locked { dir: PathBuf, pid: Option<u32> },
    /// Work that outlived a shutdown must not touch a store that the next node may already hold.
    #[error("session store {dir} was closed when the node shut down")]
    Closed { dir: PathBuf },
    #[error("session store {path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("session record {path} is unreadable: {reason}")]
    Corrupt { path: PathBuf, reason: String },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub(crate) struct Record {
    pub(crate) v: u32,
    pub(crate) session_id: SessionId,
    pub(crate) backend: String,
    pub(crate) backend_session: BackendRecord,
    #[serde(with = "time::serde::rfc3339")]
    pub(crate) created_at: OffsetDateTime,
    #[serde(with = "time::serde::rfc3339")]
    pub(crate) last_used_at: OffsetDateTime,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub(crate) context: Vec<Open<ContentBlock>>,
}

impl Record {
    pub(crate) fn new(
        key: &SessionKey,
        backend: String,
        backend_session: BackendRecord,
        context: Vec<Open<ContentBlock>>,
    ) -> Self {
        let now = OffsetDateTime::now_utc();
        Self {
            v: RECORD_VERSION,
            session_id: key.session_id(),
            backend,
            backend_session,
            created_at: now,
            last_used_at: now,
            context,
        }
    }
}

#[derive(Clone)]
pub(crate) struct SessionStore {
    inner: Arc<Inner>,
}

struct Inner {
    dir: PathBuf,
    retain: Duration,
    io: Mutex<Io>,
}

struct Io {
    written: HashMap<SessionKey, Instant>,
    lock: Option<File>,
}

impl SessionStore {
    pub(crate) fn open(dir: &Path, retain: Duration) -> Result<Self, StoreError> {
        let io_error = |path: &Path| {
            let path = path.to_path_buf();
            move |source| StoreError::Io { path, source }
        };
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)
            .map_err(io_error(dir))?;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700)).map_err(io_error(dir))?;
        let lock_path = dir.join(LOCK_FILE);
        let mut lock_file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .open(&lock_path)
            .map_err(io_error(&lock_path))?;
        let give_up_at = std::time::Instant::now() + LOCK_PATIENCE;
        loop {
            match lock_file.try_lock() {
                Ok(()) => break,
                Err(TryLockError::WouldBlock) if std::time::Instant::now() < give_up_at => {
                    std::thread::sleep(LOCK_RETRY);
                }
                Err(TryLockError::WouldBlock) => {
                    let mut held = String::new();
                    let _ = lock_file.read_to_string(&mut held);
                    return Err(StoreError::Locked {
                        dir: dir.to_path_buf(),
                        pid: held.trim().parse().ok(),
                    });
                }
                Err(TryLockError::Error(source)) => {
                    return Err(StoreError::Io {
                        path: lock_path,
                        source,
                    });
                }
            }
        }
        lock_file
            .set_len(0)
            .and_then(|()| writeln!(lock_file, "{}", std::process::id()))
            .map_err(io_error(&lock_path))?;
        let store = Self {
            inner: Arc::new(Inner {
                dir: dir.to_path_buf(),
                retain,
                io: Mutex::new(Io {
                    written: HashMap::new(),
                    lock: Some(lock_file),
                }),
            }),
        };
        store.inner.prune();
        Ok(store)
    }

    pub(crate) async fn load(&self, key: SessionKey) -> Result<Option<Record>, StoreError> {
        self.blocking(move |inner| inner.load(&key)).await
    }

    pub(crate) async fn save(&self, key: SessionKey, record: Record) -> Result<(), StoreError> {
        self.blocking(move |inner| inner.save(&key, &record)).await
    }

    pub(crate) async fn remove(&self, key: SessionKey) -> Result<(), StoreError> {
        self.blocking(move |inner| inner.remove(&key)).await
    }

    pub(crate) async fn touch(&self, key: SessionKey, record: &mut Record) {
        record.last_used_at = OffsetDateTime::now_utc();
        let due = lock(&self.inner.io)
            .written
            .get(&key)
            .is_none_or(|written| written.elapsed() >= TOUCH_DEBOUNCE);
        if !due {
            return;
        }
        if let Err(err) = self.save(key, record.clone()).await {
            tracing::warn!(session_id = %key, error = %err, "could not record when a session was last used");
        }
    }

    pub(crate) async fn prune(&self) {
        let pruned = self.blocking(|inner| {
            inner.prune();
            Ok(())
        });
        let _ = pruned.await;
    }

    pub(crate) async fn close(&self) {
        let closed = self.blocking(|inner| {
            lock(&inner.io).lock = None;
            Ok(())
        });
        let _ = closed.await;
    }

    async fn blocking<T: Send + 'static>(
        &self,
        work: impl FnOnce(&Inner) -> Result<T, StoreError> + Send + 'static,
    ) -> Result<T, StoreError> {
        let inner = self.inner.clone();
        let dir = inner.dir.clone();
        tokio::task::spawn_blocking(move || work(&inner))
            .await
            .map_err(|err| StoreError::Io {
                path: dir,
                source: io::Error::other(err.to_string()),
            })?
    }
}

impl Inner {
    fn open_io(&self) -> Result<MutexGuard<'_, Io>, StoreError> {
        let io = lock(&self.io);
        if io.lock.is_none() {
            return Err(StoreError::Closed {
                dir: self.dir.clone(),
            });
        }
        Ok(io)
    }

    fn path(&self, key: &SessionKey) -> PathBuf {
        self.dir.join(format!("{key}.json"))
    }

    fn load(&self, key: &SessionKey) -> Result<Option<Record>, StoreError> {
        let path = self.path(key);
        let _io = self.open_io()?;
        let bytes = match fs::read(&path) {
            Ok(bytes) => bytes,
            Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(None),
            Err(source) => return Err(StoreError::Io { path, source }),
        };
        let corrupt = |reason: String| StoreError::Corrupt {
            path: path.clone(),
            reason,
        };
        let record: Record =
            serde_json::from_slice(&bytes).map_err(|err| corrupt(err.to_string()))?;
        if record.v != RECORD_VERSION {
            return Err(corrupt(format!("unknown record version {}", record.v)));
        }
        if record.session_id != key.session_id() {
            return Err(corrupt(format!(
                "the record names session {}",
                record.session_id
            )));
        }
        Ok(Some(record))
    }

    fn save(&self, key: &SessionKey, record: &Record) -> Result<(), StoreError> {
        let path = self.path(key);
        let tmp = self.dir.join(format!("{key}.json.tmp"));
        let io_error = |source| StoreError::Io {
            path: path.clone(),
            source,
        };
        let bytes =
            serde_json::to_vec_pretty(record).map_err(|err| io_error(io::Error::other(err)))?;
        let mut io = self.open_io()?;
        let _ = fs::remove_file(&tmp);
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&tmp)
            .map_err(io_error)?;
        file.write_all(&bytes)
            .and_then(|()| file.sync_all())
            .map_err(io_error)?;
        drop(file);
        fs::rename(&tmp, &path).map_err(io_error)?;
        File::open(&self.dir)
            .and_then(|dir| dir.sync_all())
            .map_err(io_error)?;
        io.written.insert(*key, Instant::now());
        Ok(())
    }

    fn remove(&self, key: &SessionKey) -> Result<(), StoreError> {
        let path = self.path(key);
        let mut io = self.open_io()?;
        io.written.remove(key);
        match fs::remove_file(&path) {
            Ok(()) => Ok(()),
            Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(()),
            Err(source) => Err(StoreError::Io { path, source }),
        }
    }

    fn prune(&self) {
        let Ok(_io) = self.open_io() else {
            return;
        };
        let entries = match fs::read_dir(&self.dir) {
            Ok(entries) => entries,
            Err(err) => {
                tracing::warn!(dir = %self.dir.display(), error = %err, "could not list the session store");
                return;
            }
        };
        let now = SystemTime::now();
        let cutoff = time::Duration::try_from(self.retain)
            .ok()
            .and_then(|retain| OffsetDateTime::now_utc().checked_sub(retain));
        let mut pruned = 0usize;
        for entry in entries.flatten() {
            let path = entry.path();
            let Some(name) = path.file_name().and_then(|name| name.to_str()) else {
                continue;
            };
            let expired = if name.ends_with(".json.tmp") {
                true
            } else if name.ends_with(".json") {
                match fs::read(&path)
                    .ok()
                    .and_then(|bytes| serde_json::from_slice::<Record>(&bytes).ok())
                {
                    Some(record) => cutoff.is_some_and(|cutoff| record.last_used_at < cutoff),
                    None => entry
                        .metadata()
                        .and_then(|meta| meta.modified())
                        .ok()
                        .and_then(|modified| now.duration_since(modified).ok())
                        .is_some_and(|age| age > self.retain),
                }
            } else {
                false
            };
            if expired && fs::remove_file(&path).is_ok() {
                pruned += 1;
            }
        }
        if pruned > 0 {
            tracing::info!(dir = %self.dir.display(), pruned, "pruned unused session records");
        }
    }
}
