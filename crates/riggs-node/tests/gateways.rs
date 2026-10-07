#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

//! One node, several gateways: each session belongs to the gateway that opened it.

mod support;

use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::session::{GatewayCapabilities, SessionRef};
use rax::tool::{CallTool, ToolOutcome};
use rax::{ErrorKind, GatewayCall, GatewayReply, NodeCall, NodeReply, Open, Rejection};
use rax_tokio::gateway::LinkEvent;
use riggs_node::{NotDelivered, SessionKey, Stopped};
use serde_json::json;
use support::*;

#[tokio::test(flavor = "multi_thread")]
async fn two_links_are_live_at_once_and_each_opens_its_own_sessions() {
    let fake = Fake::new();
    let h = harness(fake.clone(), ephemeral()).await;
    let work = second_gateway(&h.node, "work").await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    initialize(&work.gateway.link, GatewayCapabilities::default()).await;

    let mine = new_session(&h.gateway.link).await;
    let theirs = new_session(&work.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &mine, "hello").await;
    assert!(matches!(
        next_event(&mut events).await,
        rax::Event::Complete { .. }
    ));
    let mut events = accepted(&work.gateway.link, &theirs, "hello").await;
    assert!(matches!(
        next_event(&mut events).await,
        rax::Event::Complete { .. }
    ));
    h.node.shut_down().await;
    assert_eq!(within(work.serving).await.unwrap(), Stopped::Shutdown);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_session_is_unknown_to_every_gateway_but_its_own() {
    let fake = Fake::new();
    let h = harness(fake.clone(), ephemeral()).await;
    let work = second_gateway(&h.node, "work").await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    initialize(&work.gateway.link, GatewayCapabilities::default()).await;
    let mine = new_session(&h.gateway.link).await;

    let pending = prompt(&work.gateway.link, &mine, "let me in").await;
    assert_eq!(faulted(pending).await.kind, ErrorKind::UnknownSession);
    for call in [
        GatewayCall::Cancel(SessionRef {
            session_id: mine.clone(),
        }),
        GatewayCall::CloseSession(SessionRef {
            session_id: mine.clone(),
        }),
    ] {
        let Err(rax_tokio::CallError::Fault(fault)) = call_on(&work, call).await else {
            panic!("expected a fault")
        };
        assert_eq!(fault.kind, ErrorKind::UnknownSession);
    }

    // The session is untouched, and its own gateway still drives it.
    let mut events = accepted(&h.gateway.link, &mine, "still mine").await;
    assert!(matches!(
        next_event(&mut events).await,
        rax::Event::Complete { .. }
    ));
    h.node.shut_down().await;
}

async fn call_on(second: &Second, call: GatewayCall) -> Result<GatewayReply, rax_tokio::CallError> {
    support::call(&second.gateway.link, call).await
}

#[tokio::test(flavor = "multi_thread")]
async fn tool_calls_and_background_events_reach_the_sessions_own_gateway() {
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    let mut work = second_gateway(&h.node, "work").await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    initialize(&work.gateway.link, GatewayCapabilities::default()).await;
    let _mine = new_session(&h.gateway.link).await;
    let theirs = new_session(&work.gateway.link).await;
    let key = SessionKey::parse(&theirs).unwrap();
    let host = fake.host().await;

    let calling = tokio::spawn({
        let host = host.clone();
        async move { host.tools.call(&key, "editor", "read", json!({})).await }
    });
    let LinkEvent::Request {
        id,
        call: NodeCall::CallTool(CallTool { session_id, .. }),
    } = next_link(&mut work.gateway.events).await
    else {
        panic!("expected the tool call on the session's gateway")
    };
    assert_eq!(session_id, theirs);
    let outcome = ToolOutcome {
        content: "contents".into(),
        is_error: false,
    };
    work.gateway
        .link
        .reply(id, NodeReply::CallTool(outcome.clone()))
        .await
        .unwrap();
    assert_eq!(within(calling).await.unwrap().unwrap(), outcome);

    let content = ContentBlock::text("done in the background").into();
    host.background
        .send(&key, BackgroundEvent::Message { content })
        .await
        .unwrap();
    let LinkEvent::Background { session_id, event } = next_link(&mut work.gateway.events).await
    else {
        panic!("expected the background event on the session's gateway")
    };
    assert_eq!(session_id, theirs);
    assert!(matches!(
        event,
        Open::Known(BackgroundEvent::Message { .. })
    ));
    assert!(
        quiet(h.gateway.events.recv()).await,
        "the other gateway heard it"
    );
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_rejection_on_one_link_leaves_the_other_serving() {
    let fake = Fake::new();
    let h = harness(fake.clone(), ephemeral()).await;
    let work = second_gateway(&h.node, "work").await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    initialize(&work.gateway.link, GatewayCapabilities::default()).await;
    let theirs = new_session(&work.gateway.link).await;
    let key = SessionKey::parse(&theirs).unwrap();

    work.gateway
        .link
        .reject(Rejection {
            key: Some("work_access".into()),
            message: "malformed".into(),
        })
        .await;
    assert!(matches!(
        within(work.serving).await.unwrap(),
        Stopped::Rejected(_)
    ));

    // The work gateway's sessions have nowhere to go, and the other link carries on.
    let host = fake.host().await;
    let content = ContentBlock::text("nobody hears this").into();
    assert!(matches!(
        host.background
            .send(&key, BackgroundEvent::Message { content })
            .await,
        Err(NotDelivered::NoGateway)
    ));
    let mine = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &mine, "still here").await;
    assert!(matches!(
        next_event(&mut events).await,
        rax::Event::Complete { .. }
    ));
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn each_gateway_hears_its_own_metadata_over_the_shared_table() {
    let fake = Fake::new();
    let mut config = ephemeral();
    config.metadata =
        serde_json::from_value(json!({"shared_key": 1, "murtaugh_access": "top"})).unwrap();
    config.gateway_metadata.insert(
        "work".into(),
        serde_json::from_value(json!({"murtaugh_access": "work"})).unwrap(),
    );
    let h = harness(fake.clone(), config).await;
    let work = second_gateway(&h.node, "work").await;
    let primary = initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let theirs = initialize(&work.gateway.link, GatewayCapabilities::default()).await;
    assert_eq!(primary.metadata.get("murtaugh_access"), Some(&json!("top")));
    assert_eq!(theirs.metadata.get("murtaugh_access"), Some(&json!("work")));
    assert_eq!(theirs.metadata.get("shared_key"), Some(&json!(1)));
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_record_from_before_gateways_belongs_to_the_primary() {
    let dir = tempfile::tempdir().unwrap();
    let fake = Fake::new();
    let h = harness(fake.clone(), durable(dir.path())).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    h.node.shut_down().await;

    // Written as version 1 did: no gateway.
    let path = dir
        .path()
        .join(format!("{}.json", SessionKey::parse(&session).unwrap()));
    let mut record: serde_json::Value =
        serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    assert_eq!(record["gateway"], json!("default"));
    record["v"] = json!(1);
    record.as_object_mut().unwrap().remove("gateway");
    std::fs::write(&path, serde_json::to_vec(&record).unwrap()).unwrap();

    let fake = Fake::new();
    let h = harness(fake.clone(), durable(dir.path())).await;
    let work = second_gateway(&h.node, "work").await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    initialize(&work.gateway.link, GatewayCapabilities::default()).await;
    let pending = prompt(&work.gateway.link, &session, "mine now?").await;
    assert_eq!(faulted(pending).await.kind, ErrorKind::UnknownSession);
    let mut events = accepted(&h.gateway.link, &session, "back again").await;
    assert!(matches!(
        next_event(&mut events).await,
        rax::Event::Complete { .. }
    ));
    h.node.shut_down().await;
}
