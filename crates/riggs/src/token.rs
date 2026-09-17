use std::fmt;
use std::io;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

pub const PREFIX: &str = "mrtg_node_";
const SELECTOR_LEN: usize = 16;
pub const MINT_HINT: &str = "mint one on the gateway with `murtaugh-gateway node token mint`, then write it to that path with mode 0600";

#[derive(Debug, thiserror::Error)]
pub enum TokenError {
    #[error("cannot read the node token {path}: {source}; {MINT_HINT}")]
    Unreadable {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    /// The mode is shown so the operator can see what to fix; the content never is.
    #[error(
        "the node token {path} has mode {mode:04o}, which other users can read; run `chmod 600 {path}`"
    )]
    Exposed { path: PathBuf, mode: u32 },
    #[error("the node token {path} is empty; {MINT_HINT}")]
    Empty { path: PathBuf },
    #[error("the node token {path} is still a placeholder; {MINT_HINT}")]
    Placeholder { path: PathBuf },
    #[error("the node token {path} is malformed: {reason}; {MINT_HINT}")]
    Malformed { path: PathBuf, reason: &'static str },
}

/// Never printed: `Debug` hides the value, and only the dialler reads it.
#[derive(Clone, PartialEq, Eq)]
pub struct NodeToken(String);

impl fmt::Debug for NodeToken {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("NodeToken(<redacted>)")
    }
}

/// Lets the dialler notice a rotated token without keeping the rejected one around.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Fingerprint([u8; 32]);

impl NodeToken {
    pub fn expose(&self) -> &str {
        &self.0
    }

    pub fn fingerprint(&self) -> Fingerprint {
        Fingerprint(Sha256::digest(self.0.as_bytes()).into())
    }
}

/// A blank value or a `*-replace-me` seed counts as unset, so a copied example never passes.
pub fn is_configured(value: &str) -> bool {
    let value = value.trim();
    !value.is_empty() && !value.ends_with("-replace-me")
}

pub fn read(path: &Path) -> Result<NodeToken, TokenError> {
    let unreadable = |source| TokenError::Unreadable {
        path: path.to_path_buf(),
        source,
    };
    let mode = std::fs::metadata(path)
        .map_err(unreadable)?
        .permissions()
        .mode()
        & 0o7777;
    if mode & 0o077 != 0 {
        return Err(TokenError::Exposed {
            path: path.to_path_buf(),
            mode,
        });
    }
    let content = std::fs::read_to_string(path).map_err(unreadable)?;
    let content = content.trim();
    if content.is_empty() {
        return Err(TokenError::Empty {
            path: path.to_path_buf(),
        });
    }
    if !is_configured(content) {
        return Err(TokenError::Placeholder {
            path: path.to_path_buf(),
        });
    }
    parse(content)
        .map(|()| NodeToken(content.to_owned()))
        .map_err(|reason| TokenError::Malformed {
            path: path.to_path_buf(),
            reason,
        })
}

fn parse(token: &str) -> Result<(), &'static str> {
    let rest = token
        .strip_prefix(PREFIX)
        .ok_or("it does not start with mrtg_node_")?;
    let (selector, secret) = rest
        .split_once('_')
        .ok_or("it has no secret after the selector")?;
    if selector.len() != SELECTOR_LEN || !selector.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err("its selector is not 16 hex characters");
    }
    if secret.is_empty() {
        return Err("its secret is empty");
    }
    if secret.chars().any(char::is_whitespace) {
        return Err("it contains whitespace");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

    use std::fs::Permissions;

    use super::*;

    const TOKEN: &str = "mrtg_node_0123456789abcdef_c2VjcmV0_c2VjcmV0-LXNlY3JldC1zZWNyZXQtc2Vj";

    fn file(content: &str, mode: u32) -> (tempfile::TempDir, PathBuf) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("node-token");
        std::fs::write(&path, content).unwrap();
        std::fs::set_permissions(&path, Permissions::from_mode(mode)).unwrap();
        (dir, path)
    }

    #[test]
    fn a_private_well_formed_token_is_read_and_trimmed() {
        let (_dir, path) = file(&format!("  {TOKEN}\n"), 0o600);
        assert_eq!(read(&path).unwrap().expose(), TOKEN);
    }

    #[test]
    fn a_token_others_can_read_is_refused_naming_the_mode_but_not_the_content() {
        let (_dir, path) = file(TOKEN, 0o644);
        let message = read(&path).unwrap_err().to_string();
        assert!(message.contains("0644"), "{message}");
        assert!(!message.contains("c2VjcmV0"), "{message}");
    }

    #[test]
    fn empty_placeholder_and_missing_files_carry_the_mint_hint() {
        let (_dir, empty) = file("\n  \n", 0o600);
        let (_dir2, placeholder) = file("mrtg_node_token-replace-me", 0o600);
        let missing = empty.with_file_name("absent");
        for path in [empty, placeholder, missing] {
            let message = read(&path).unwrap_err().to_string();
            assert!(message.contains("node token mint"), "{message}");
        }
    }

    #[test]
    fn malformed_tokens_are_refused_without_quoting_them() {
        for bad in [
            "xrtg_node_0123456789abcdef_secret",
            "mrtg_node_0123456789abcdef",
            "mrtg_node_0123456789abcde_secret",
            "mrtg_node_0123456789abcdeg_secret",
            "mrtg_node_0123456789abcdef_",
            "mrtg_node_0123456789abcdef_sec ret",
        ] {
            let (_dir, path) = file(bad, 0o600);
            let message = read(&path).unwrap_err().to_string();
            assert!(message.contains("malformed"), "{bad}: {message}");
            assert!(!message.contains("0123456789abcde"), "{message}");
        }
    }

    #[test]
    fn the_secret_may_contain_underscores() {
        assert_eq!(parse(TOKEN), Ok(()));
    }

    #[test]
    fn fingerprints_differ_only_when_the_token_does() {
        let (_dir, first) = file(TOKEN, 0o600);
        let (_dir2, same) = file(&format!("{TOKEN}\n"), 0o600);
        let (_dir3, other) = file("mrtg_node_0123456789abcdef_other", 0o600);
        let fingerprint = |path: &Path| read(path).unwrap().fingerprint();
        assert_eq!(fingerprint(&first), fingerprint(&same));
        assert_ne!(fingerprint(&first), fingerprint(&other));
    }

    #[test]
    fn debug_output_hides_the_token() {
        let (_dir, path) = file(TOKEN, 0o600);
        assert!(!format!("{:?}", read(&path).unwrap()).contains("mrtg"));
    }
}
