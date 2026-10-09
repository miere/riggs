//! Files this node agreed to hand a gateway. A gateway tool never gets a path: it gets an
//! identifier minted here, which only the session's gateway can redeem, once, and only while the
//! call that carried it is open. So no gateway can browse this machine.

use std::collections::HashMap;
use std::fs::File;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use rax::attachment::MAX_ATTACHMENT_BYTES;
use rax::id::LocalFileId;
use rax::local_file::replace_local_files;
use serde_json::Value;
use uuid::Uuid;

use crate::state::{GatewayName, Shared, lock};

/// Why a path was not handed over. Worded for the agent, which can often fix the path and call
/// again.
#[derive(Debug, thiserror::Error)]
pub enum FileRefused {
    #[error("a file path is required")]
    Empty,
    #[error("the working directory cannot be read: {0}")]
    Root(std::io::Error),
    #[error("{path} cannot be opened: {source}")]
    Unreadable {
        path: String,
        source: std::io::Error,
    },
    #[error("{0} is outside the working directory, so it cannot be sent")]
    Outside(String),
    #[error("{0} is a directory")]
    Directory(String),
    #[error("{0} is not a regular file")]
    NotAFile(String),
    #[error("{0} is empty")]
    EmptyFile(String),
    #[error("{0} is larger than the 100 MiB limit")]
    TooLarge(String),
}

/// Opened when it is offered, so what the gateway reads is the file that was checked, whatever
/// happens to the path afterwards.
pub(crate) struct Offered {
    gateway: GatewayName,
    pub(crate) file: File,
    pub(crate) size: u64,
    pub(crate) name: Option<String>,
    pub(crate) mimetype: Option<&'static str>,
}

#[derive(Default)]
pub(crate) struct LocalFiles {
    offered: Mutex<HashMap<LocalFileId, Offered>>,
}

impl LocalFiles {
    /// Hands the file over and forgets it. An identifier offered to another gateway is as unknown
    /// as one never minted.
    pub(crate) fn redeem(&self, id: &LocalFileId, gateway: &str) -> Option<Offered> {
        let mut offered = lock(&self.offered);
        if offered.get(id)?.gateway != gateway {
            return None;
        }
        offered.remove(id)
    }
}

/// The files one tool call offered. Dropping it withdraws whatever the gateway did not read, so
/// an identifier never outlives its call.
pub(crate) struct Offer {
    shared: Arc<Shared>,
    ids: Vec<LocalFileId>,
}

impl Drop for Offer {
    fn drop(&mut self) {
        let mut offered = lock(&self.shared.local_files.offered);
        for id in &self.ids {
            offered.remove(id);
        }
    }
}

/// Swaps every argument `schema` marks as a local file for an identifier `gateway` can redeem
/// while the returned offer lives. One refused path refuses the whole call.
pub(crate) async fn offer(
    shared: &Arc<Shared>,
    gateway: &GatewayName,
    schema: &Value,
    arguments: &mut Value,
    root: &Path,
) -> Result<Offer, FileRefused> {
    let (schema, root, owner) = (schema.clone(), root.to_owned(), gateway.clone());
    let mut rewritten = arguments.take();
    let checked = tokio::task::spawn_blocking(move || {
        let root = root.canonicalize().map_err(FileRefused::Root)?;
        let mut files = Vec::new();
        replace_local_files(&schema, &mut rewritten, &mut |path| {
            let offered = open(&root, path, &owner)?;
            let id = LocalFileId(format!("lf_{}", Uuid::new_v4().simple()));
            files.push((id.clone(), offered));
            Ok(id)
        })?;
        Ok((rewritten, files))
    })
    .await
    .unwrap_or_else(|panicked| Err(FileRefused::Root(std::io::Error::other(panicked))));
    let (rewritten, files) = checked?;
    *arguments = rewritten;
    let ids = files.iter().map(|(id, _)| id.clone()).collect();
    lock(&shared.local_files.offered).extend(files);
    Ok(Offer {
        shared: shared.clone(),
        ids,
    })
}

fn open(root: &Path, path: &str, gateway: &GatewayName) -> Result<Offered, FileRefused> {
    if path.trim().is_empty() {
        return Err(FileRefused::Empty);
    }
    let shown = || path.to_owned();
    let unreadable = |source| FileRefused::Unreadable {
        path: shown(),
        source,
    };
    let candidate = if Path::new(path).is_absolute() {
        PathBuf::from(path)
    } else {
        root.join(path)
    };
    let resolved = candidate.canonicalize().map_err(unreadable)?;
    if !resolved.starts_with(root) {
        return Err(FileRefused::Outside(shown()));
    }
    let file = File::open(&resolved).map_err(unreadable)?;
    let meta = file.metadata().map_err(unreadable)?;
    if meta.is_dir() {
        return Err(FileRefused::Directory(shown()));
    }
    if !meta.is_file() {
        return Err(FileRefused::NotAFile(shown()));
    }
    if meta.len() == 0 {
        return Err(FileRefused::EmptyFile(shown()));
    }
    if meta.len() > MAX_ATTACHMENT_BYTES {
        return Err(FileRefused::TooLarge(shown()));
    }
    Ok(Offered {
        gateway: gateway.clone(),
        file,
        size: meta.len(),
        name: resolved
            .file_name()
            .map(|name| name.to_string_lossy().into_owned()),
        mimetype: mimetype(&resolved),
    })
}

fn mimetype(path: &Path) -> Option<&'static str> {
    let extension = path.extension()?.to_str()?.to_ascii_lowercase();
    Some(match extension.as_str() {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "pdf" => "application/pdf",
        "json" => "application/json",
        "zip" => "application/zip",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        "md" => "text/markdown",
        "txt" | "log" => "text/plain",
        _ => return None,
    })
}
