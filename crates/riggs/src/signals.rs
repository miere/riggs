use std::io;

use tokio::signal::unix::{SignalKind, signal};
use tokio_util::sync::CancellationToken;

/// What a second signal exits with: the operator asked twice, so nothing is cleaned up.
pub const FORCED_EXIT: i32 = 130;

/// The first SIGTERM or SIGINT starts a graceful shutdown; a second one exits at once.
pub fn install(shutdown: CancellationToken) -> io::Result<()> {
    let mut terminate = signal(SignalKind::terminate())?;
    let mut interrupt = signal(SignalKind::interrupt())?;
    tokio::spawn(async move {
        tokio::select! {
            _ = terminate.recv() => {}
            _ = interrupt.recv() => {}
        }
        tracing::info!("stopping; send the signal again to exit at once");
        shutdown.cancel();
        tokio::select! {
            _ = terminate.recv() => {}
            _ = interrupt.recv() => {}
        }
        tracing::warn!("stopping at once without a clean shutdown");
        std::process::exit(FORCED_EXIT);
    });
    Ok(())
}
