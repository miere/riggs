//! Confines the agent with macOS seatbelt, ported from the Go gateway's box.
//!
//! SBPL evaluates rules in order and the last matching rule wins, which is what makes "deny
//! broadly, then carve out narrowly" expressible: the write carve-outs must follow the blanket
//! write deny, or they are dead text.
//!
//! Only the agent is confined. The credential warden and the sign-in commands run outside, because
//! a boxed `claude` can read its credential but not write a refreshed one back, and a refresh that
//! cannot be saved destroys it.

use std::path::{Path, PathBuf};

/// The credential stores an agent is blinded to unless the profile names its own list. These are
/// the paths whose contents are directly reusable as someone else's identity.
const DENY_READ: [&str; 5] = [
    "~/.ssh",
    "~/.aws",
    "~/.config/gcloud",
    "~/.config/gh",
    "~/.netrc",
];
const SANDBOX_EXEC: &str = "/usr/bin/sandbox-exec";

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum SandboxMode {
    #[default]
    Off,
    Seatbelt,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SandboxConfig {
    pub mode: SandboxMode,
    /// Writable beyond the always-on set.
    pub write: Vec<PathBuf>,
    /// Unreadable; `None` takes the credential stores above, and an empty list blinds nothing.
    pub deny_read: Option<Vec<PathBuf>>,
    /// Never readable or writable, whatever else the profile says: it is this node's identity.
    pub node_token: Option<PathBuf>,
}

impl SandboxConfig {
    pub(crate) fn wraps(&self) -> bool {
        self.mode == SandboxMode::Seatbelt
    }

    /// The argv that runs `command` inside the box.
    pub(crate) fn wrap(&self, command: &Path, workdir: &Path) -> Vec<String> {
        vec![
            SANDBOX_EXEC.to_owned(),
            "-p".to_owned(),
            self.profile(workdir),
            command.display().to_string(),
        ]
    }

    /// The SBPL policy for this box.
    pub(crate) fn profile(&self, workdir: &Path) -> String {
        let mut out = String::from("(version 1)\n(allow default)\n\n");
        out.push_str("; writes: denied, then carved out for the agent's own surfaces\n");
        out.push_str("(deny file-write*)\n");
        for path in self.writable(workdir) {
            out.push_str(&format!("(allow file-write* {})\n", both(&path)));
        }
        // /dev stays writable or the agent dies at once: /dev/null and the tty are writes.
        out.push_str("(allow file-write* (subpath \"/dev\"))\n");
        let denied = self.deny_read.clone().unwrap_or_else(|| {
            DENY_READ
                .iter()
                .map(|path| PathBuf::from(expand(path)))
                .collect()
        });
        if !denied.is_empty() {
            out.push_str("\n; reads: everything but the credential stores\n");
            for path in denied {
                out.push_str(&format!("(deny file-read* {})\n", both(&path)));
            }
        }
        if let Some(token) = &self.node_token {
            out.push_str("\n; this node's credential: never readable or writable\n");
            out.push_str(&format!("(deny file-read* {})\n", both(token)));
            out.push_str(&format!("(deny file-write* {})\n", both(token)));
        }
        out
    }

    /// Each entry is load-bearing: the workspace, a temp dir the CLI refuses to run without, and
    /// Claude Code's own session state and settings file.
    fn writable(&self, workdir: &Path) -> Vec<PathBuf> {
        let mut raw = vec![workdir.to_path_buf()];
        if let Some(tmp) = std::env::var_os("TMPDIR") {
            raw.push(PathBuf::from(tmp));
        }
        raw.push(PathBuf::from("/tmp"));
        if let Some(home) = std::env::var_os("HOME") {
            let home = PathBuf::from(home);
            raw.push(home.join(".claude"));
            raw.push(home.join(".claude.json"));
        }
        raw.extend(self.write.iter().cloned());
        let mut seen = Vec::new();
        for path in raw {
            if !path.as_os_str().is_empty() && !seen.contains(&path) {
                seen.push(path);
            }
        }
        seen
    }
}

/// Both filter forms, which SBPL reads as a union, so a rule covers a directory or a plain file
/// without the caller having to know which one it was given.
fn both(path: &Path) -> String {
    let rendered = sbpl(&path.display().to_string());
    format!("(subpath {rendered}) (literal {rendered})")
}

/// A path holding a quote or a backslash is pathological, but it must not be able to end the
/// literal early and turn the rest of itself into policy.
fn sbpl(text: &str) -> String {
    let mut out = String::with_capacity(text.len() + 2);
    out.push('"');
    for ch in text.chars() {
        if ch == '"' || ch == '\\' {
            out.push('\\');
        }
        out.push(ch);
    }
    out.push('"');
    out
}

fn expand(path: &str) -> String {
    match (path.strip_prefix("~/"), std::env::var("HOME")) {
        (Some(rest), Ok(home)) => format!("{home}/{rest}"),
        _ => path.to_owned(),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use super::*;

    fn seatbelt() -> SandboxConfig {
        SandboxConfig {
            mode: SandboxMode::Seatbelt,
            ..SandboxConfig::default()
        }
    }

    #[test]
    fn the_workspace_is_writable_and_everything_else_is_not() {
        let profile = seatbelt().profile(Path::new("/work/here"));
        let deny = profile.find("(deny file-write*)").unwrap();
        let allow = profile
            .find(r#"(allow file-write* (subpath "/work/here")"#)
            .unwrap();
        assert!(deny < allow, "a carve-out before the deny is dead text");
        assert!(profile.contains(r#"(allow file-write* (subpath "/tmp")"#));
        assert!(profile.contains(r#"(allow file-write* (subpath "/dev"))"#));
    }

    #[test]
    fn the_credential_stores_are_blinded_and_can_be_overridden() {
        let profile = seatbelt().profile(Path::new("/work"));
        assert!(profile.contains(".ssh"));
        assert!(profile.contains(".config/gcloud"));

        let named = SandboxConfig {
            deny_read: Some(vec![PathBuf::from("/secrets")]),
            ..seatbelt()
        };
        let profile = named.profile(Path::new("/work"));
        assert!(profile.contains(r#"(deny file-read* (subpath "/secrets")"#));
        assert!(
            !profile.contains(".ssh"),
            "a named list replaces the default"
        );

        let blind_nothing = SandboxConfig {
            deny_read: Some(vec![]),
            ..seatbelt()
        };
        assert!(
            !blind_nothing
                .profile(Path::new("/work"))
                .contains("file-read*")
        );
    }

    #[test]
    fn the_nodes_own_credential_is_denied_both_ways() {
        let config = SandboxConfig {
            node_token: Some(PathBuf::from("/work/node-token")),
            ..seatbelt()
        };
        let profile = config.profile(Path::new("/work"));
        assert!(profile.contains(r#"(deny file-read* (subpath "/work/node-token")"#));
        assert!(profile.contains(r#"(deny file-write* (subpath "/work/node-token")"#));
        let token = profile.find("; this node's credential").unwrap();
        let writes = profile
            .find("(allow file-write* (subpath \"/work\")")
            .unwrap();
        assert!(writes < token, "the token deny must win over the workspace");
    }

    #[test]
    fn a_path_cannot_break_out_of_its_own_policy_line() {
        let config = SandboxConfig {
            write: vec![PathBuf::from(
                r#"/tmp/od")) (allow file-write* (subpath "/"#,
            )],
            ..seatbelt()
        };
        let profile = config.profile(Path::new("/work"));
        let escaped = r#"(subpath "/tmp/od\")) (allow file-write* (subpath \"/")"#;
        assert!(profile.contains(escaped), "{profile}");
    }

    /// The policy is only worth writing if macOS will take it.
    #[cfg(target_os = "macos")]
    #[test]
    fn macos_accepts_the_policy_this_builds() {
        let config = SandboxConfig {
            write: vec![PathBuf::from("/tmp/riggs-test")],
            node_token: Some(PathBuf::from("/tmp/riggs-test/node-token")),
            ..seatbelt()
        };
        let wrapper = config.wrap(Path::new("/usr/bin/true"), Path::new("/tmp/riggs-test"));
        let (first, rest) = wrapper.split_first().unwrap();
        let run = std::process::Command::new(first)
            .args(rest)
            .output()
            .unwrap();
        let complaint = String::from_utf8_lossy(&run.stderr).into_owned();
        if complaint.contains("sandbox_apply") {
            // Already inside someone else's box, which cannot nest another. The policy itself
            // was read and accepted before this point.
            return;
        }
        assert!(
            run.status.success(),
            "sandbox-exec refused the policy: {complaint}"
        );
    }

    #[test]
    fn a_box_that_is_off_wraps_nothing() {
        assert!(!SandboxConfig::default().wraps());
        assert!(seatbelt().wraps());
        assert_eq!(
            seatbelt().wrap(Path::new("/bin/claude"), Path::new("/work"))[..2],
            [SANDBOX_EXEC.to_owned(), "-p".to_owned()]
        );
    }
}
