#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::time::Duration;

use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::session::GatewayCapabilities;
use rax::tool::{DeniedBy, ToolCallStatus, ToolCallUpdate, ToolKind};
use rax::{Decision, Event, Open, ToolCall, ToolVerdict};
use rax_tokio::gateway::LinkEvent;
use riggs_node::{BackendEvent, NotDelivered};
use serde_json::json;
use support::*;
use tokio::sync::mpsc;
use tokio::time::Instant;

fn bash(id: &str) -> ToolCall {
    ToolCall {
        id: id.into(),
        name: "Bash".into(),
        title: Some("touch ran".into()),
        kind: ToolKind::Execute,
        input: Some(json!({"command": "touch ran"})),
        content: vec![],
    }
}

fn update(id: &str, status: ToolCallStatus) -> BackendEvent {
    BackendEvent::ToolCallUpdate(ToolCallUpdate {
        id: id.into(),
        status,
        title: None,
        content: vec![],
        output: None,
    })
}

struct Gated {
    decisions: mpsc::UnboundedReceiver<Decision>,
    ran: mpsc::UnboundedReceiver<()>,
}

fn gated(fake: &Fake, wait: Duration) -> Gated {
    let (decided, decisions) = mpsc::unbounded_channel();
    let (tool_ran, ran) = mpsc::unbounded_channel();
    fake.on_prompt(script(move |turn: Turn| {
        let (decided, tool_ran) = (decided.clone(), tool_ran.clone());
        async move {
            let events = &turn.handle.events;
            let decision = turn
                .handle
                .gate
                .hold(bash("tc1"), Instant::now() + wait)
                .await;
            decided.send(decision.clone()).unwrap();
            match decision {
                Decision::Allow => {
                    events
                        .send(update("tc1", ToolCallStatus::InProgress))
                        .await
                        .unwrap();
                    tool_ran.send(()).unwrap();
                    events
                        .send(update("tc1", ToolCallStatus::Completed))
                        .await
                        .unwrap();
                }
                Decision::Deny { reason, .. } => {
                    events
                        .send(update("tc1", ToolCallStatus::Denied))
                        .await
                        .unwrap();
                    let said = format!("I was told: {}", reason.unwrap_or_default());
                    let message = BackendEvent::Message(ContentBlock::text(said).into());
                    events.send(message).await.unwrap();
                }
            }
            complete(&turn).await;
        }
    }));
    Gated { decisions, ran }
}

