#![allow(dead_code, clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::future::Future;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use nix::sys::signal::kill;
use nix::unistd::Pid;
use rax::content::ContentBlock;
use rax::session::{GatewayCapabilities, Initialized};
use rax::{Event, Open};
use rax_sim::{NodeOptions, SimConfig, SimError, SimNode, Simulator};
use rax_tokio::gateway::GatewayConfig;
use rax_tokio::node::{NodeConfig, NodeLink};
use riggs_claude_code::{ClaudeCode, ClaudeCodeConfig};
use riggs_node::{NodeServer, SESSION_RETENTION, ServerConfig, SessionsConfig, Stopped};
use serde_json::Value;
use tempfile::TempDir;
use tokio::task::JoinHandle;

pub const TOKEN: &str = "mrtg_node_0123456789abcdef_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I";
const WAIT: Duration = Duration::from_secs(20);

pub async fn within<F: Future>(future: F) -> F::Output {
    tokio::time::timeout(Duration::from_secs(60), future)
        .await
        .expect("test guard timed out")
}

pub fn text(text: &str) -> Vec<Open<ContentBlock>> {
    vec![ContentBlock::text(text).into()]
}

pub fn all_caps() -> GatewayCapabilities {
    GatewayCapabilities {
        question: true,
        plan: true,
        sign_in: true,
        resource_schemes: vec![],
        readable_schemes: vec![],
    }
}

pub fn fault<T>(result: Result<T, SimError>) -> rax::Error {
    match result {
        Ok(_) => panic!("expected a fault, the call succeeded"),
        Err(err) => err
            .fault()
            .cloned()
            .unwrap_or_else(|| panic!("expected a fault, got {err}")),
    }
}

pub fn alive(pid: u32) -> bool {
    kill(Pid::from_raw(pid as i32), None).is_ok()
}

pub fn messages(events: &[Open<Event>]) -> Vec<String> {
    events
        .iter()
        .filter_map(|event| match event {
            Open::Known(Event::Message {
                content: Open::Known(ContentBlock::Text { text }),
            }) => Some(text.clone()),
            _ => None,
        })
        .collect()
}

struct Running {
    server: NodeServer,
    serving: JoinHandle<Stopped>,
}

/// One simulator, one scratch directory and at most one node at a time, so a test can stop a node
/// and start another on the same session store and fake-claude state.
pub struct World {
    pub sim: Simulator,
    pub config: ClaudeCodeConfig,
    /// Installs the backend's credential repair as the node's `turn_failed`, as the binary does.
    pub repair: bool,
    pub call_timeout: Option<Duration>,
    dir: TempDir,
    node: Option<Running>,
}

impl World {
    pub async fn new(script: &str) -> Self {
        Self::with(script, |_| {}).await
    }

    pub async fn with(script: &str, tweak: impl FnOnce(&mut ClaudeCodeConfig)) -> Self {
        Self::build(script, tweak, NodeOptions::default()).await
    }

    pub async fn build(
        script: &str,
        tweak: impl FnOnce(&mut ClaudeCodeConfig),
        node: NodeOptions,
    ) -> Self {
        let dir = tempfile::Builder::new()
            .prefix("claude-code-")
            .tempdir_in(env!("CARGO_TARGET_TMPDIR"))
            .unwrap();
        for sub in ["work", "state"] {
            std::fs::create_dir_all(dir.path().join(sub)).unwrap();
        }
        let script = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("fixtures/claude")
            .join(format!("{script}.json"));
        assert!(script.exists(), "no fixture {}", script.display());
        let mut config = ClaudeCodeConfig::new(dir.path().join("work"));
        config.command = PathBuf::from(env!("CARGO_BIN_EXE_fake-claude"));
        config.env.insert(
            "FAKE_CLAUDE_SCRIPT".to_owned(),
            script.display().to_string(),
        );
        config.env.insert(
            "FAKE_CLAUDE_STATE".to_owned(),
            dir.path().join("state").display().to_string(),
        );
        config.handshake_timeout = Duration::from_secs(20);
        config.interrupt_grace = Duration::from_secs(20);
        config.sign_in.scratch_dir = dir.path().to_path_buf();
        tweak(&mut config);
        let sim_config = SimConfig {
            gateway: GatewayConfig {
                keepalive: Duration::from_secs(1),
                handshake_timeout: Duration::from_secs(5),
                ..Default::default()
            },
            node,
            wait: WAIT,
            ..Default::default()
        };
        let sim = Simulator::start(sim_config.with_token(TOKEN, "node-1"))
            .await
            .unwrap();
        Self {
            sim,
            config,
            repair: false,
            call_timeout: None,
            dir,
            node: None,
        }
    }

