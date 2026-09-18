use std::fs::Permissions;
use std::io::Read;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use nix::sys::signal::{Signal, kill};
use nix::unistd::Pid;
use rax_sim::{Handshake, SimConfig, SimNode, Simulator};
use rax_tokio::gateway::GatewayConfig;
use serde_json::{Value, json};
use tempfile::TempDir;
use tokio::time::Instant;

use crate::support::{TOKEN, all_caps};

pub const WAIT: Duration = Duration::from_secs(30);
pub const SECRET: &str = "c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I";
pub const OTHER_TOKEN: &str =
    "mrtg_node_fedcba9876543210_b3RoZXItb3RoZXItb3RoZXItb3RoZXItb3RoZXItb3Ro";
pub const OTHER_SECRET: &str = "b3RoZXItb3RoZXItb3RoZXItb3RoZXItb3RoZXItb3Ro";

/// Cargo only exposes `CARGO_BIN_EXE_*` for the test's own package, so the daemon is built through
/// Cargo itself; with the workspace already built this is a no-op.
pub fn riggs_binary() -> &'static Path {
    static BINARY: OnceLock<PathBuf> = OnceLock::new();
    BINARY.get_or_init(|| {
        let manifest = Path::new(env!("CARGO_MANIFEST_DIR")).join("../../Cargo.toml");
        escargot::CargoBuild::new()
            .manifest_path(manifest)
            .package("riggs")
            .bin("riggs")
            .current_release()
            .current_target()
            .run()
            .expect("build the riggs binary")
            .path()
            .to_path_buf()
    })
}

pub enum Agent {
    Claude(&'static str),
    Acp(Value),
}

/// One simulator and one scratch HOME per test, so nothing touches the real user's files.
pub struct Rig {
    pub sim: Simulator,
    dir: TempDir,
}

impl Rig {
    pub async fn new() -> Self {
        let config = SimConfig {
            gateway: GatewayConfig {
                keepalive: Duration::from_secs(1),
                handshake_timeout: Duration::from_secs(5),
                ..Default::default()
            },
            wait: WAIT,
            ..Default::default()
        };
        let sim = Simulator::start(config.with_token(TOKEN, "node-1"))
            .await
            .unwrap();
        let dir = tempfile::Builder::new()
            .prefix("daemon-")
            .tempdir_in(env!("CARGO_TARGET_TMPDIR"))
            .unwrap();
        for sub in ["home", "config", "work", "state"] {
            std::fs::create_dir_all(dir.path().join(sub)).unwrap();
        }
        Self { sim, dir }
    }

    pub fn home(&self) -> PathBuf {
        self.dir.path().join("home")
    }

    pub fn config_dir(&self) -> PathBuf {
        self.dir.path().join("config")
    }

    pub fn config_path(&self) -> PathBuf {
        self.config_dir().join("riggs.toml")
    }

    pub fn token_path(&self) -> PathBuf {
        self.config_dir().join("node-token")
    }

    pub fn work(&self) -> PathBuf {
        self.dir.path().join("work")
    }

    pub fn state(&self) -> PathBuf {
        self.dir.path().join("state")
    }

    pub fn write_config(&self, text: &str) {
        std::fs::write(self.config_path(), text).unwrap();
    }

    pub fn agent_config(&self, agent: Agent) -> String {
        match agent {
            Agent::Claude(script) => {
                let script = Path::new(env!("CARGO_MANIFEST_DIR"))
                    .join("fixtures/claude")
                    .join(format!("{script}.json"));
                format!(
                    "[agent]\nkind = \"claude_code\"\ncommand = {command}\nworkdir = {work}\nhandshake_timeout = \"20s\"\ninterrupt_grace = \"20s\"\nenv = {{ FAKE_CLAUDE_SCRIPT = {script}, FAKE_CLAUDE_STATE = {state} }}\n",
                    command = quoted(Path::new(env!("CARGO_BIN_EXE_fake-claude"))),
                    work = quoted(&self.work()),
                    script = quoted(&script),
                    state = quoted(&self.state()),
                )
            }
            Agent::Acp(mut script) => {
                script["log"] = json!(self.state().join("acp.jsonl"));
                script["pid_file"] = json!(self.state().join("pids"));
                let path = self.state().join("script.json");
                std::fs::write(&path, script.to_string()).unwrap();
                format!(
                    "[agent]\nkind = \"acp\"\ncommand = {command}\nworkdir = {work}\nstartup_timeout = \"20s\"\nenv = {{ FAKE_ACP_SCRIPT = {script} }}\n",
                    command = quoted(Path::new(env!("CARGO_BIN_EXE_fake-acp"))),
                    work = quoted(&self.work()),
                    script = quoted(&path),
                )
            }
        }
    }

    /// A complete config: the simulator's address, the scratch session store, and the agent.
    pub fn configure(&self, agent: Agent, extra: &str) {
        self.write_config(&format!(
            "[gateway]\nurls = [\"{url}\"]\n\n{agent}\n{extra}",
            url = self.sim.url(),
            agent = self.agent_config(agent),
        ));
    }

    pub fn write_token(&self, token: &str, mode: u32) {
        write_file(&self.token_path(), &format!("{token}\n"), mode);
    }

