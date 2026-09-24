//! Picks up edits to the `[metadata]` table while riggs runs, so a node's owner can change who may
//! talk to their machine without a restart. Everything else in the file still needs one.

use std::path::{Path, PathBuf};
use std::time::Duration;

use rax::Metadata;
use riggs_node::NodeServer;
use tokio::time::sleep;

use crate::config::{self, Overrides};

/// Reading a small file every two seconds costs nothing and needs no file-watching crate.
pub const CONFIG_POLL: Duration = Duration::from_secs(2);

pub struct Watch {
    pub path: PathBuf,
    pub overrides: Overrides,
    pub server: NodeServer,
    /// What the gateway was last told.
    pub metadata: Metadata,
}

impl Watch {
    /// Returns when the server shuts down.
    pub async fn run(mut self) {
        let shutdown = self.server.shutdown_token();
        let mut seen = read(&self.path);
        let started = outside_metadata(seen.as_deref());
        loop {
            tokio::select! {
                () = sleep(CONFIG_POLL) => {}
                () = shutdown.cancelled() => return,
            }
            let text = read(&self.path);
            if text == seen {
                continue;
            }
            seen = text;
            // The whole file is validated, so a half-saved edit is reported rather than sent.
            let config = match config::load(&self.path, &self.overrides) {
                Ok(config) => config,
                Err(err) => {
                    tracing::error!(error = %err, "the configuration no longer loads; keeping the metadata it last read");
                    continue;
                }
            };
            if outside_metadata(seen.as_deref()) != started {
                tracing::warn!(config = %self.path.display(), "the configuration changed outside [metadata]; those changes apply at the next restart");
            }
            if config.metadata != self.metadata {
                tracing::info!(keys = ?config.metadata.keys().collect::<Vec<_>>(), "metadata changed; telling the gateway");
                self.server.set_metadata(config.metadata.clone());
                self.metadata = config.metadata;
            }
        }
    }
}

fn read(path: &Path) -> Option<String> {
    std::fs::read_to_string(path).ok()
}

fn outside_metadata(text: Option<&str>) -> Option<toml::Table> {
    let mut table: toml::Table = text?.parse().ok()?;
    table.remove("metadata");
    Some(table)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_edit_inside_metadata_is_not_an_edit_outside_it() {
        let before = "[gateway]\nurls = [\"wss://a\"]\n[metadata]\nmurtaugh_access = 1\n";
        let metadata_only = "[gateway]\nurls = [\"wss://a\"]\n[metadata]\nmurtaugh_access = 2\n";
        let gateway_too = "[gateway]\nurls = [\"wss://b\"]\n[metadata]\nmurtaugh_access = 2\n";
        let unchanged = outside_metadata(Some(before));
        assert!(unchanged.is_some());
        assert_eq!(outside_metadata(Some(metadata_only)), unchanged);
        assert_ne!(outside_metadata(Some(gateway_too)), unchanged);
    }
}