fn status_of(event: Event) -> ToolCallStatus {
    match event {
        Event::ToolCallUpdate { tool_call_update } => tool_call_update.status,
        other => panic!("expected a tool call update, got {other:?}"),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_held_call_does_not_run_until_it_is_allowed() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_secs(30));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "run it").await;

    let Event::ToolCall { tool_call } = next_event(&mut events).await else {
        panic!("expected the tool call")
    };
    assert_eq!(tool_call, bash("tc1"));
    assert!(
        gated.ran.try_recv().is_err(),
        "the tool ran before its verdict"
    );

    let allow = ToolVerdict {
        id: "tc1".into(),
        decision: Decision::Allow,
    };
    link.verdict(allow).await.unwrap();
    assert_eq!(
        status_of(next_event(&mut events).await),
        ToolCallStatus::InProgress
    );
    assert_eq!(
        status_of(next_event(&mut events).await),
        ToolCallStatus::Completed
    );
    within(gated.ran.recv()).await.unwrap();
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_denied_call_never_runs_and_the_agent_hears_the_reason() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_secs(30));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "run it").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::ToolCall { .. }
    ));

    let reason = "Deleting files is not allowed here.";
    let deny = ToolVerdict {
        id: "tc1".into(),
        decision: Decision::Deny {
            by: DeniedBy::Policy,
            reason: Some(reason.into()),
        },
    };
    link.verdict(deny).await.unwrap();
    assert_eq!(
        status_of(next_event(&mut events).await),
        ToolCallStatus::Denied
    );
    let Event::Message {
        content: Open::Known(ContentBlock::Text { text }),
    } = next_event(&mut events).await
    else {
        panic!("expected the agent's message")
    };
    assert!(text.contains(reason), "{text}");
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    assert!(gated.ran.try_recv().is_err(), "a denied tool ran");
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn the_node_denies_by_timeout_at_the_backend_deadline() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_millis(200));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "run it").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::ToolCall { .. }
    ));

    let decision = within(gated.decisions.recv()).await.unwrap();
    assert!(
        matches!(
            decision,
            Decision::Deny {
                by: DeniedBy::Timeout,
                reason: Some(_)
            }
        ),
        "{decision:?}"
    );
    assert_eq!(
        status_of(next_event(&mut events).await),
        ToolCallStatus::Denied
    );
    assert!(gated.ran.try_recv().is_err());
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_call_still_held_when_its_turn_ends_waits_for_its_verdict() {
    let fake = Fake::new();
    let (decided, mut decisions) = mpsc::unbounded_channel();
    fake.on_prompt(script(move |turn: Turn| {
        let decided = decided.clone();
        async move {
            let gate = turn.handle.gate.clone();
            let (announced, held) = tokio::sync::oneshot::channel();
            tokio::spawn(async move {
                let hold = gate.hold(bash("sub1"), Instant::now() + Duration::from_secs(30));
                let _ = announced.send(());
                decided.send(hold.await).unwrap();
            });
            held.await.unwrap();
            complete(&turn).await;
        }
    }));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "start a subagent").await;

    let Event::ToolCall { tool_call } = next_event(&mut events).await else {
        panic!("expected the subagent's tool call")
    };
    assert_eq!(tool_call.id, "sub1".into());
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    assert!(
        decisions.try_recv().is_err(),
        "the call was settled when its turn ended"
    );

    let allow = ToolVerdict {
        id: "sub1".into(),
        decision: Decision::Allow,
    };
    link.verdict(allow).await.unwrap();
    let decision = tokio::time::timeout(Duration::from_secs(10), decisions.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(decision, Decision::Allow);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_verdict_for_an_unknown_call_is_ignored() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_secs(30));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    let stray = ToolVerdict {
        id: "nobody-holds-this".into(),
        decision: Decision::Allow,
    };
    link.verdict(stray).await.unwrap();

    let session = new_session(link).await;
    let mut events = accepted(link, &session, "run it").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::ToolCall { .. }
    ));
    let allow = ToolVerdict {
        id: "tc1".into(),
        decision: Decision::Allow,
    };
    link.verdict(allow).await.unwrap();
    assert_eq!(within(gated.decisions.recv()).await, Some(Decision::Allow));
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_verdict_sent_after_an_accepted_resume_is_honoured() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_secs(30));
    let mut h = harness_behind_proxy(fake, ephemeral()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "run it").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::ToolCall { .. }
    ));

    h.proxy.as_ref().unwrap().sever();
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Disconnected { .. }
    ));
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Resumed
    ));
    assert!(
        gated.decisions.try_recv().is_err(),
        "a socket drop alone denied the call"
    );

    let allow = ToolVerdict {
        id: "tc1".into(),
        decision: Decision::Allow,
    };
    h.gateway.link.verdict(allow).await.unwrap();
    assert_eq!(within(gated.decisions.recv()).await, Some(Decision::Allow));
    assert_eq!(
        status_of(next_event(&mut events).await),
        ToolCallStatus::InProgress
    );
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_refused_resume_denies_held_calls_unavailable() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_secs(30));
    let h = harness_behind_proxy(fake, ephemeral()).await;
    let (restarted, mut restarted_links) = gateway(gateway_config()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "run it").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::ToolCall { .. }
    ));

    let proxy = h.proxy.as_ref().unwrap();
    proxy.retarget(restarted.local_addr());
    proxy.sever();
    let decision = within(gated.decisions.recv()).await.unwrap();
    assert!(
        matches!(
            decision,
            Decision::Deny {
                by: DeniedBy::Unavailable,
                reason: Some(_)
            }
        ),
        "{decision:?}"
    );
    accept(&mut restarted_links).await;
    assert!(gated.ran.try_recv().is_err());
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_link_that_resumes_after_its_grace_ran_out_ends_the_turns_it_gave_up_on() {
    let fake = Fake::new();
    let mut gated = gated(&fake, Duration::from_secs(30));
    let mut config = ephemeral();
    config.link_grace = Duration::from_millis(300);
    let mut h = harness_behind_proxy(fake, config).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "run it").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::ToolCall { .. }
    ));

    let proxy = h.proxy.as_ref().unwrap();
    proxy.retarget(closed_port().await);
    proxy.sever();
    let decision = within(gated.decisions.recv()).await.unwrap();
    assert!(
        matches!(
            decision,
            Decision::Deny {
                by: DeniedBy::Unavailable,
                ..
            }
        ),
        "{decision:?}"
    );

    proxy.retarget(h.server.local_addr());
    loop {
        if let LinkEvent::Resumed = next_link(&mut h.gateway.events).await {
            break;
        }
    }
    ended(&mut events).await;
    assert!(gated.ran.try_recv().is_err());
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_background_tool_call_is_held_until_its_verdict() {
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, GatewayCapabilities::default()).await;
    let session = new_session(link).await;
    let key = riggs_node::SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let holding = tokio::spawn(async move {
        let deadline = Instant::now() + Duration::from_secs(30);
        host.gate.hold_background(&key, bash("bg1"), deadline).await
    });
    let LinkEvent::Background {
        session_id,
        event: Open::Known(BackgroundEvent::ToolCall { tool_call }),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected a background tool call")
    };
    assert_eq!(session_id, session);
    assert_eq!(tool_call.id, "bg1".into());
    assert!(!holding.is_finished());

    let allow = ToolVerdict {
        id: "bg1".into(),
        decision: Decision::Allow,
    };
    link.verdict(allow).await.unwrap();
    assert_eq!(within(holding).await.unwrap(), Decision::Allow);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn once_the_grace_runs_out_held_calls_are_denied_and_background_events_dropped() {
    let fake = Fake::new();
    let mut config = ephemeral();
    config.link_grace = Duration::from_millis(300);
    let mut h = harness_behind_proxy(fake.clone(), config).await;
    let session = new_session(&h.gateway.link).await;
    let key = riggs_node::SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let holding = tokio::spawn({
        let host = host.clone();
        async move {
            let deadline = Instant::now() + Duration::from_secs(30);
            host.gate.hold_background(&key, bash("bg1"), deadline).await
        }
    });
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Background { .. }
    ));

    let proxy = h.proxy.as_ref().unwrap();
    proxy.retarget(closed_port().await);
    proxy.sever();
    let decision = within(holding).await.unwrap();
    assert!(
        matches!(
            decision,
            Decision::Deny {
                by: DeniedBy::Unavailable,
                reason: Some(_)
            }
        ),
        "{decision:?}"
    );

    let status = BackgroundEvent::Status {
        text: "still going".into(),
    };
    assert!(matches!(
        host.background.send(&key, status).await,
        Err(NotDelivered::NoGateway)
    ));
    h.node.shut_down().await;
}
