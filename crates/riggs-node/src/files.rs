use std::io;
use std::path::{Path, PathBuf};

use rax::attachment::MAX_ATTACHMENT_BYTES;
use rax::content::ContentBlock;
use rax::id::SessionId;
use rax::open::{Subject, UnhandledReason};
use rax::resource::ReadResource;
use rax::{ErrorKind, Open, Unhandled};
use rax_tokio::CallError;
use rax_tokio::node::{NodeHandle, Resource};
use tokio::io::AsyncWriteExt;
use tokio_util::sync::CancellationToken;
use url::Url;

use crate::backend::SessionKey;

const MAX_NAME: usize = 100;

pub(crate) struct Fetched {
    pub(crate) content: Vec<Open<ContentBlock>>,
    pub(crate) unhandled: Vec<Unhandled>,
}

struct Failure {
    reason: UnhandledReason,
    message: String,
}

pub(crate) struct Files {
    dir: PathBuf,
}

impl Files {
    pub(crate) fn new(dir: PathBuf, ephemeral: bool) -> Self {
        let files = Self { dir };
        if ephemeral {
            files.remove(&files.dir);
        }
        files
    }

    pub(crate) async fn fetch(
        &self,
        handle: &NodeHandle,
        readable: &[String],
        key: &SessionKey,
        blocks: Vec<Open<ContentBlock>>,
        cancelled: &CancellationToken,
    ) -> Fetched {
        let mut fetched = Fetched {
            content: Vec::with_capacity(blocks.len()),
            unhandled: Vec::new(),
        };
        for (index, block) in blocks.into_iter().enumerate() {
            let Open::Known(ContentBlock::ResourceLink {
                uri,
                name,
                mime_type,
                title,
                description,
                ..
            }) = &block
            else {
                fetched.content.push(block);
                continue;
            };
            if !is_readable(uri, readable) {
                fetched.content.push(block);
                continue;
            }
            let (uri, name) = (uri.clone(), name.clone());
            let (mime_type, title, description) =
                (mime_type.clone(), title.clone(), description.clone());
            let read = tokio::select! {
                read = self.read(handle, key, &uri, &name) => read,
                () = cancelled.cancelled() => Err(Failure {
                    reason: UnhandledReason::Other,
                    message: "the turn was cancelled before the file arrived".to_owned(),
                }),
            };
            match read {
                Ok((path, resource)) => {
                    fetched
                        .content
                        .push(Open::Known(ContentBlock::ResourceLink {
                            uri: file_url(&path),
                            name,
                            mime_type: resource.mimetype.or(mime_type),
                            title,
                            description,
                            size: Some(resource.bytes.len() as u64),
                        }))
                }
                Err(Failure { reason, message }) => {
                    tracing::warn!(session_id = %key, %uri, %message, "could not fetch a linked file");
                    fetched.content.push(Open::Known(ContentBlock::text(format!(
                        "[{name}: the file could not be fetched: {message}]"
                    ))));
                    fetched.unhandled.push(Unhandled {
                        subject: Subject::Block { index },
                        reason,
                        message: Some(message),
                    });
                }
            }
        }
        fetched
    }

    async fn read(
        &self,
        handle: &NodeHandle,
        key: &SessionKey,
        uri: &str,
        name: &str,
    ) -> Result<(PathBuf, Resource), Failure> {
        let read = ReadResource {
            uri: uri.to_owned(),
            max_bytes: Some(MAX_ATTACHMENT_BYTES),
        };
        let resource = handle.read_resource(read).await.map_err(refused)?;
        let dir = self.dir.join(key.to_string());
        let path = dir.join(format!("{}-{}", resource.transfer_id, safe_name(name)));
        store(&dir, &path, &resource.bytes)
            .await
            .map_err(|err| Failure {
                reason: UnhandledReason::Other,
                message: format!("this node could not save it: {err}"),
            })?;
        Ok((path, resource))
    }

    pub(crate) fn discard(&self, key: &SessionKey) {
        self.remove(&self.dir.join(key.to_string()));
    }

    pub(crate) fn sessions(&self) -> Vec<SessionKey> {
        let Ok(entries) = std::fs::read_dir(&self.dir) else {
            return Vec::new();
        };
        entries
            .flatten()
            .filter_map(|entry| entry.file_name().into_string().ok())
            .filter_map(|name| SessionKey::parse(&SessionId(name)))
            .collect()
    }

    fn remove(&self, path: &Path) {
        match std::fs::remove_dir_all(path) {
            Ok(()) => {}
            Err(err) if err.kind() == io::ErrorKind::NotFound => {}
            Err(err) => {
                tracing::warn!(path = %path.display(), error = %err, "could not remove fetched files");
            }
        }
    }
}

fn is_readable(uri: &str, readable: &[String]) -> bool {
    uri.split_once(':')
        .is_some_and(|(scheme, _)| readable.iter().any(|known| known == scheme))
}

fn refused(err: CallError) -> Failure {
    let (reason, message) = match err {
        CallError::Fault(fault) => {
            let reason = match fault.kind {
                ErrorKind::Forbidden => UnhandledReason::Forbidden,
                ErrorKind::Unsupported => UnhandledReason::UnsupportedScheme,
                ErrorKind::TooLarge => UnhandledReason::Other,
                _ => UnhandledReason::Unreachable,
            };
            (reason, fault.message)
        }
        other => (UnhandledReason::Unreachable, other.to_string()),
    };
    Failure { reason, message }
}

fn safe_name(name: &str) -> String {
    let cleaned: String = name
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_') {
                c
            } else {
                '_'
            }
        })
        .take(MAX_NAME)
        .collect();
    let trimmed = cleaned.trim_start_matches('.');
    if trimmed.is_empty() {
        "file".to_owned()
    } else {
        trimmed.to_owned()
    }
}

fn file_url(path: &Path) -> String {
    Url::from_file_path(path)
        .map(String::from)
        .unwrap_or_else(|()| format!("file://{}", path.display()))
}

async fn store(dir: &Path, path: &Path, bytes: &[u8]) -> io::Result<()> {
    tokio::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .await?;
    let mut file = tokio::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)
        .await?;
    file.write_all(bytes).await?;
    file.flush().await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_cannot_escape_the_session_folder() {
        assert_eq!(safe_name("../../etc/passwd"), "_.._etc_passwd");
        assert_eq!(safe_name("..."), "file");
        assert_eq!(safe_name("Q3 report.pdf"), "Q3_report.pdf");
        assert_eq!(safe_name(&"a".repeat(300)).len(), MAX_NAME);
    }

    #[test]
    fn only_schemes_the_gateway_serves_are_fetched() {
        let readable = ["gateway".to_owned()];
        assert!(is_readable("gateway://files/F1", &readable));
        assert!(!is_readable("chat://workspace/C1/1.2", &readable));
        assert!(!is_readable("gatewayish", &readable));
    }
}
