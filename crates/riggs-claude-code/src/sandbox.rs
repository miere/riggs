//! Confines the agent with macOS seatbelt, ported from the Go gateway's box.
//!
//! SBPL evaluates rules in order and the last matching rule wins, which is what makes "deny
//! broadly, then carve out narrowly" expressible: the write carve-outs must follow the blanket
//! write deny, or they are dead text.
//!
//! Only the agent is confined. The credential warden and the sign-in commands run outside, because
//! a boxed `claude` can read its credential but not write a refreshed one back, and a refresh that
//! cannot be saved destroys it.
//!
//! That holds for a keychain entry by itself. A credential kept in a file sits inside Claude Code's
//! own directory, which the box must leave writable, so the file gets a write deny of its own.

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
    /// Never readable or writable, whatever else the profile says: they are this node's identity,
    /// one per gateway.
    pub node_tokens: Vec<PathBuf>,
}

impl SandboxConfig {
    pub(crate) fn wraps(&self) -> bool {
        self.mode == SandboxMode::Seatbelt
    }

    /// The argv that runs `command` inside the box.
    pub(crate) fn wrap(
        &self,
        command: &Path,
        workdir: &Path,
        credential: Option<&Path>,
    ) -> Vec<String> {
        vec![
            SANDBOX_EXEC.to_owned(),
            "-p".to_owned(),
            self.profile(workdir, credential),
            command.display().to_string(),
        ]
    }

