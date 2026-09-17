#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use futures_util::{SinkExt, StreamExt};
use rax::frame::ResumeOutcome;
use rax::message::{from_payload, to_payload};
use rax::open::{Subject, UnhandledReason};
use rax::session::{GatewayCapabilities, SessionDurability, ToolGate};
use rax::{ErrorKind, Frame, GatewayReply, Link, LinkConfig, NodeMessage, Open, Received};
use rax_tokio::CallError;
use serde_json::json;
use serde_json::value::RawValue;
use support::*;
use tokio::net::TcpListener;
use tokio_tungstenite::tungstenite::Message;

#[tokio::test(flavor = "multi_thread")]
async fn initialize_declares_every_capability_the_backend_resolved() {
    let h = harness(Fake::new(), ephemeral()).await;
    let initialized = initialize(&h.gateway.link, GatewayCapabilities::default()).await;

    assert_eq!(initialized.protocol_version, rax::PROTOCOL_VERSION);
    let caps = initialized.capabilities;
    assert_eq!(caps.interruptible, Some(true));
    assert_eq!(caps.tool_gate, ToolGate::EveryCall);
    assert_eq!(
        caps.sessions,
        SessionDurability::Ephemeral,
        "an ephemeral store must not promise durable sessions"
    );
    assert!(caps.prompt.image);
    assert_eq!(caps.resource_schemes, vec!["chat".to_owned()]);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_durable_store_and_a_durable_backend_declare_durable_sessions() {
    let dir = tempfile::tempdir().unwrap();
    let h = harness(Fake::new(), durable(dir.path())).await;
    let initialized = initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    assert_eq!(
        initialized.capabilities.sessions,
        SessionDurability::Durable
    );
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unknown_method_is_faulted_unsupported_and_the_link_stays_up() {
    let h = harness(Fake::new(), ephemeral()).await;
    let fork = Open::Unknown {
        name: "session.fork".into(),
        raw: json!({"method": "session.fork", "body": {"session_id": "s1"}}),
    };
    let pending = within(h.gateway.link.call(fork)).await.unwrap();
    let Err(CallError::Fault(fault)) = within(pending.reply).await else {
        panic!("expected a fault")
    };
    assert_eq!(fault.kind, ErrorKind::Unsupported);
    assert!(fault.message.contains("session.fork"), "{}", fault.message);

    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    h.node.shut_down().await;
}

async fn read_frame(
    socket: &mut tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
) -> Frame {
    loop {
        match within(socket.next()).await {
            Some(Ok(Message::Binary(bytes))) => return Frame::decode(&bytes).unwrap(),
            Some(Ok(Message::Ping(_) | Message::Pong(_))) => {}
            other => panic!("expected a frame, got {other:?}"),
        }
    }
}

async fn send_frame(
    socket: &mut tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
    frame: Frame,
) {
    socket
        .send(Message::Binary(frame.encode().unwrap().into()))
        .await
        .unwrap();
}

async fn next_message(
    socket: &mut tokio_tungstenite::WebSocketStream<tokio::net::TcpStream>,
    link: &mut Link,
) -> NodeMessage {
    loop {
        let frame = read_frame(socket).await;
        if let Received::Deliver { delivery, payload } = link.receive(frame).unwrap() {
            let _ = link.consumed(delivery);
            match from_payload::<Open<NodeMessage>>(&payload).unwrap() {
                Open::Known(message) => return message,
                Open::Unknown { name, .. } => panic!("the node sent an unknown kind {name}"),
            }
        }
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unknown_message_kind_is_answered_unhandled_and_the_link_stays_up() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let node = run_node(
        Fake::new(),
        ephemeral(),
        node_config(listener.local_addr().unwrap()),
    );
    let (tcp, _) = within(listener.accept()).await.unwrap();
    let mut socket = within(tokio_tungstenite::accept_async(tcp)).await.unwrap();
    assert!(matches!(
        read_frame(&mut socket).await,
        Frame::Resume { epoch: 0, .. }
    ));
    let epoch = 7;
    let refusal = ResumeOutcome::Refused {
        reason: "fresh".into(),
    };
    send_frame(
        &mut socket,
        Frame::Resumed {
            last_seen: 0,
            epoch,
            outcome: refusal,
        },
    )
    .await;
    let mut link = Link::new(LinkConfig {
        epoch,
        ..Default::default()
    });

    let shiny = RawValue::from_string(json!({"k": "shiny", "body": {}}).to_string()).unwrap();
    send_frame(&mut socket, link.send(shiny).unwrap()).await;
    let NodeMessage::Unhandled { stream, body } = next_message(&mut socket, &mut link).await else {
        panic!("expected unhandled")
    };
    assert_eq!(stream, None);
    assert_eq!(
        body.subject,
        Subject::MessageKind {
            name: "shiny".into()
        }
    );
    assert_eq!(body.reason, UnhandledReason::UnsupportedType);

    let initialize = json!({
        "k": "req", "id": "g1", "method": "initialize",
        "body": {"protocol_version": 1, "capabilities": {}}
    });
    let payload = to_payload(&initialize).unwrap();
    send_frame(&mut socket, link.send(payload).unwrap()).await;
    let NodeMessage::Reply { id, reply } = next_message(&mut socket, &mut link).await else {
        panic!("expected the initialize reply")
    };
    assert_eq!(id, "g1".into());
    assert!(matches!(reply, GatewayReply::Initialize(_)));
    node.server.shutdown_token().cancel();
    within(node.serving).await.unwrap();
}