    pub fn root(&self) -> PathBuf {
        self.dir.path().to_path_buf()
    }

    pub fn work(&self) -> PathBuf {
        self.dir.path().join("work")
    }

    pub fn state(&self) -> PathBuf {
        self.dir.path().join("state")
    }

    pub fn store(&self) -> PathBuf {
        self.dir.path().join("store")
    }

    pub async fn start_node(&mut self) -> SimNode {
        self.start_node_initialized().await.0
    }

    pub async fn start_node_initialized(&mut self) -> (SimNode, Initialized) {
        assert!(self.node.is_none(), "a node is already running");
        let backend = Arc::new(ClaudeCode::new(self.config.clone()));
        let mut config = ServerConfig::new(SessionsConfig::Durable {
            dir: self.store(),
            retain: SESSION_RETENTION,
        });
        if self.repair {
            config.turn_failed = Some(backend.turn_failed());
        }
        if let Some(call_timeout) = self.call_timeout {
            config.call_timeout = call_timeout;
        }
        let server = NodeServer::new(backend, config).unwrap();
        let (handle, events) = NodeLink::start(NodeConfig {
            endpoints: vec![self.sim.url()],
            token: TOKEN.to_owned(),
            keepalive: Duration::from_secs(1),
            backoff_min: Duration::from_millis(5),
            backoff_max: Duration::from_millis(50),
            handshake_timeout: Duration::from_secs(5),
            ..Default::default()
        })
        .unwrap();
        let serving = tokio::spawn({
            let server = server.clone();
            async move { server.serve(handle, events).await }
        });
        self.node = Some(Running { server, serving });
        let node = self.sim.next_node().await.unwrap();
        let initialized = node.initialize(all_caps()).await.unwrap();
        (node, initialized)
    }

    pub async fn stop_node(&mut self) -> Stopped {
        let Running { server, serving } = self.node.take().expect("no node is running");
        server.shutdown_token().cancel();
        within(serving).await.unwrap()
    }

    pub fn log(&self) -> Vec<Value> {
        std::fs::read_to_string(self.state().join("log.jsonl"))
            .unwrap_or_default()
            .lines()
            .filter_map(|line| serde_json::from_str(line).ok())
            .collect()
    }

    pub fn events(&self, name: &str) -> Vec<Value> {
        self.log()
            .into_iter()
            .filter(|entry| entry["event"] == name)
            .collect()
    }

    pub fn has_event(&self, name: &str) -> bool {
        !self.events(name).is_empty()
    }

    pub fn starts(&self) -> Vec<Value> {
        self.events("start")
    }

    pub fn argv(&self, start: usize) -> Vec<String> {
        self.starts()[start]["argv"]
            .as_array()
            .unwrap()
            .iter()
            .map(|arg| arg.as_str().unwrap().to_owned())
            .collect()
    }

    pub fn pid(&self, start: usize) -> u32 {
        self.starts()[start]["pid"].as_u64().unwrap() as u32
    }

    pub fn stdin(&self) -> Vec<Value> {
        self.events("stdin")
            .into_iter()
            .map(|entry| entry["frame"].clone())
            .collect()
    }

    pub fn user_frames(&self) -> Vec<Value> {
        self.stdin()
            .into_iter()
            .filter(|frame| frame["type"] == "user")
            .collect()
    }

    pub fn stdin_contains(&self, needle: &str) -> bool {
        self.stdin()
            .iter()
            .any(|frame| frame.to_string().contains(needle))
    }

    pub fn assert_no_violations(&self) {
        let violations = self.events("violation");
        assert!(violations.is_empty(), "fake-claude saw: {violations:?}");
    }

    /// Polls, because what it waits for happens in another process.
    pub async fn eventually(&self, what: &str, done: impl Fn(&World) -> bool) {
        let deadline = tokio::time::Instant::now() + WAIT;
        while !done(self) {
            assert!(
                tokio::time::Instant::now() < deadline,
                "timed out waiting for {what}"
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    }
}

impl Drop for World {
    fn drop(&mut self) {
        if let Some(running) = self.node.take() {
            running.server.shutdown_token().cancel();
        }
    }
}