    /// The SBPL policy for this box. `credential` is the agent's own login when it is a file: the
    /// agent may read it, and only the warden and the sign-ins, outside the box, may write it.
    pub(crate) fn profile(&self, workdir: &Path, credential: Option<&Path>) -> String {
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
        if let Some(credential) = credential {
            out.push_str("\n; the agent's own login: readable, never writable\n");
            out.push_str(&format!("(deny file-write* {})\n", both(credential)));
        }
        if !self.node_tokens.is_empty() {
            out.push_str("\n; this node's credentials: never readable or writable\n");
        }
        for token in &self.node_tokens {
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

/// The kernel checks the path a write actually resolves to, so a rule naming a symlink matches
/// nothing: `~/Development` pointing at another volume would leave the workspace unwritable.
///
/// A file that does not exist yet is resolved through its folder, so a rule written before the
/// file is still names where it will land.
fn real(path: &Path) -> PathBuf {
    if let Ok(resolved) = std::fs::canonicalize(path) {
        return resolved;
    }
    match (path.parent(), path.file_name()) {
        (Some(dir), Some(name)) => match std::fs::canonicalize(dir) {
            Ok(dir) => dir.join(name),
            Err(_) => path.to_path_buf(),
        },
        _ => path.to_path_buf(),
    }
}

/// Both filter forms, which SBPL reads as a union, so a rule covers a directory or a plain file
/// without the caller having to know which one it was given.
fn both(path: &Path) -> String {
    let rendered = sbpl(&real(path).display().to_string());
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
        let profile = seatbelt().profile(Path::new("/work/here"), None);
        let deny = profile.find("(deny file-write*)").unwrap();
        let allow = profile
            .find(r#"(allow file-write* (subpath "/work/here")"#)
            .unwrap();
        assert!(deny < allow, "a carve-out before the deny is dead text");
        // /tmp is itself a symlink on macOS, so the rule names what it resolves to.
        let tmp = real(Path::new("/tmp"));
        assert!(
            profile.contains(&format!(
                "(allow file-write* (subpath \"{}\")",
                tmp.display()
            )),
            "{profile}"
        );
        assert!(profile.contains(r#"(allow file-write* (subpath "/dev"))"#));
    }

    #[test]
    fn the_credential_stores_are_blinded_and_can_be_overridden() {
        let profile = seatbelt().profile(Path::new("/work"), None);
        assert!(profile.contains(".ssh"));
        assert!(profile.contains(".config/gcloud"));

        let named = SandboxConfig {
            deny_read: Some(vec![PathBuf::from("/secrets")]),
            ..seatbelt()
        };
        let profile = named.profile(Path::new("/work"), None);
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
                .profile(Path::new("/work"), None)
                .contains("file-read*")
        );
    }

    #[test]
    fn the_nodes_own_credential_is_denied_both_ways() {
        let config = SandboxConfig {
            node_tokens: vec![PathBuf::from("/work/node-token")],
            ..seatbelt()
        };
        let profile = config.profile(Path::new("/work"), None);
        assert!(profile.contains(r#"(deny file-read* (subpath "/work/node-token")"#));
        assert!(profile.contains(r#"(deny file-write* (subpath "/work/node-token")"#));
        let token = profile.find("; this node's credential").unwrap();
        let writes = profile
            .find("(allow file-write* (subpath \"/work\")")
            .unwrap();
        assert!(writes < token, "the token deny must win over the workspace");
    }

    #[test]
    fn a_rule_names_the_path_a_write_really_lands_on() {
        let dir = tempfile::tempdir().unwrap();
        let real_dir = dir.path().join("real");
        let link = dir.path().join("link");
        std::fs::create_dir(&real_dir).unwrap();
        std::os::unix::fs::symlink(&real_dir, &link).unwrap();
        let profile = seatbelt().profile(&link, None);
        let resolved = std::fs::canonicalize(&real_dir).unwrap();
        assert!(
            profile.contains(&format!("(subpath \"{}\")", resolved.display())),
            "{profile}"
        );
        assert!(!profile.contains(&format!("(subpath \"{}\")", link.display())));
    }

    #[test]
    fn a_path_cannot_break_out_of_its_own_policy_line() {
        let config = SandboxConfig {
            write: vec![PathBuf::from(
                r#"/nowhere/od")) (allow file-write* (subpath "/"#,
            )],
            ..seatbelt()
        };
        let profile = config.profile(Path::new("/work"), None);
        let escaped = r#"(subpath "/nowhere/od\")) (allow file-write* (subpath \"/")"#;
        assert!(profile.contains(escaped), "{profile}");
    }

    #[test]
    fn the_agents_own_login_is_readable_and_never_writable() {
        let credential = Path::new("/work/.claude/.credentials.json");
        let profile = seatbelt().profile(Path::new("/work"), Some(credential));
        let deny = profile
            .find(r#"(deny file-write* (subpath "/work/.claude/.credentials.json")"#)
            .unwrap();
        let writes = profile
            .find("(allow file-write* (subpath \"/work\")")
            .unwrap();
        assert!(
            writes < deny,
            "the deny must win over the folder it sits in"
        );
        assert!(
            !profile.contains(r#"(deny file-read* (subpath "/work/.claude/.credentials.json")"#),
            "the agent still has to read its login"
        );
        assert!(
            !seatbelt()
                .profile(Path::new("/work"), None)
                .contains("the agent's own login")
        );
    }

    /// Before the first sign-in there is no file to resolve, and the rule must still name the
    /// place the sign-in will write it.
    #[test]
    fn a_file_that_is_not_there_yet_is_named_through_its_real_folder() {
        let dir = tempfile::tempdir().unwrap();
        let real_dir = dir.path().join("real");
        let link = dir.path().join("link");
        std::fs::create_dir(&real_dir).unwrap();
        std::os::unix::fs::symlink(&real_dir, &link).unwrap();
        let resolved = std::fs::canonicalize(&real_dir).unwrap();
        assert_eq!(real(&link.join("missing")), resolved.join("missing"));
        assert_eq!(
            real(Path::new("/no/such/folder/file")),
            Path::new("/no/such/folder/file")
        );
    }

    /// The rule as the kernel applies it: inside a real box, in a writable folder.
    #[cfg(target_os = "macos")]
    #[test]
    fn a_boxed_process_can_read_its_login_and_cannot_change_it() {
        let dir = tempfile::tempdir().unwrap();
        let home = std::fs::canonicalize(dir.path()).unwrap();
        let credential = home.join(".credentials.json");
        std::fs::write(&credential, "original").unwrap();
        let config = SandboxConfig {
            write: vec![home.clone()],
            deny_read: Some(vec![]),
            ..seatbelt()
        };
        let boxed = |script: &str| {
            let wrapper = config.wrap(Path::new("/bin/sh"), &home, Some(&credential));
            let (first, rest) = wrapper.split_first().unwrap();
            std::process::Command::new(first)
                .args(rest)
                .args(["-c", script])
                .current_dir(&home)
                .output()
                .unwrap()
        };
        let probe = boxed("true");
        if String::from_utf8_lossy(&probe.stderr).contains("sandbox_apply") {
            // Already inside someone else's box, which cannot nest another.
            return;
        }
        assert!(probe.status.success());

        let read = boxed("cat .credentials.json");
        assert_eq!(String::from_utf8_lossy(&read.stdout), "original");
        assert!(boxed("echo fine > neighbour").status.success());
        for attempt in [
            "echo forged > .credentials.json",
            "echo forged >> .credentials.json",
            "echo forged > other && mv other .credentials.json",
            "rm .credentials.json",
            "mv .credentials.json elsewhere",
        ] {
            assert!(!boxed(attempt).status.success(), "{attempt} got through");
            assert_eq!(
                std::fs::read_to_string(&credential).unwrap(),
                "original",
                "{attempt} changed the login"
            );
        }
    }

    /// The policy is only worth writing if macOS will take it.
    #[cfg(target_os = "macos")]
    #[test]
    fn macos_accepts_the_policy_this_builds() {
        let config = SandboxConfig {
            write: vec![PathBuf::from("/tmp/riggs-test")],
            node_tokens: vec![PathBuf::from("/tmp/riggs-test/node-token")],
            ..seatbelt()
        };
        let wrapper = config.wrap(
            Path::new("/usr/bin/true"),
            Path::new("/tmp/riggs-test"),
            Some(Path::new("/tmp/riggs-test/.credentials.json")),
        );
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
            seatbelt().wrap(Path::new("/bin/claude"), Path::new("/work"), None)[..2],
            [SANDBOX_EXEC.to_owned(), "-p".to_owned()]
        );
    }
}