    /// Rotates the way the docs tell an operator to: write beside it, then rename over it.
    pub fn rotate_token(&self, token: &str, mode: u32) {
        let staged = self.config_dir().join("node-token.new");
        write_file(&staged, &format!("{token}\n"), mode);
        std::fs::rename(staged, self.token_path()).unwrap();
    }

    pub fn command(&self, args: &[&str]) -> Command {
        let mut command = Command::new(riggs_binary());
        command
            .args(args)
            .env_clear()
            .env("HOME", self.home())
            .env("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
            .env("TMPDIR", self.dir.path())
            .current_dir(self.dir.path())
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        command
    }

    pub fn spawn(&self, args: &[&str]) -> Riggs {
        let mut child = self.command(args).spawn().unwrap();
        let stdout = capture(child.stdout.take().unwrap());
        let stderr = capture(child.stderr.take().unwrap());
        Riggs {
            child,
            stdout,
            stderr,
        }
    }

    pub fn start(&self) -> Riggs {
        let config = self.config_path();
        self.spawn(&["--config", config.to_str().unwrap(), "run"])
    }

    pub async fn exits(&self, args: &[&str]) -> Finished {
        let mut riggs = self.spawn(args);
        let status = riggs.exited().await;
        Finished {
            status,
            stdout: riggs.stdout(),
            stderr: riggs.stderr(),
        }
    }

    pub async fn with_config(&self, command: &str) -> Finished {
        let config = self.config_path();
        self.exits(&["--config", config.to_str().unwrap(), command])
            .await
    }

    pub async fn attached(&self) -> SimNode {
        let node = self.sim.next_node().await.unwrap();
        node.initialize(all_caps()).await.unwrap();
        node
    }

    pub fn fake_log(&self) -> Vec<Value> {
        std::fs::read_to_string(self.state().join("log.jsonl"))
            .unwrap_or_default()
            .lines()
            .filter_map(|line| serde_json::from_str(line).ok())
            .collect()
    }

    pub fn fake_events(&self, name: &str) -> Vec<Value> {
        self.fake_log()
            .into_iter()
            .filter(|entry| entry["event"] == name)
            .collect()
    }

    pub async fn handshakes_until(
        &self,
        what: &str,
        done: impl Fn(&[Handshake]) -> bool,
    ) -> Vec<Handshake> {
        eventually(what, || done(&self.sim.handshakes())).await;
        self.sim.handshakes()
    }
}

pub struct Finished {
    pub status: ExitStatus,
    pub stdout: String,
    pub stderr: String,
}

pub struct Riggs {
    child: Child,
    stdout: Arc<Captured>,
    stderr: Arc<Captured>,
}

#[derive(Default)]
pub struct Captured {
    bytes: Mutex<Vec<u8>>,
    closed: AtomicBool,
}

impl Captured {
    fn text(&self) -> String {
        String::from_utf8_lossy(&self.bytes.lock().unwrap()).into_owned()
    }
}

impl Riggs {
    pub fn pid(&self) -> u32 {
        self.child.id()
    }

    pub fn stdout(&self) -> String {
        self.stdout.text()
    }

    pub fn stderr(&self) -> String {
        self.stderr.text()
    }

    pub fn signal(&self, signal: Signal) {
        kill(Pid::from_raw(self.child.id() as i32), signal).unwrap();
    }

    pub fn is_running(&mut self) -> bool {
        self.child.try_wait().unwrap().is_none()
    }

    pub async fn exited(&mut self) -> ExitStatus {
        let deadline = Instant::now() + WAIT;
        loop {
            if let Some(status) = self.child.try_wait().unwrap() {
                self.drained().await;
                return status;
            }
            assert!(
                Instant::now() < deadline,
                "riggs did not exit; stderr:\n{}",
                self.stderr()
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }

    async fn drained(&self) {
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline
            && !(self.stdout.closed.load(Ordering::SeqCst)
                && self.stderr.closed.load(Ordering::SeqCst))
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    pub async fn logged(&self, needle: &str) {
        eventually(&format!("riggs to log {needle:?}"), || {
            self.stderr().contains(needle)
        })
        .await;
    }
}

impl Drop for Riggs {
    fn drop(&mut self) {
        if self.child.try_wait().ok().flatten().is_none() {
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }
}

/// Polls, because what it waits for happens in another process.
pub async fn eventually(what: &str, done: impl Fn() -> bool) {
    let deadline = Instant::now() + WAIT;
    while !done() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

pub fn quoted(path: &Path) -> String {
    format!("{:?}", path.display().to_string())
}

pub fn write_file(path: &Path, content: &str, mode: u32) {
    std::fs::write(path, content).unwrap();
    std::fs::set_permissions(path, Permissions::from_mode(mode)).unwrap();
}

pub fn warnings_and_errors(log: &str) -> Vec<&str> {
    log.lines()
        .filter(|line| line.contains(" WARN ") || line.contains(" ERROR "))
        .collect()
}

fn capture(mut pipe: impl Read + Send + 'static) -> Arc<Captured> {
    let captured = Arc::new(Captured::default());
    let filling = captured.clone();
    std::thread::spawn(move || {
        let mut chunk = [0u8; 4096];
        while let Ok(read) = pipe.read(&mut chunk) {
            if read == 0 {
                break;
            }
            filling
                .bytes
                .lock()
                .unwrap()
                .extend_from_slice(&chunk[..read]);
        }
        filling.closed.store(true, Ordering::SeqCst);
    });
    captured
}
