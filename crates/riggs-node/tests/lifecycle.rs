#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::process::Stdio;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use rax::event::StopReason;
use rax::interaction::{DisplayOutcome, Question, QuestionRequest};
use rax::session::GatewayCapabilities;
use rax::tool::{DeniedBy, ToolKind};
use rax::{Decision, ErrorKind, Event, NodeCall, ToolCall};
use rax_tokio::Disconnect;
use rax_tokio::gateway::LinkEvent;
use riggs_node::{BackendEvent, NodeServer, ServerError, SessionKey, Stopped, StoreError};
use support::*;
use tokio::sync::mpsc;
use tokio::time::Instant;
use tokio_util::sync::CancellationToken;

#[tokio::test(flavor = "multi_thread")]
async fn after_a_gateway_restart_open_turns_reset_and_old_sessions_still_prompt() {
    let fake = Fake::new();
    let mut seen = fake.seen();
    fake.on_prompt(script(|turn: Turn| async move {
        if turn.text == "long job" {
            turn.handle.cancelled.cancelled().await;
            let stop = BackendEvent::Complete(Some(StopReason::Cancelled));
            let _ = turn.handle.events.send(stop).await;
            return;
        }
        complete(&turn).await;
    }));
    let h = harness_behind_proxy(fake, ephemeral()).await;
    let (restarted, mut restarted_links) = gateway(gateway_config()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let _abandoned = accepted(&h.gateway.link, &session, "long job").await;

    let proxy = h.proxy.as_ref().unwrap();
    proxy.retarget(restarted.local_addr());
    proxy.sever();
    let restarted = accept(&mut restarted_links).await;
    seen.wait_for(|seen| matches!(seen, Seen::Cancel(_))).await;

    initialize(&restarted.link, GatewayCapabilities::default()).await;
    let mut events = accepted(&restarted.link, &session, "hello again").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.node.shut_down().await;
}

async fn session_on_first_node(dir: &std::path::Path) -> (rax::id::SessionId, std::path::PathBuf) {
    let h = harness(Fake::new(), durable(dir)).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "remember this").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    assert_eq!(h.node.shut_down().await, Stopped::Shutdown);
    let record = dir.join(format!("{session}.json"));
    (session, record)
}

