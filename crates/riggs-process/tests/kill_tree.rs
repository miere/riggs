#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::panic,
    clippy::zombie_processes
)]

use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use nix::sys::signal::kill;
use nix::unistd::Pid;
use riggs_process::{Descendants, Leader};
use tokio::process::Command;

const ROLE: &str = "RIGGS_PROCESS_TEST_ROLE";
const PIDS: &str = "RIGGS_PROCESS_TEST_PIDS";
const HELPER: &str = "helper";

/// Not a test: the child processes below are this binary re-run as `helper` with a role.
#[test]
fn helper() {
    match std::env::var(ROLE).as_deref() {
        Ok("leader") => {
            spawn_self("escaper").spawn().unwrap();
            let _ = std::io::stdin().read_to_end(&mut Vec::new());
        }
        Ok("escaper") => {
            nix::unistd::setsid().unwrap();
            let sleeper = spawn_self("sleeper").spawn().unwrap();
            let pids = PathBuf::from(std::env::var(PIDS).unwrap());
            let partial = pids.with_extension("tmp");
            std::fs::write(
                &partial,
                format!("{}\n{}\n", std::process::id(), sleeper.id()),
            )
            .unwrap();
            std::fs::rename(partial, pids).unwrap();
            std::thread::sleep(Duration::from_secs(600));
        }
        Ok("sleeper") => std::thread::sleep(Duration::from_secs(600)),
        _ => {}
    }
}

fn spawn_self(role: &str) -> std::process::Command {
    let mut command = std::process::Command::new(std::env::current_exe().unwrap());
    command
        .args(["--exact", HELPER, "--test-threads=1"])
        .env(ROLE, role)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    command
}

fn alive(pid: i32) -> bool {
    kill(Pid::from_raw(pid), None).is_ok()
}

/// Polls, because what it waits for happens in other processes.
async fn eventually(what: &str, done: impl Fn() -> bool) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(20);
    while !done() {
        assert!(
            tokio::time::Instant::now() < deadline,
            "timed out waiting for {what}"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

async fn start(dir: &Path) -> (Leader, riggs_process::Pipes, Vec<i32>) {
    let pids = dir.join("pids");
    let mut command = Command::new(std::env::current_exe().unwrap());
    command
        .args(["--exact", HELPER, "--test-threads=1"])
        .env(ROLE, "leader")
        .env(PIDS, &pids);
    let (leader, pipes) = Leader::spawn(&mut command, 1024).unwrap();
    eventually("the escaped descendants to start", || pids.exists()).await;
    let escaped: Vec<i32> = std::fs::read_to_string(&pids)
        .unwrap()
        .lines()
        .map(|pid| pid.parse().unwrap())
        .collect();
    assert_eq!(escaped.len(), 2);
    assert!(escaped.iter().all(|pid| alive(*pid)));
    (leader, pipes, escaped)
}

#[tokio::test(flavor = "multi_thread")]
async fn kill_tree_reaches_descendants_in_another_session() {
    let dir = tempfile::tempdir().unwrap();
    let (mut leader, _pipes, escaped) = start(dir.path()).await;
    let leader_pid = i32::try_from(leader.pid()).unwrap();

    leader.kill_tree(Descendants::default()).await.unwrap();
    assert!(!alive(leader_pid), "the leader was not reaped");
    eventually("the escaped descendants to die", || {
        escaped.iter().all(|pid| !alive(*pid))
    })
    .await;
}

#[tokio::test(flavor = "multi_thread")]
async fn descendants_taken_before_the_leader_exits_are_killed_after_it() {
    let dir = tempfile::tempdir().unwrap();
    let (mut leader, pipes, escaped) = start(dir.path()).await;

    let known = leader.descendants().await;
    drop(pipes.stdin);
    assert!(leader.wait().await.unwrap().success());
    assert!(
        escaped.iter().all(|pid| alive(*pid)),
        "the scenario needs descendants that outlive their leader"
    );

    leader.kill_tree(known).await.unwrap();
    eventually("the orphaned descendants to die", || {
        escaped.iter().all(|pid| !alive(*pid))
    })
    .await;
}
