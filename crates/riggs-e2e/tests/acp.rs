#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use nix::sys::signal::kill;
use nix::unistd::Pid;
use rax::event::{BackgroundEvent, StopReason};
use rax::id::SessionId;
use rax::session::{GatewayCapabilities, SessionDurability, ToolGate};
use rax::tool::{DeniedBy, ToolCallStatus};
use rax::{ContentBlock, Decision, ErrorKind, Event, Open};
use rax_sim::{Match, SimConfig, SimNode, Simulator};
use rax_tokio::gateway::GatewayConfig;
use rax_tokio::node::{NodeConfig, NodeLink};
use riggs_acp::{AcpBackend, AcpConfig};
use riggs_node::{NodeServer, ServerConfig, SessionsConfig, Stopped};
use serde_json::{Value, json};
use tempfile::TempDir;
use tokio::task::JoinHandle;

const TOKEN: &str = "mrtg_node_0123456789abcdef_c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3I";

struct Fixture {
    dir: TempDir,
}

impl Fixture {
    fn new() -> Self {
        Self {
            dir: tempfile::tempdir().unwrap(),
        }
    }

    fn path(&self, name: &str) -> PathBuf {
        self.dir.path().join(name)
    }

    fn text(&self, name: &str) -> String {
        self.path(name).to_string_lossy().into_owned()
    }

    fn agent(&self, mut script: Value) -> AcpConfig {
        script["log"] = json!(self.text("log.jsonl"));
        script["pid_file"] = json!(self.text("pids"));
        let path = self.path("script.json");
        std::fs::write(&path, script.to_string()).unwrap();
        let mut config = AcpConfig::new(env!("CARGO_BIN_EXE_fake-acp"));
        config.env.insert(
            "FAKE_ACP_SCRIPT".into(),
            Some(path.to_string_lossy().into()),
        );
        config.workdir = Some(self.dir.path().to_path_buf());
        config.startup_timeout = Duration::from_secs(20);
        config
    }

    fn store(&self) -> SessionsConfig {
        SessionsConfig::Durable {
            dir: self.path("sessions"),
            retain: riggs_node::SESSION_RETENTION,
        }
    }

    fn received(&self) -> Vec<(u32, Value)> {
        read_lines(&self.path("log.jsonl"))
            .into_iter()
            .map(|line| {
                let entry: Value = serde_json::from_str(&line).unwrap();
                (
                    u32::try_from(entry["pid"].as_u64().unwrap()).unwrap(),
                    entry["message"].clone(),
                )
            })
            .collect()
    }

    fn methods(&self) -> Vec<String> {
        self.received()
            .into_iter()
            .filter_map(|(_, message)| message["method"].as_str().map(str::to_owned))
            .collect()
    }

    fn pids(&self, name: &str) -> Vec<i32> {
        read_lines(&self.path(name))
            .iter()
            .map(|pid| pid.parse().unwrap())
            .collect()
    }

    fn permission_outcome(&self, request_id: &str) -> Value {
        self.received()
            .into_iter()
            .map(|(_, message)| message)
            .find(|message| message["id"] == request_id && message.get("method").is_none())
            .unwrap_or_else(|| panic!("no answer to {request_id}"))["result"]
            .clone()
    }
}

fn read_lines(path: &Path) -> Vec<String> {
    std::fs::read_to_string(path)
        .unwrap_or_default()
        .lines()
        .map(str::to_owned)
        .collect()
}

