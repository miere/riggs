use std::collections::VecDeque;
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};

use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::sync::watch;

/// The last bytes a child wrote to a pipe. The pipe is read for the child's whole life, so a
/// chatty agent never blocks on a full pipe.
#[derive(Clone)]
pub struct Tail {
    bytes: Arc<Mutex<VecDeque<u8>>>,
    closed: watch::Receiver<bool>,
}

impl Tail {
    /// Every chunk is also handed to `tap` as it arrives, for a reader that has to watch the
    /// stream live rather than read its tail once the child is done with it. The pipe closing
    /// taps a newline, so a last line the child never terminated still reaches `tap` whole.
    pub(crate) fn read(
        pipe: impl AsyncRead + Unpin + Send + 'static,
        capacity: usize,
        pid: u32,
        tap: impl FnMut(&[u8]) + Send + 'static,
    ) -> Self {
        let bytes = Arc::new(Mutex::new(VecDeque::new()));
        let (done, closed) = watch::channel(false);
        tokio::spawn(drain(pipe, bytes.clone(), capacity, pid, done, tap));
        Self { bytes, closed }
    }

    pub fn text(&self) -> String {
        let tail = lock(&self.bytes);
        let (front, back) = tail.as_slices();
        let mut bytes = front.to_vec();
        bytes.extend_from_slice(back);
        String::from_utf8_lossy(&bytes).trim().to_owned()
    }

    pub async fn closed(&self) {
        let mut closed = self.closed.clone();
        let _ = closed.wait_for(|closed| *closed).await;
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

async fn drain(
    mut pipe: impl AsyncRead + Unpin,
    tail: Arc<Mutex<VecDeque<u8>>>,
    capacity: usize,
    pid: u32,
    done: watch::Sender<bool>,
    mut tap: impl FnMut(&[u8]),
) {
    let mut buffer = vec![0u8; 8192];
    while let Ok(read) = pipe.read(&mut buffer).await {
        if read == 0 {
            break;
        }
        let chunk = buffer.get(..read).unwrap_or_default();
        tracing::debug!(pid, stderr = %String::from_utf8_lossy(chunk).trim_end(), "agent stderr");
        tap(chunk);
        let mut tail = lock(&tail);
        tail.extend(chunk);
        let excess = tail.len().saturating_sub(capacity);
        tail.drain(..excess);
    }
    tap(b"\n");
    done.send_replace(true);
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]

    use super::*;

    #[tokio::test]
    async fn keeps_only_the_last_bytes_and_reports_when_the_pipe_closes() {
        let mut written = vec![b'a'; 64];
        written.extend_from_slice(b"the end\n");
        let tail = Tail::read(std::io::Cursor::new(written), 32, 1, |_| {});
        tail.closed().await;
        let text = tail.text();
        assert_eq!(text.len(), 31);
        assert!(text.ends_with("the end"));
        assert!(text.starts_with('a'));
    }

    #[tokio::test]
    async fn a_tap_sees_everything_written_and_a_newline_ending_the_last_line() {
        let tapped = Arc::new(Mutex::new(Vec::new()));
        let seen = tapped.clone();
        let tail = Tail::read(
            std::io::Cursor::new(b"first\nunterminated".to_vec()),
            1024,
            1,
            move |chunk| lock(&seen).extend_from_slice(chunk),
        );
        tail.closed().await;
        assert_eq!(
            String::from_utf8_lossy(&lock(&tapped)),
            "first\nunterminated\n"
        );
    }
}
