use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use rax::session::SessionDurability;
use tokio::sync::OwnedMutexGuard;

use crate::backend::{Backend, BackendError, BackendInfo, Restore, SessionKey};
use crate::state::lock;
use crate::store::{Record, SessionStore, StoreError};

pub(crate) enum Slot {
    Unloaded,
    Live(Record),
    Gone,
}

pub(crate) enum OpenError {
    Unknown,
    Backend(BackendError),
    Store(StoreError),
}

type SlotLock = Arc<tokio::sync::Mutex<Slot>>;

pub(crate) struct Sessions {
    store: Option<SessionStore>,
    slots: Mutex<HashMap<SessionKey, SlotLock>>,
}

impl Sessions {
    pub(crate) fn new(store: Option<SessionStore>) -> Self {
        Self {
            store,
            slots: Mutex::new(HashMap::new()),
        }
    }

    pub(crate) fn durability(&self, info: &BackendInfo) -> SessionDurability {
        match (&self.store, info.sessions) {
            (Some(_), SessionDurability::Durable) => SessionDurability::Durable,
            _ => SessionDurability::Ephemeral,
        }
    }

    fn slot(&self, key: SessionKey) -> SlotLock {
        lock(&self.slots)
            .entry(key)
            .or_insert_with(|| Arc::new(tokio::sync::Mutex::new(Slot::Unloaded)))
            .clone()
    }

    fn forget(&self, key: &SessionKey, slot: &SlotLock) {
        let mut slots = lock(&self.slots);
        if slots.get(key).is_some_and(|held| Arc::ptr_eq(held, slot)) {
            slots.remove(key);
        }
    }

    pub(crate) fn is_known(&self, key: &SessionKey) -> bool {
        lock(&self.slots).contains_key(key)
    }

    pub(crate) async fn exists(&self, key: &SessionKey) -> bool {
        if self.is_known(key) {
            return true;
        }
        match &self.store {
            Some(store) => !matches!(store.load(*key).await, Ok(None)),
            None => false,
        }
    }

    pub(crate) async fn create(
        &self,
        key: SessionKey,
        record: Record,
        info: &BackendInfo,
    ) -> Result<(), StoreError> {
        if let (Some(store), SessionDurability::Durable) = (&self.store, self.durability(info)) {
            store.save(key, record.clone()).await?;
        }
        let slot = Arc::new(tokio::sync::Mutex::new(Slot::Live(record)));
        lock(&self.slots).insert(key, slot);
        Ok(())
    }

    pub(crate) async fn open(
        &self,
        key: SessionKey,
        backend: &dyn Backend,
        info: &BackendInfo,
    ) -> Result<OwnedMutexGuard<Slot>, OpenError> {
        let slot = self.slot(key);
        let mut guard = slot.clone().lock_owned().await;
        match &*guard {
            Slot::Live(_) => return Ok(guard),
            Slot::Gone => return Err(OpenError::Unknown),
            Slot::Unloaded => {}
        }
        let restored = self.restore(key, backend, info).await;
        match restored {
            Ok(Some(record)) => {
                *guard = Slot::Live(record);
                Ok(guard)
            }
            Ok(None) => {
                *guard = Slot::Gone;
                self.forget(&key, &slot);
                Err(OpenError::Unknown)
            }
            Err(err) => {
                drop(guard);
                self.forget(&key, &slot);
                Err(err)
            }
        }
    }

    async fn restore(
        &self,
        key: SessionKey,
        backend: &dyn Backend,
        info: &BackendInfo,
    ) -> Result<Option<Record>, OpenError> {
        let Some(store) = &self.store else {
            return Ok(None);
        };
        if self.durability(info) != SessionDurability::Durable {
            return Ok(None);
        }
        let record = match store.load(key).await {
            Ok(Some(record)) => record,
            Ok(None) => return Ok(None),
            Err(err @ StoreError::Corrupt { .. }) => {
                tracing::warn!(session_id = %key, error = %err, "deleting a session record that cannot be read");
                self.delete(store, key).await;
                return Ok(None);
            }
            Err(err) => return Err(OpenError::Store(err)),
        };
        if record.backend != info.name {
            tracing::warn!(session_id = %key, backend = %record.backend, "deleting a session record another agent wrote");
            self.delete(store, key).await;
            return Ok(None);
        }
        match backend.restore_session(&key, &record.backend_session).await {
            Ok(Restore::Restored) => {
                tracing::info!(session_id = %key, "session restored");
                Ok(Some(record))
            }
            Ok(Restore::Gone) => {
                tracing::warn!(session_id = %key, "the agent no longer has this session; deleting its record");
                self.delete(store, key).await;
                Ok(None)
            }
            Err(err) => Err(OpenError::Backend(err)),
        }
    }

    async fn delete(&self, store: &SessionStore, key: SessionKey) {
        if let Err(err) = store.remove(key).await {
            tracing::warn!(session_id = %key, error = %err, "could not delete a session record");
        }
    }

    pub(crate) async fn close(&self, key: SessionKey, backend: &dyn Backend) {
        let slot = self.slot(key);
        let mut guard = slot.lock().await;
        if matches!(*guard, Slot::Live(_)) {
            backend.close_session(&key).await;
        }
        *guard = Slot::Gone;
        self.forget(&key, &slot);
        if let Some(store) = &self.store {
            self.delete(store, key).await;
        }
    }

    pub(crate) async fn touch(&self, key: SessionKey, info: &BackendInfo) {
        let Some(store) = &self.store else {
            return;
        };
        if self.durability(info) != SessionDurability::Durable {
            return;
        }
        let Some(slot) = lock(&self.slots).get(&key).cloned() else {
            return;
        };
        let mut guard = slot.lock().await;
        if let Slot::Live(record) = &mut *guard {
            store.touch(key, record).await;
        }
    }

    pub(crate) async fn prune(&self) {
        if let Some(store) = &self.store {
            store.prune().await;
        }
    }

    pub(crate) async fn release_store(&self) {
        if let Some(store) = &self.store {
            store.close().await;
        }
    }
}