async fn eventually(what: &str, check: impl Fn() -> bool) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(20);
    while !check() {
        assert!(
            tokio::time::Instant::now() < deadline,
            "timed out waiting for {what}"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

fn gone(pid: i32) -> bool {
    kill(Pid::from_raw(pid), None).is_err()
}

async fn simulator() -> Simulator {
    let config = SimConfig {
        gateway: GatewayConfig {
            keepalive: Duration::from_secs(1),
            handshake_timeout: Duration::from_secs(5),
            ..Default::default()
        },
        wait: Duration::from_secs(20),
        ..Default::default()
    };
    Simulator::start(config.with_token(TOKEN, "node-1"))
        .await
        .unwrap()
}

struct Node {
    server: NodeServer,
    serving: JoinHandle<Stopped>,
    gateway: SimNode,
}

async fn start(sim: &Simulator, config: AcpConfig, sessions: SessionsConfig) -> Node {
    let backend = Arc::new(AcpBackend::new(config));
    let server = NodeServer::new(backend, ServerConfig::new(sessions)).unwrap();
    let (handle, events) = NodeLink::start(NodeConfig {
        endpoints: vec![sim.url()],
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
    let gateway = sim.next_node().await.unwrap();
    Node {
        server,
        serving,
        gateway,
    }
}

impl Node {
    async fn open(&self) -> SessionId {
        self.gateway
            .initialize(GatewayCapabilities::default())
            .await
            .unwrap();
        self.gateway.new_session(vec![]).await.unwrap().session_id
    }

    async fn stop(self) {
        self.server.shutdown_token().cancel();
        let stopped = tokio::time::timeout(Duration::from_secs(20), self.serving)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(stopped, Stopped::Shutdown);
    }
}

fn text(text: &str) -> Vec<Open<ContentBlock>> {
    vec![ContentBlock::text(text).into()]
}

fn said(events: &[Open<Event>], wanted: &str) -> bool {
    events.iter().any(|event| {
        matches!(
            event,
            Open::Known(Event::Message { content: Open::Known(ContentBlock::Text { text }) })
                if text.contains(wanted)
        )
    })
}

fn permission(fixture: &Fixture) -> Value {
    json!({"permission": {
        "tool_call": {
            "toolCallId": "tc1",
            "title": "touch effect",
            "kind": "execute",
            "rawInput": {"command": "touch effect"},
            "_meta": {"claudeCode": {"toolName": "Bash"}},
        },
        "options": [
            {"optionId": "always", "name": "Always allow", "kind": "allow_always"},
            {"optionId": "once", "name": "Allow", "kind": "allow_once"},
            {"optionId": "no", "name": "Reject", "kind": "reject_once"},
        ],
        "effect": fixture.text("effect"),
    }})
}

fn tool_update(id: &str, status: &str) -> Value {
    json!({"update": {"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status}})
}

#[tokio::test(flavor = "multi_thread")]
async fn initialize_declares_permission_prompts_and_durability_from_load_session() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let node = start(
        &sim,
        fixture.agent(json!({"load_session": true})),
        fixture.store(),
    )
    .await;
    let caps = node
        .gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap()
        .capabilities;
    assert_eq!(caps.tool_gate, ToolGate::PermissionPrompts);
    assert_eq!(caps.sessions, SessionDurability::Durable);
    assert_eq!(caps.interruptible, Some(true));
    node.stop().await;

    let fixture = Fixture::new();
    let mut config = fixture.agent(json!({"load_session": false}));
    config.interruptible = Some(false);
    let node = start(&sim, config, fixture.store()).await;
    let caps = node
        .gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap()
        .capabilities;
    assert_eq!(caps.sessions, SessionDurability::Ephemeral);
    assert_eq!(caps.interruptible, Some(false));
    node.stop().await;
    assert!(
        fixture
            .methods()
            .iter()
            .all(|method| method != "session/cancel"),
        "interruptible must not be probed"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_turn_streams_messages_and_plans_then_completes() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {"hello": [
        {"update": {"sessionUpdate": "agent_thought_chunk", "content": {"type": "text", "text": "thinking"}}},
        {"say": "hi there"},
        {"update": {"sessionUpdate": "plan", "entries": [{"content": "wave", "priority": "high", "status": "in_progress"}]}},
        {"update": {"sessionUpdate": "brand_new_kind"}},
        {"stop": "max_tokens"},
    ]}});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node.gateway.prompt(session, text("hello")).await.unwrap();
    turn.expect(Match::message_contains("hi there"))
        .await
        .unwrap();
    let plan = turn
        .expect(Match::when("a plan update", |event| {
            matches!(event, Event::PlanUpdate { .. })
        }))
        .await
        .unwrap();
    assert!(matches!(plan, Event::PlanUpdate { entries } if entries[0].content == "wave"));
    turn.expect(Match::complete(StopReason::MaxTokens))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert!(!said(turn.seen(), "thinking"));
    let report = sim.report();
    let reported = report
        .turns
        .iter()
        .find(|reported| &reported.stream == turn.id())
        .unwrap();
    assert_eq!(reported.reply_before_first_event, Some(true));
    let initialize = fixture
        .received()
        .into_iter()
        .find(|(_, message)| message["method"] == "initialize")
        .unwrap()
        .1;
    assert_eq!(
        initialize["params"]["clientCapabilities"],
        json!({"fs": {"readTextFile": false, "writeTextFile": false}, "terminal": false, "auth": {"terminal": false}})
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_allowed_permission_runs_the_tool_only_after_the_verdict_and_never_remembers() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {"run it": [
        permission(&fixture),
        tool_update("tc1", "in_progress"),
        tool_update("tc1", "completed"),
        {"say": "done"},
    ]}});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node.gateway.prompt(session, text("run it")).await.unwrap();
    let Event::ToolCall { tool_call } = turn.expect(Match::tool_call("Bash")).await.unwrap() else {
        unreachable!()
    };
    assert_eq!(tool_call.id.0, "tc1");
    assert_eq!(tool_call.title.as_deref(), Some("touch effect"));
    assert_eq!(tool_call.input, Some(json!({"command": "touch effect"})));
    assert!(!fixture.path("effect").exists());

    turn.verdict("tc1", Decision::Allow).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::InProgress))
        .await
        .unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Completed))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert!(fixture.path("effect").exists());
    assert_eq!(
        fixture.permission_outcome("fake-1"),
        json!({"outcome": {"outcome": "selected", "optionId": "once"}})
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_denied_permission_picks_the_reject_option_and_the_tool_never_runs() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {"run it": [permission(&fixture), {"say": "skipped"}]}});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node.gateway.prompt(session, text("run it")).await.unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();
    let deny = Decision::Deny {
        by: DeniedBy::Policy,
        reason: Some("no touching".into()),
    };
    turn.verdict("tc1", deny).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert!(!fixture.path("effect").exists());
    assert_eq!(
        fixture.permission_outcome("fake-1"),
        json!({
            "outcome": {"outcome": "selected", "optionId": "no"},
            "_meta": {"riggs": {"by": "policy", "reason": "no touching"}},
        })
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn the_node_rejects_a_permission_nobody_answers_by_its_deadline() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {"run it": [permission(&fixture)]}});
    let mut config = fixture.agent(script);
    config.permission_timeout = Duration::from_millis(300);
    let node = start(&sim, config, SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node.gateway.prompt(session, text("run it")).await.unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::when(
        "a timeout status",
        |event| matches!(event, Event::Status { text } if text == "approval timed out"),
    ))
    .await
    .unwrap();
    turn.until_end().await.unwrap();
    assert!(!fixture.path("effect").exists());
    assert_eq!(
        fixture.permission_outcome("fake-1")["outcome"],
        json!({"outcome": "selected", "optionId": "no"})
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn tools_the_agent_did_not_ask_about_are_not_announced() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {"look": [
        {"update": {"sessionUpdate": "tool_call", "toolCallId": "tc9", "title": "ls", "kind": "read", "status": "pending"}},
        tool_update("tc9", "in_progress"),
        tool_update("tc9", "completed"),
        {"say": "looked"},
    ]}});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node.gateway.prompt(session, text("look")).await.unwrap();
    let rest = turn.until_end().await.unwrap();
    assert!(said(&rest, "looked"));
    assert!(
        rest.iter().all(|event| !matches!(
            event,
            Open::Known(Event::ToolCall { .. } | Event::ToolCallUpdate { .. })
        )),
        "{rest:?}"
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn cancelling_with_a_pending_permission_answers_cancelled_and_late_updates_go_to_background()
{
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {
        "hold": [permission(&fixture), {"wait_cancel": true}, {"stop": "cancelled"}, {"say": "late"}],
        "again": [{"say": "fresh"}],
    }});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("hold"))
        .await
        .unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();
    node.gateway.cancel(session.clone()).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::Cancelled))
        .await
        .unwrap();
    turn.until_end().await.unwrap();

    let (from, late) = node.gateway.next_background().await.unwrap();
    assert_eq!(from, session);
    assert!(matches!(
        late,
        Open::Known(BackgroundEvent::Message { content: Open::Known(ContentBlock::Text { ref text }) }) if text == "late"
    ));

    let mut next = node
        .gateway
        .prompt(session.clone(), text("again"))
        .await
        .unwrap();
    let rest = next.until_end().await.unwrap();
    assert!(said(&rest, "fresh"));
    assert!(!said(&rest, "late"));

    assert!(!fixture.path("effect").exists());
    let received: Vec<Value> = fixture.received().into_iter().map(|(_, m)| m).collect();
    let answer = received
        .iter()
        .position(|message| message["id"] == "fake-1")
        .unwrap();
    let cancel = received
        .iter()
        .position(|message| message["method"] == "session/cancel")
        .unwrap();
    assert!(
        answer < cancel,
        "the permission must be answered before session/cancel"
    );
    assert!(
        received[cancel].get("id").is_none(),
        "session/cancel is a notification"
    );
    assert_eq!(
        received[answer]["result"],
        json!({"outcome": {"outcome": "cancelled"}})
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_agent_that_ignores_cancel_is_stopped_and_its_session_reloaded() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"load_session": true, "turns": {
        "stubborn": [{"say": "waiting"}, {"wait_cancel": true}, {"say": "still here"}, {"hang": true}],
        "again": [{"say": "back"}],
    }});
    let mut config = fixture.agent(script);
    config.cancel_grace_period = Duration::from_millis(300);
    let node = start(&sim, config, SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("stubborn"))
        .await
        .unwrap();
    turn.expect(Match::message_contains("waiting"))
        .await
        .unwrap();
    node.gateway.cancel(session.clone()).await.unwrap();
    turn.expect(Match::complete(StopReason::Cancelled))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    let stubborn = fixture.pids("pids")[1];
    eventually("the stubborn agent to be killed", || gone(stubborn)).await;

    let mut next = node.gateway.prompt(session, text("again")).await.unwrap();
    assert!(said(&next.until_end().await.unwrap(), "back"));
    assert_eq!(fixture.pids("pids").len(), 3);
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_crash_mid_turn_is_an_error_and_the_next_prompt_reloads_in_a_fresh_process() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"load_session": true, "turns": {
        "crash": [{"say": "going down"}, {"crash": {"code": 3, "stderr": "boom: out of cheese"}}],
        "after": [{"say": "recovered"}],
    }});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("crash"))
        .await
        .unwrap();
    let Event::Error { error } = turn.expect(Match::error(ErrorKind::Unknown)).await.unwrap()
    else {
        unreachable!()
    };
    assert!(error.message.contains("exited"), "{}", error.message);
    assert!(error.message.contains("out of cheese"), "{}", error.message);
    turn.until_end().await.unwrap();

    let mut next = node.gateway.prompt(session, text("after")).await.unwrap();
    assert!(said(&next.until_end().await.unwrap(), "recovered"));
    let pids = fixture.pids("pids");
    assert_eq!(pids.len(), 3, "probe, first agent, respawned agent");
    let respawned = u32::try_from(pids[2]).unwrap();
    let methods: Vec<String> = fixture
        .received()
        .into_iter()
        .filter(|(pid, _)| *pid == respawned)
        .filter_map(|(_, message)| message["method"].as_str().map(str::to_owned))
        .collect();
    assert_eq!(methods, ["initialize", "session/load", "session/prompt"]);
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_crashed_agent_that_cannot_reload_leaves_an_unknown_session() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {"crash": [{"crash": {"code": 1, "stderr": "bye"}}]}});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("crash"))
        .await
        .unwrap();
    turn.expect(Match::error(ErrorKind::Unknown)).await.unwrap();
    turn.until_end().await.unwrap();
    let refused = node
        .gateway
        .prompt(session, text("again"))
        .await
        .err()
        .unwrap();
    assert_eq!(refused.fault().unwrap().kind, ErrorKind::UnknownSession);
    assert_eq!(
        fixture.pids("pids").len(),
        2,
        "no agent is started for a lost session"
    );
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_oversize_line_fails_the_turn_and_stops_the_agent() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"load_session": true, "turns": {
        "big": [{"oversize": 65 * 1024 * 1024}, {"say": "unreachable"}],
        "small": [{"say": "fine"}],
    }});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("big"))
        .await
        .unwrap();
    let Event::Error { error } = turn.expect(Match::error(ErrorKind::Unknown)).await.unwrap()
    else {
        unreachable!()
    };
    assert!(error.message.contains("longer than"), "{}", error.message);
    turn.until_end().await.unwrap();
    assert!(gone(fixture.pids("pids")[1]));

    let mut next = node.gateway.prompt(session, text("small")).await.unwrap();
    assert!(said(&next.until_end().await.unwrap(), "fine"));
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_cancel_reaches_a_prompt_that_is_still_reloading_its_agent() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"load_session": true, "load_hang": true, "turns": {
        "crash": [{"crash": {"code": 1, "stderr": "bye"}}],
    }});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("crash"))
        .await
        .unwrap();
    turn.until_end().await.unwrap();

    let pending = node
        .gateway
        .start_prompt(session.clone(), text("next"))
        .await
        .unwrap();
    eventually("the reload to reach the agent", || {
        fixture.methods().contains(&"session/load".to_owned())
    })
    .await;
    node.gateway.cancel(session).await.unwrap();
    let refused = pending.accepted().await.err().unwrap();
    assert_eq!(refused.fault().unwrap().kind, ErrorKind::Cancelled);
    let reloading = fixture.pids("pids")[2];
    eventually("the abandoned agent to stop", || gone(reloading)).await;
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_socket_lost_for_good_cancels_a_held_permission_and_the_session_still_prompts() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"turns": {
        "hold": [permission(&fixture), {"wait_cancel": true}, {"stop": "cancelled"}],
        "again": [{"say": "fresh"}],
    }});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("hold"))
        .await
        .unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();

    sim.restart().await.unwrap();
    let gateway = sim.next_node().await.unwrap();
    eventually("the agent to hear the cancel", || {
        fixture.methods().contains(&"session/cancel".to_owned())
    })
    .await;
    assert!(!fixture.path("effect").exists());
    assert_eq!(
        fixture.permission_outcome("fake-1"),
        json!({"outcome": {"outcome": "cancelled"}})
    );

    gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap();
    let mut next = gateway.prompt(session, text("again")).await.unwrap();
    assert!(said(&next.until_end().await.unwrap(), "fresh"));
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn closing_a_session_stops_its_agent_and_forgets_it() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let node = start(
        &sim,
        fixture.agent(json!({"load_session": true})),
        fixture.store(),
    )
    .await;
    let session = node.open().await;
    let record = fixture.path("sessions").join(format!("{session}.json"));
    assert!(record.exists());
    let pid = fixture.pids("pids")[1];
    assert!(!gone(pid));

    node.gateway.close_session(session.clone()).await.unwrap();
    assert!(gone(pid));
    assert!(!record.exists());
    let refused = node
        .gateway
        .prompt(session, text("hi"))
        .await
        .err()
        .unwrap();
    assert_eq!(refused.fault().unwrap().kind, ErrorKind::UnknownSession);
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_durable_session_is_reloaded_after_a_restart_without_replaying_history() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"load_session": true,
        "replay": [
            {"sessionUpdate": "user_message_chunk", "content": {"type": "text", "text": "remember this"}},
            {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "old reply"}},
        ],
        "turns": {"remember this": [{"say": "noted"}], "recall": [{"say": "recalled"}]},
    });
    let config = fixture.agent(script);
    let node = start(&sim, config.clone(), fixture.store()).await;
    let session = node.open().await;
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("remember this"))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    node.stop().await;
    assert!(fixture.pids("pids").iter().all(|pid| gone(*pid)));

    let node = start(&sim, config, fixture.store()).await;
    node.gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap();
    let mut turn = node
        .gateway
        .prompt(session.clone(), text("recall"))
        .await
        .unwrap();
    let rest = turn.until_end().await.unwrap();
    assert!(said(&rest, "recalled"));
    assert!(!said(&rest, "old reply"), "{rest:?}");
    let load = fixture
        .received()
        .into_iter()
        .find(|(_, message)| message["method"] == "session/load")
        .unwrap()
        .1;
    assert_eq!(load["params"]["sessionId"], "sess-1");
    assert_eq!(load["params"]["mcpServers"], json!([]));
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_durable_session_the_agent_cannot_load_is_unknown_and_forgotten() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script = json!({"load_session": true,
        "load_error": {"code": -32002, "message": "Resource not found"},
    });
    let config = fixture.agent(script);
    let node = start(&sim, config.clone(), fixture.store()).await;
    let session = node.open().await;
    node.stop().await;
    let record = fixture.path("sessions").join(format!("{session}.json"));
    assert!(record.exists());

    let node = start(&sim, config, fixture.store()).await;
    node.gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap();
    let refused = node
        .gateway
        .prompt(session, text("hi"))
        .await
        .err()
        .unwrap();
    assert_eq!(refused.fault().unwrap().kind, ErrorKind::UnknownSession);
    assert!(!record.exists());
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_agent_without_load_session_is_ephemeral_across_a_restart() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let config = fixture.agent(json!({"load_session": false}));
    let node = start(&sim, config.clone(), fixture.store()).await;
    let session = node.open().await;
    node.stop().await;

    let node = start(&sim, config, fixture.store()).await;
    let caps = node
        .gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap()
        .capabilities;
    assert_eq!(caps.sessions, SessionDurability::Ephemeral);
    let refused = node
        .gateway
        .prompt(session, text("hi"))
        .await
        .err()
        .unwrap();
    assert_eq!(refused.fault().unwrap().kind, ErrorKind::UnknownSession);
    assert!(!fixture.methods().contains(&"session/load".to_owned()));
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_agent_that_needs_a_sign_in_fails_the_session_as_a_credential_error() {
    let sim = simulator().await;
    let fixture = Fixture::new();
    let script =
        json!({"new_session_error": {"code": -32000, "message": "Authentication required"}});
    let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
    node.gateway
        .initialize(GatewayCapabilities::default())
        .await
        .unwrap();
    let refused = node.gateway.new_session(vec![]).await.err().unwrap();
    let fault = refused.fault().unwrap();
    assert_eq!(fault.kind, ErrorKind::Credential);
    assert!(fault.message.contains("Fake login"), "{}", fault.message);
    assert!(!fixture.methods().contains(&"authenticate".to_owned()));
    node.stop().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn shutdown_kills_every_agent_and_the_descendants_that_left_its_group() {
    let sim = simulator().await;
    for ignore_eof in [false, true] {
        let fixture = Fixture::new();
        let script = json!({
            "grandchild_pid_file": fixture.text("grandchildren"),
            "ignore_eof": ignore_eof,
            "turns": {"wait": [{"wait_cancel": true}, {"stop": "cancelled"}]},
        });
        let node = start(&sim, fixture.agent(script), SessionsConfig::Ephemeral).await;
        let session = node.open().await;
        let _turn = node.gateway.prompt(session, text("wait")).await.unwrap();
        let agents = fixture.pids("pids");
        let grandchildren = fixture.pids("grandchildren");
        assert_eq!(
            grandchildren.len(),
            2,
            "one from the probe, one from the session"
        );
        eventually("the probe's descendant to be killed", || {
            gone(grandchildren[0])
        })
        .await;
        assert!(!gone(grandchildren[1]));

        node.stop().await;
        for pid in agents.iter().chain(&grandchildren) {
            eventually(&format!("process {pid} to be killed"), || gone(*pid)).await;
        }
    }
}
