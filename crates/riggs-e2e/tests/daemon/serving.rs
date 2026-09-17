use nix::sys::signal::Signal;
use rax::event::StopReason;
use rax::session::{SessionDurability, ToolGate};
use rax_sim::Match;
use serde_json::json;

use crate::harness::{Agent, OTHER_SECRET, OTHER_TOKEN, Rig, SECRET, warnings_and_errors};
use crate::support::{TOKEN, alive, all_caps, text};

const NORMAL_CLOSURE: u16 = 1000;

async fn closed_normally(rig: &Rig) {
    let handshakes = rig
        .handshakes_until("the first socket to end", |handshakes| {
            handshakes
                .first()
                .is_some_and(|first| first.close_code.is_some())
        })
        .await;
    assert_eq!(
        handshakes[0].close_code,
        Some(NORMAL_CLOSURE),
        "{handshakes:?}"
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_killed_daemon_restarts_and_resumes_a_durable_claude_code_session() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("resume"), "");
    rig.write_token(TOKEN, 0o600);
    let mut riggs = rig.start();
    let node = rig.attached().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("remember 42"))
        .await
        .unwrap();
    turn.expect(Match::message_contains("noted")).await.unwrap();
    turn.until_end().await.unwrap();
    riggs.signal(Signal::SIGKILL);
    riggs.exited().await;

    let _restarted = rig.start();
    let node = rig.attached().await;
    let mut turn = node
        .prompt(session.clone(), text("what did I say?"))
        .await
        .unwrap();
    turn.expect(Match::message_contains("recalled: remember 42"))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    let starts = rig.fake_events("start");
    assert_eq!(starts.len(), 2);
    let argv: Vec<&str> = starts[1]["argv"]
        .as_array()
        .unwrap()
        .iter()
        .map(|arg| arg.as_str().unwrap())
        .collect();
    assert_eq!(argv[argv.len() - 2..], ["--resume", session.0.as_str()]);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_second_daemon_on_the_same_config_exits_naming_the_first() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    let mut first = rig.start();
    rig.attached().await;

    let second = rig.with_config("run").await;
    assert_eq!(second.status.code(), Some(1), "{}", second.stderr);
    assert!(
        second.stderr.contains(&format!("pid {}", first.pid())),
        "{}",
        second.stderr
    );
    assert!(first.is_running());
    assert_eq!(rig.sim.handshakes().len(), 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn sigterm_closes_the_socket_normally_and_exits_zero() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    let mut riggs = rig.start();
    rig.attached().await;
    riggs.signal(Signal::SIGTERM);
    let status = riggs.exited().await;
    assert_eq!(status.code(), Some(0), "{}", riggs.stderr());
    closed_normally(&rig).await;
    assert!(riggs.stdout().starts_with("riggs "), "{}", riggs.stdout());
    assert!(riggs.stdout().contains("tool_gate: every_call"));
}

#[tokio::test(flavor = "multi_thread")]
async fn sigterm_mid_turn_never_runs_the_held_tool_and_stops_the_agent() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("tool-gate"), "");
    rig.write_token(TOKEN, 0o600);
    let mut riggs = rig.start();
    let node = rig.attached().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("touch it")).await.unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();
    let agent = rig.fake_events("start")[0]["pid"].as_u64().unwrap() as u32;
    assert!(alive(agent));

    riggs.signal(Signal::SIGTERM);
    let status = riggs.exited().await;
    assert_eq!(status.code(), Some(0), "{}", riggs.stderr());
    assert!(!alive(agent), "the agent outlived riggs");
    assert!(rig.fake_events("hook_allowed").is_empty());
    assert!(!rig.work().join("ran.txt").exists());
    closed_normally(&rig).await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_gateway_close_is_answered_closed_normally_and_redialled() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    let mut riggs = rig.start();
    let node = rig.attached().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    node.close().await.unwrap();

    closed_normally(&rig).await;
    let node = rig.attached().await;
    assert_eq!(rig.sim.handshakes().len(), 2);
    let mut turn = node.prompt(session, text("still there?")).await.unwrap();
    turn.expect(Match::message_contains("pong")).await.unwrap();
    turn.until_end().await.unwrap();
    assert!(riggs.is_running());
}

#[tokio::test(flavor = "multi_thread")]
async fn no_output_ever_contains_the_token_secret() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("tool-gate"), "[log]\nlevel = \"trace\"\n");
    rig.write_token(OTHER_TOKEN, 0o600);
    let validated = rig.with_config("validate").await;
    let mut riggs = rig.start();
    riggs
        .logged("the gateway rejected this node's credential")
        .await;

    rig.rotate_token(TOKEN, 0o600);
    let node = rig.attached().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("touch it")).await.unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();
    riggs.signal(Signal::SIGTERM);
    riggs.exited().await;

    for (name, output) in [
        ("validate stdout", validated.stdout),
        ("validate stderr", validated.stderr),
        ("run stdout", riggs.stdout()),
        ("run stderr", riggs.stderr()),
    ] {
        for secret in [SECRET, OTHER_SECRET] {
            assert!(
                !output.contains(secret),
                "{name} leaked a secret:\n{output}"
            );
        }
    }
    assert!(
        riggs.stderr().contains(" DEBUG "),
        "debug logging was not on:\n{}",
        riggs.stderr()
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_clean_turn_and_a_new_session_log_no_warnings() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    let mut riggs = rig.start();
    let node = rig.attached().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("say pong")).await.unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    node.new_session(vec![]).await.unwrap();
    riggs.signal(Signal::SIGTERM);
    assert_eq!(riggs.exited().await.code(), Some(0));
    let stderr = riggs.stderr();
    assert!(stderr.contains(" INFO "), "{stderr}");
    assert_eq!(warnings_and_errors(&stderr), Vec::<&str>::new(), "{stderr}");
}

#[tokio::test(flavor = "multi_thread")]
async fn an_acp_agent_is_served_under_permission_prompts() {
    let rig = Rig::new().await;
    rig.configure(
        Agent::Acp(json!({"turns": {"hello": [{"say": "hi there"}]}})),
        "[sessions]\ndurable = false\n",
    );
    rig.write_token(TOKEN, 0o600);
    let mut riggs = rig.start();
    let node = rig.sim.next_node().await.unwrap();
    let caps = node.initialize(all_caps()).await.unwrap().capabilities;
    assert_eq!(caps.tool_gate, ToolGate::PermissionPrompts);
    assert_eq!(caps.sessions, SessionDurability::Ephemeral);
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("hello")).await.unwrap();
    turn.expect(Match::message_contains("hi there"))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    riggs.signal(Signal::SIGTERM);
    assert_eq!(riggs.exited().await.code(), Some(0), "{}", riggs.stderr());
    assert!(riggs.stdout().contains("tool_gate: permission_prompts"));
}
