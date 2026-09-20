#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::collections::BTreeMap;
use std::io::Write;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use rax::credential::{CredentialHealth, CredentialRenewal};
use rax::id::PromptId;
use rax::interaction::{DisplayAnswer, DisplayOutcome, SignInRequest, SignInSettled, SignInState};
use rax::{ErrorKind, Event, NodeCall};
use rax_sim::{Match, NodeCallRequest, NodeMethod, NodeOptions, SimNode, VerdictPolicy};
use support::*;
use tracing_subscriber::fmt::MakeWriter;

const URL: &str = "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&code_challenge=fake-challenge&code_challenge_method=S256&state=fake-state";
const CODE: &str = "GOOD#fake-state";
const NOT_SHOWN: &str = "no sign-in could be put in front of its owner";

#[derive(Clone, Default)]
struct Captured(Arc<Mutex<Vec<u8>>>);

impl Write for Captured {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.lock().unwrap().extend_from_slice(buf);
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

impl<'a> MakeWriter<'a> for Captured {
    type Writer = Captured;

    fn make_writer(&'a self) -> Self::Writer {
        self.clone()
    }
}

fn logs() -> String {
    static CAPTURED: OnceLock<Captured> = OnceLock::new();
    let captured = CAPTURED.get_or_init(|| {
        let captured = Captured::default();
        let subscriber = tracing_subscriber::fmt()
            .with_max_level(tracing::Level::TRACE)
            .with_ansi(false)
            .with_writer(captured.clone())
            .finish();
        let _ = tracing::subscriber::set_global_default(subscriber);
        captured
    });
    String::from_utf8_lossy(&captured.0.lock().unwrap()).into_owned()
}

fn answer(id: &PromptId, outcome: DisplayOutcome, code: Option<&str>) -> DisplayAnswer {
    DisplayAnswer {
        id: id.clone(),
        outcome,
        answers: BTreeMap::new(),
        choice: None,
        user_id: None,
        note: None,
        code: code.map(str::to_owned),
    }
}

fn manual_health() -> NodeOptions {
    NodeOptions::default()
}

async fn world(
    script: &str,
    tweak: impl FnOnce(&mut riggs_claude_code::ClaudeCodeConfig),
) -> World {
    logs();
    let options = NodeOptions {
        auto_reply: [NodeMethod::CredentialHealth].into(),
        ..Default::default()
    };
    World::build(script, tweak, options).await
}

async fn sign_in_call(node: &SimNode) -> (SignInRequest, NodeCallRequest) {
    let call = node.next_node_call().await.unwrap();
    match &call.call {
        NodeCall::SignIn(request) => (request.clone(), call),
        other => panic!("expected a sign_in, got {other:?}"),
    }
}

async fn settled_call(node: &SimNode) -> (SignInSettled, NodeCallRequest) {
    let call = node.next_node_call().await.unwrap();
    match &call.call {
        NodeCall::SignInSettled(settled) => (settled.clone(), call),
        other => panic!("expected a sign_in.settled, got {other:?}"),
    }
}

async fn health_call(node: &SimNode) -> (CredentialHealth, NodeCallRequest) {
    let call = node.next_node_call().await.unwrap();
    match &call.call {
        NodeCall::CredentialHealth(health) => (health.clone(), call),
        other => panic!("expected a credential.health, got {other:?}"),
    }
}

async fn failing_turn(node: &SimNode, session: &rax::id::SessionId) -> rax::Error {
    let mut turn = node.prompt(session.clone(), text("hi")).await.unwrap();
    let error = turn
        .expect(Match::when("an error", |event| {
            matches!(event, Event::Error { .. })
        }))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    match error {
        Event::Error { error } => error,
        other => panic!("expected an error, got {other:?}"),
    }
}

async fn failing_turn_with_sign_in(
    node: &SimNode,
    session: &rax::id::SessionId,
    shown: bool,
) -> (rax::Error, SignInRequest) {
    let mut turn = node.prompt(session.clone(), text("hi")).await.unwrap();
    let (request, call) = sign_in_call(node).await;
    if shown {
        call.reply().await.unwrap();
    } else {
        let refusal = rax::Error::new(
            ErrorKind::Unknown,
            "the owner of this machine may not use this gateway",
        );
        call.fault(refusal).await.unwrap();
    }
    let error = turn
        .expect(Match::when("an error", |event| {
            matches!(event, Event::Error { .. })
        }))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    match error {
        Event::Error { error } => (error, request),
        other => panic!("expected an error, got {other:?}"),
    }
}

async fn finish_sign_in(node: &SimNode, id: &PromptId, delay: Duration) -> Vec<SignInState> {
    node.answer(answer(id, DisplayOutcome::Answered, Some(CODE)))
        .await
        .unwrap();
    let mut states = Vec::new();
    loop {
        let (settled, call) = settled_call(node).await;
        assert_eq!(&settled.id, id);
        states.push(settled.state);
        tokio::time::sleep(delay).await;
        call.reply().await.unwrap();
        match settled.state {
            SignInState::Confirming => node
                .answer(answer(id, DisplayOutcome::Approved, None))
                .await
                .unwrap(),
            state if state.is_terminal() => return states,
            _ => {}
        }
    }
}

fn sign_ins(node: &SimNode) -> usize {
    node.call_log()
        .iter()
        .filter(|call| call.method == NodeMethod::SignIn)
        .count()
}

#[tokio::test(flavor = "multi_thread")]
async fn a_rejected_credential_asks_the_owner_once_and_their_code_signs_claude_code_back_in() {
    let mut world = World::build("sign-in", |_| {}, manual_health()).await;
    logs();
    world.repair = true;
    let node = world.start_node().await;
    let (healthy, call) = health_call(&node).await;
    assert_eq!(
        healthy.credential,
        world.config.command.display().to_string()
    );
    assert!(!healthy.degraded);
    call.reply().await.unwrap();
    let session = node.new_session(vec![]).await.unwrap().session_id;

    let mut turn = node.prompt(session.clone(), text("hi")).await.unwrap();
    let mut degraded = None;
    let request = loop {
        let call = node.next_node_call().await.unwrap();
        match call.call.clone() {
            NodeCall::CredentialHealth(health) => {
                if health.degraded {
                    degraded = Some(health);
                }
                call.reply().await.unwrap();
            }
            NodeCall::SignIn(request) => {
                call.reply().await.unwrap();
                break request;
            }
            other => panic!("unexpected {other:?}"),
        }
    };
    assert_eq!(request.tool, "Claude Code");
    assert_eq!(request.url.as_deref(), Some(URL));
    assert!(request.needs_code);
    assert_eq!(request.command, None);
    let Event::Error { error } = turn
        .expect(Match::error(ErrorKind::Credential))
        .await
        .unwrap()
    else {
        panic!("expected an error")
    };
    assert!(
        error
            .message
            .starts_with("the agent's credential was rejected: ")
            && error.message.contains("Please run /login"),
        "{error}"
    );
    turn.until_end().await.unwrap();
    let degraded = match degraded {
        Some(health) => health,
        None => loop {
            let (health, call) = health_call(&node).await;
            call.reply().await.unwrap();
            if health.degraded {
                break health;
            }
        },
    };
    assert!(
        degraded.degraded && degraded.since.is_some(),
        "{degraded:?}"
    );
    assert!(degraded.reason.unwrap().contains("Please run /login"));

    let again = failing_turn(&node, &session).await;
    assert_eq!(again.kind, ErrorKind::Credential, "{again}");
    assert_eq!(
        sign_ins(&node),
        1,
        "a second failure raised a second sign-in"
    );

    let states = finish_sign_in(&node, &request.id, Duration::ZERO).await;
    assert_eq!(
        states,
        [
            SignInState::Working,
            SignInState::Confirming,
            SignInState::Success
        ]
    );
    let (recovered, call) = health_call(&node).await;
    call.reply().await.unwrap();
    assert!(!recovered.degraded);
    assert_eq!(recovered.since, degraded.since);

    let codes: Vec<_> = world
        .events("auth_code")
        .into_iter()
        .map(|entry| entry["code"].clone())
        .collect();
    assert_eq!(codes, [CODE]);
    let login = &world.events("auth_login")[0];
    assert_eq!(
        login["argv"],
        serde_json::json!(["auth", "login", "--claudeai"])
    );
    assert_eq!(login["open_is_stand_in"], true, "{login}");
    assert!(login["browser"].as_str().unwrap().ends_with("/open"));

    let captured = logs();
    for secret in [CODE, "owner@example.com", &TOKEN[TOKEN.len() - 43..]] {
        assert!(!captured.contains(secret), "the logs carry {secret:?}");
    }
    assert!(captured.contains("asking this node's owner to sign in"));
    assert!(captured.contains("profile=claude-code"));
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_sign_in_the_gateway_refuses_is_not_reported_as_credential_and_is_not_asked_again() {
    let mut world = world("sign-in", |_| {}).await;
    world.repair = true;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;

    let (error, _) = failing_turn_with_sign_in(&node, &session, false).await;
    assert_eq!(error.kind, ErrorKind::Unknown, "{error}");
    assert!(error.message.contains(NOT_SHOWN), "{error}");
    for _ in 0..2 {
        let error = failing_turn(&node, &session).await;
        assert_eq!(error.kind, ErrorKind::Unknown, "{error}");
        assert!(error.message.contains(NOT_SHOWN), "{error}");
    }
    assert_eq!(sign_ins(&node), 1);
    let login = world.events("auth_login")[0]["pid"].as_u64().unwrap() as u32;
    world
        .eventually("the refused login to be stopped", |_| !alive(login))
        .await;
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn without_a_cooldown_the_next_failure_asks_again() {
    let mut world = world("sign-in", |config| config.sign_in.cooldown = Duration::ZERO).await;
    world.repair = true;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    for _ in 0..2 {
        let (error, _) = failing_turn_with_sign_in(&node, &session, false).await;
        assert!(error.message.contains(NOT_SHOWN), "{error}");
    }
    assert_eq!(sign_ins(&node), 2);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn other_failures_pass_the_repair_hook_unchanged() {
    let mut world = world("api-429", |_| {}).await;
    world.repair = true;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let error = failing_turn(&node, &session).await;
    assert!(matches!(error.kind, ErrorKind::Provider { .. }), "{error}");
    assert!(!error.message.contains(NOT_SHOWN));
    assert_eq!(sign_ins(&node), 0);
    assert!(world.events("auth_login").is_empty());
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_sign_in_the_gateway_never_takes_is_withdrawn_and_its_login_stopped() {
    let mut world = world("sign-in", |_| {}).await;
    world.call_timeout = Some(Duration::from_secs(1));
    let node = world.start_node().await;
    assert_eq!(
        node.renew_credential().await.unwrap(),
        CredentialRenewal::Started
    );
    let (request, _unanswered) = sign_in_call(&node).await;
    let (settled, call) = settled_call(&node).await;
    call.reply().await.unwrap();
    assert_eq!(settled.id, request.id);
    assert_eq!(settled.state, SignInState::Cancelled);
    let login = world.events("auth_login")[0]["pid"].as_u64().unwrap() as u32;
    world
        .eventually("the login to be stopped", |_| !alive(login))
        .await;
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn sign_in_updates_go_to_the_gateway_one_at_a_time() {
    let mut world = world("sign-in", |_| {}).await;
    let node = world.start_node().await;
    assert_eq!(
        node.renew_credential().await.unwrap(),
        CredentialRenewal::Started
    );
    let (request, call) = sign_in_call(&node).await;
    call.reply().await.unwrap();
    let states = finish_sign_in(&node, &request.id, Duration::from_millis(200)).await;
    assert_eq!(states.last(), Some(&SignInState::Success));
    let overlaps = node.call_overlaps(&[NodeMethod::SignIn, NodeMethod::SignInSettled]);
    assert!(overlaps.is_empty(), "{overlaps:?}");
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn credential_health_follows_initialize_and_a_recovery_never_overtakes_its_failure() {
    let mut world = World::build("sign-in-recovers", |_| {}, manual_health()).await;
    let node = world.start_node().await;
    let (healthy, call) = health_call(&node).await;
    assert!(!healthy.degraded && healthy.reason.is_none());
    call.reply().await.unwrap();
    let session = node.new_session(vec![]).await.unwrap().session_id;

    let error = failing_turn(&node, &session).await;
    assert_eq!(error.kind, ErrorKind::Credential);
    let (degraded, held) = loop {
        let (health, call) = health_call(&node).await;
        if health.degraded {
            break (health, call);
        }
        call.reply().await.unwrap();
    };
    let mut turn = node.prompt(session, text("hi again")).await.unwrap();
    turn.expect(Match::message_contains("pong")).await.unwrap();
    turn.until_end().await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    held.reply().await.unwrap();

    let (recovered, call) = health_call(&node).await;
    call.reply().await.unwrap();
    assert!(!recovered.degraded);
    assert_eq!(recovered.since, degraded.since);
    let overlaps = node.call_overlaps(&[NodeMethod::CredentialHealth]);
    assert!(overlaps.is_empty(), "{overlaps:?}");
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_signed_out_claude_code_is_reported_degraded_after_initialize() {
    let mut world = World::build("signed-out", |_| {}, manual_health()).await;
    let node = world.start_node().await;
    let (health, call) = health_call(&node).await;
    call.reply().await.unwrap();
    assert!(health.degraded);
    assert_eq!(
        health.reason.as_deref(),
        Some("Claude Code is not signed in on this machine")
    );
    assert_eq!(
        world.events("auth_status")[0]["argv"],
        serde_json::json!(["auth", "status", "--json"])
    );
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn asking_to_sign_in_again_replaces_the_open_sign_in_and_kills_its_process_tree() {
    let mut world = world("sign-in-grandchild", |_| {}).await;
    let node = world.start_node().await;
    assert_eq!(
        node.renew_credential().await.unwrap(),
        CredentialRenewal::Started
    );
    let (first, call) = sign_in_call(&node).await;
    call.reply().await.unwrap();
    let pidfile = world.state().join("login.pid");
    world
        .eventually("the login's grandchild", |_| pidfile.exists())
        .await;
    let pids: Vec<u32> = std::fs::read_to_string(&pidfile)
        .unwrap()
        .lines()
        .map(|pid| pid.parse().unwrap())
        .collect();
    assert!(pids.iter().all(|pid| alive(*pid)));

    let renewing = tokio::spawn({
        let node = node.clone();
        async move { node.renew_credential().await.unwrap() }
    });
    let (settled, call) = settled_call(&node).await;
    call.reply().await.unwrap();
    assert_eq!(
        (settled.id.clone(), settled.state),
        (first.id.clone(), SignInState::Cancelled)
    );
    let (second, call) = sign_in_call(&node).await;
    call.reply().await.unwrap();
    assert_ne!(second.id, first.id);
    assert_eq!(renewing.await.unwrap(), CredentialRenewal::Started);
    world
        .eventually("the replaced login and its grandchild to be gone", |_| {
            pids.iter().all(|pid| !alive(*pid))
        })
        .await;

    let renewing = tokio::spawn({
        let node = node.clone();
        async move { node.renew_credential().await.unwrap() }
    });
    let (settled, stuck) = settled_call(&node).await;
    assert_eq!(settled.id, second.id);
    assert_eq!(renewing.await.unwrap(), CredentialRenewal::AlreadyRunning);
    stuck.reply().await.unwrap();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_login_that_fails_settles_failed_with_its_output() {
    let mut world = world("sign-in-fails", |_| {}).await;
    let node = world.start_node().await;
    assert_eq!(
        node.renew_credential().await.unwrap(),
        CredentialRenewal::Started
    );
    let (request, call) = sign_in_call(&node).await;
    call.reply().await.unwrap();
    let (settled, call) = settled_call(&node).await;
    call.reply().await.unwrap();
    assert_eq!(settled.id, request.id);
    assert_eq!(settled.state, SignInState::Failed);
    assert!(
        settled
            .reason
            .unwrap()
            .contains("Login failed: the stub refused to sign in")
    );
    world.stop_node().await;
}

/// Drives a sign-in the agent asked for: answer the code, approve the confirmation, and report
/// what the turn finally said.
async fn agent_sign_in(
    node: &SimNode,
    session: &rax::id::SessionId,
    approve_command: bool,
) -> String {
    node.set_verdicts(VerdictPolicy::AllowAll);
    let mut turn = node
        .prompt(session.clone(), text("sign me in"))
        .await
        .unwrap();
    let (request, call) = sign_in_call(node).await;
    assert_eq!(request.tool, "gcp-mcp");
    call.reply().await.unwrap();
    if approve_command {
        assert!(
            request.url.is_none(),
            "a command runs only once it is approved"
        );
        assert!(
            request
                .command
                .as_deref()
                .is_some_and(|line| line.contains("auth login")),
            "{:?}",
            request.command
        );
        node.answer(answer(&request.id, DisplayOutcome::Approved, None))
            .await
            .unwrap();
        let (ready, call) = settled_call(node).await;
        assert_eq!(ready.state, SignInState::Ready);
        assert!(
            ready.url.is_some_and(|url| url.contains("oauth/authorize")),
            "no link"
        );
        call.reply().await.unwrap();
    } else {
        assert!(
            request.url.is_some(),
            "a built-in flow offers its link up front"
        );
        assert!(request.command.is_none());
    }
    let states = finish_sign_in(node, &request.id, Duration::ZERO).await;
    assert_eq!(states.last(), Some(&SignInState::Success), "{states:?}");
    let said = turn
        .expect(Match::when("the agent's answer", |event| {
            matches!(event, Event::Message { .. })
        }))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    match said {
        Event::Message { content } => format!("{content:?}"),
        other => panic!("expected a message, got {other:?}"),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn the_agent_asks_for_credentials_and_the_owner_signs_in() {
    let mut world = world("auth-builtin", |_| {}).await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let said = agent_sign_in(&node, &session, false).await;
    assert!(said.contains("completed the sign-in for gcp-mcp"), "{said}");
    assert_eq!(sign_ins(&node), 1);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_command_the_agent_supplied_runs_only_after_the_owner_approves_it() {
    let mut world = world("auth-custom", |_| {}).await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let said = agent_sign_in(&node, &session, true).await;
    assert!(said.contains("completed the sign-in for gcp-mcp"), "{said}");
    world.stop_node().await;
}
