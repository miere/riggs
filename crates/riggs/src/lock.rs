use std::fs::{File, OpenOptions, TryLockError};
use std::io::{self, Read, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};

pub const FILE_NAME: &str = "riggs.lock";

#[derive(Debug, thiserror::Error)]
pub enum LockError {
    /// Two daemons on one config would share a token, and the gateway would keep swapping them.
    #[error("another riggs{} is already running with {dir}; stop it first", pid.map(|pid| format!(" (pid {pid})")).unwrap_or_default())]
    Held { dir: PathBuf, pid: Option<u32> },
    #[error("cannot lock {path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
}

/// Held for the life of the process; the kernel releases it even after SIGKILL.
#[derive(Debug)]
pub struct DirLock {
    _file: File,
}

pub fn acquire(dir: &Path) -> Result<DirLock, LockError> {
    let path = dir.join(FILE_NAME);
    let io_error = |source| LockError::Io {
        path: path.clone(),
        source,
    };
    let mut file = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(&path)
        .map_err(io_error)?;
    match file.try_lock() {
        Ok(()) => {}
        Err(TryLockError::WouldBlock) => {
            let mut held = String::new();
            let _ = file.read_to_string(&mut held);
            return Err(LockError::Held {
                dir: dir.to_path_buf(),
                pid: held.trim().parse().ok(),
            });
        }
        Err(TryLockError::Error(source)) => return Err(io_error(source)),
    }
    file.set_len(0)
        .and_then(|()| writeln!(file, "{}", std::process::id()))
        .map_err(io_error)?;
    Ok(DirLock { _file: file })
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

    use super::*;

    #[test]
    fn a_second_lock_on_the_same_directory_names_the_holder() {
        let dir = tempfile::tempdir().unwrap();
        let _held = acquire(dir.path()).unwrap();
        let message = acquire(dir.path()).unwrap_err().to_string();
        assert!(
            message.contains(&format!("pid {}", std::process::id())),
            "{message}"
        );
    }
}
