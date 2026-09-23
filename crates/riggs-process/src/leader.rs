use std::io;
use std::process::{ExitStatus, Stdio};

use tokio::process::{Child, ChildStdin, ChildStdout, Command};

use crate::tail::Tail;
use crate::tree;

pub struct Pipes {
    pub stdin: ChildStdin,
    pub stdout: ChildStdout,
    pub stderr: Tail,
}

/// Only a walk makes these, so `kill_tree` never signals a pid that was not part of the tree.
#[derive(Debug, Default)]
pub struct Descendants(pub(crate) Vec<i32>);

/// Leads its own process group, so one signal reaches everything that stayed in it. Dropping it
/// kills only the leader, which is why every planned stop goes through `kill_tree`.
pub struct Leader {
    child: Child,
    pid: u32,
}

impl Leader {
    pub fn spawn(command: &mut Command, stderr_bytes: usize) -> io::Result<(Self, Pipes)> {
        Self::spawn_tapped(command, stderr_bytes, |_| {})
    }

    /// The same, with the child's stderr also handed to `tap` chunk by chunk. A flow that says
    /// what it wants on stderr — `gcloud` prints its sign-in link there — cannot be read from the
    /// tail alone, because the tail is only worth reading once the child has given up.
    pub fn spawn_tapped(
        command: &mut Command,
        stderr_bytes: usize,
        tap: impl FnMut(&[u8]) + Send + 'static,
    ) -> io::Result<(Self, Pipes)> {
        let mut child = command
            .process_group(0)
            .kill_on_drop(true)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()?;
        let (Some(pid), Some(stdin), Some(stdout), Some(stderr)) = (
            child.id(),
            child.stdin.take(),
            child.stdout.take(),
            child.stderr.take(),
        ) else {
            return Err(io::Error::other(
                "the child's standard streams were not piped",
            ));
        };
        let pipes = Pipes {
            stdin,
            stdout,
            stderr: Tail::read(stderr, stderr_bytes, pid, tap),
        };
        Ok((Self { child, pid }, pipes))
    }

    pub fn pid(&self) -> u32 {
        self.pid
    }

    pub async fn wait(&mut self) -> io::Result<ExitStatus> {
        self.child.wait().await
    }

    /// Take this before asking the leader to exit, for example by closing its stdin. Once it has
    /// gone, its children belong to init and no walk can tell they were part of the tree.
    pub async fn descendants(&self) -> Descendants {
        let Ok(root) = i32::try_from(self.pid) else {
            return Descendants::default();
        };
        let found = tokio::task::spawn_blocking(move || tree::descendants(&[root])).await;
        Descendants(found.unwrap_or_default())
    }

    /// Stops the whole tree before killing it, so nothing forks between the walk and the kill,
    /// then reaps the leader. Safe to call after the leader has exited on its own.
    pub async fn kill_tree(&mut self, known: Descendants) -> io::Result<ExitStatus> {
        let group = i32::try_from(self.pid).ok();
        let running = group.filter(|_| self.child.id().is_some());
        let killed = tokio::task::spawn_blocking(move || tree::kill(running, group, known.0)).await;
        if killed.is_err() {
            let _ = self.child.start_kill();
        }
        self.child.wait().await
    }
}