#[tokio::test(flavor = "multi_thread")]
async fn a_durable_session_is_restored_on_its_first_prompt_after_a_restart() {
    let dir = tempfile::tempdir().unwrap();
    let (session, record) = session_on_first_node(dir.path()).await;
    assert!(record.exists());

    let fake = Fake::new();
    let mut seen = fake.seen();
    let h = harness(fake, durable(dir.path())).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let mut events = accepted(&h.gateway.link, &session, "what did I say").await;
    let key = SessionKey::parse(&session).unwrap();
    assert_eq!(
        seen.wait_for(|seen| matches!(seen, Seen::Restore(_))).await,
        Seen::Restore(key)
    );
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_durable_session_the_agent_lost_is_unknown_and_its_record_removed() {
    let dir = tempfile::tempdir().unwrap();
    let (session, record) = session_on_first_node(dir.path()).await;

    let fake = Fake::new();
    fake.restores(false);
    let h = harness(fake, durable(dir.path())).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let fault = faulted(prompt(&h.gateway.link, &session, "what did I say").await).await;
    assert_eq!(fault.kind, ErrorKind::UnknownSession);
    assert!(!record.exists());

    let fresh = new_session(&h.gateway.link).await;
    assert_ne!(fresh, session);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_corrupt_record_is_unknown_and_removed() {
    let dir = tempfile::tempdir().unwrap();
    let (session, record) = session_on_first_node(dir.path()).await;
    std::fs::write(&record, b"{not json").unwrap();

    let h = harness(Fake::new(), durable(dir.path())).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let fault = faulted(prompt(&h.gateway.link, &session, "hello").await).await;
    assert_eq!(fault.kind, ErrorKind::UnknownSession);
    assert!(!record.exists());
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_ephemeral_node_forgets_its_sessions_on_restart() {
    let h = harness(Fake::new(), ephemeral()).await;
    let session = new_session(&h.gateway.link).await;
    h.node.shut_down().await;

    let h = harness(Fake::new(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let fault = faulted(prompt(&h.gateway.link, &session, "hello").await).await;
    assert_eq!(fault.kind, ErrorKind::UnknownSession);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_second_server_on_the_same_store_is_refused() {
    let dir = tempfile::tempdir().unwrap();
    let first = NodeServer::new(Fake::new(), durable(dir.path())).unwrap();
    let Err(ServerError::Store(refusal)) = NodeServer::new(Fake::new(), durable(dir.path())) else {
        panic!("the second server must be refused")
    };
    assert!(matches!(
        &refusal,
        StoreError::Locked { pid: Some(pid), .. } if *pid == std::process::id()
    ));
    assert!(
        refusal
            .to_string()
            .contains("another riggs is already serving"),
        "{refusal}"
    );
    drop(first);
    NodeServer::new(Fake::new(), durable(dir.path())).unwrap();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_store_opens_while_this_process_is_starting_other_programs() {
    let dir = tempfile::tempdir().unwrap();
    let stop = Arc::new(AtomicBool::new(false));
    let spawning: Vec<_> = (0..4)
        .map(|_| {
            let stop = stop.clone();
            std::thread::spawn(move || {
                while !stop.load(Ordering::Relaxed) {
                    let mut child = std::process::Command::new("/bin/sh")
                        .args(["-c", "exit"])
                        .stdin(Stdio::null())
                        .stdout(Stdio::null())
                        .stderr(Stdio::null())
                        .spawn()
                        .unwrap();
                    let _ = child.wait();
                }
            })
        })
        .collect();
    let opened = (0..100).try_for_each(|round| {
        NodeServer::new(Fake::new(), durable(dir.path()))
            .map(drop)
            .map_err(|err| format!("round {round}: {err}"))
    });
    stop.store(true, Ordering::Relaxed);
    for thread in spawning {
        thread.join().unwrap();
    }
    opened.unwrap();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_shut_down_node_frees_its_store_at_once_even_when_a_turn_outlives_the_grace() {
    let dir = tempfile::tempdir().unwrap();
    let release = CancellationToken::new();
    for round in 0..20 {
        let fake = Fake::new();
        if round % 2 == 0 {
            let stuck = release.clone();
            fake.on_prompt(script(move |turn: Turn| {
                let stuck = stuck.clone();
                async move {
                    stuck.cancelled().await;
                    drop(turn);
                }
            }));
        }
        let mut config = durable(dir.path());
        config.shutdown_grace = Duration::from_millis(20);
        let h = harness(fake, config).await;
        initialize(&h.gateway.link, GatewayCapabilities::default()).await;
        let session = new_session(&h.gateway.link).await;
        let _events = accepted(&h.gateway.link, &session, "take your time").await;
        assert_eq!(h.node.shut_down().await, Stopped::Shutdown);
        if let Err(err) = NodeServer::new(Fake::new(), durable(dir.path())) {
            panic!("round {round}: {err}");
        }
    }
    release.cancel();
}

#[tokio::test(flavor = "multi_thread")]
async fn shutdown_does_not_wait_for_a_credential_report_the_gateway_ignores() {
    let fake = Fake::new();
    fake.reports_health("claude-code");
    let mut h = harness(fake, ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    loop {
        if let LinkEvent::Request { call, .. } = next_link(&mut h.gateway.events).await {
            assert!(matches!(call, NodeCall::CredentialHealth(_)), "{call:?}");
            break;
        }
    }

    let started = Instant::now();
    assert_eq!(h.node.shut_down().await, Stopped::Shutdown);
    let took = started.elapsed();
    assert!(
        took < Duration::from_secs(1),
        "the shutdown waited {took:?} on a report nobody answered"
    );
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Disconnected {
            reason: Disconnect::PeerClosed
        }
    ));
}

#[tokio::test(flavor = "multi_thread")]
async fn shutdown_denies_held_calls_dismisses_questions_and_closes_cleanly() {
    let fake = Fake::new();
    let mut seen = fake.seen();
    let (outcomes, mut settled) = mpsc::unbounded_channel::<String>();
    fake.on_prompt(script(move |turn: Turn| {
        let outcomes = outcomes.clone();
        async move {
            let call = ToolCall {
                id: "tc1".into(),
                name: "Bash".into(),
                title: None,
                kind: ToolKind::Execute,
                input: None,
                content: vec![],
            };
            let question = QuestionRequest {
                id: "q1".into(),
                title: None,
                questions: vec![Question {
                    key: "go".into(),
                    header: None,
                    question: "Go?".into(),
                    options: vec![],
                    multi_select: false,
                }],
            };
            let deadline = Instant::now() + Duration::from_secs(30);
            let (decision, answer) = tokio::join!(
                turn.handle.gate.hold(call, deadline),
                turn.handle.prompts.question(question),
            );
            let denied = matches!(
                decision,
                Decision::Deny {
                    by: DeniedBy::Unavailable,
                    ..
                }
            );
            outcomes.send(format!("denied={denied}")).unwrap();
            outcomes.send(format!("{:?}", answer.outcome)).unwrap();
            turn.handle.cancelled.cancelled().await;
            let stop = BackendEvent::Complete(Some(StopReason::Cancelled));
            let _ = turn.handle.events.send(stop).await;
        }
    }));
    let mut h = harness(fake, ephemeral()).await;
    initialize(&h.gateway.link, all_caps()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "hold on").await;
    let mut raised = [next_event(&mut events).await, next_event(&mut events).await];
    raised.sort_by_key(|event| matches!(event, Event::Question { .. }));
    assert!(matches!(raised[0], Event::ToolCall { .. }));
    assert!(matches!(raised[1], Event::Question { .. }));

    let stopped = tokio::spawn(h.node.shut_down());
    assert_eq!(within(settled.recv()).await.unwrap(), "denied=true");
    assert_eq!(
        within(settled.recv()).await.unwrap(),
        format!("{:?}", DisplayOutcome::Dismissed)
    );
    assert_eq!(
        next_event(&mut events).await,
        Event::Complete {
            stop_reason: Some(StopReason::Cancelled)
        }
    );
    ended(&mut events).await;
    seen.wait_for(|seen| matches!(seen, Seen::Shutdown)).await;
    assert_eq!(within(stopped).await.unwrap(), Stopped::Shutdown);
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Disconnected {
            reason: Disconnect::PeerClosed
        }
    ));
}
