#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::sync::Arc;

use rax::session::GatewayCapabilities;
use rax::{Activity, ErrorKind, Event, NodeCall};
use rax_tokio::gateway::{LinkEvent, NewLink};
use rax_tokio::node::NodeLink;
use riggs_node::SessionKey;
use support::*;
use tokio::sync::Notify;

const IDLE: Activity = Activity {
    turns: 0,
    background_tasks: 0,
};

fn working(turns: u32, background_tasks: u32) -> Activity {
    Activity {
        turns,
        background_tasks,
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_gateway_hears_the_node_is_idle_once_it_has_initialized() {
    let mut h = harness(Fake::new(), ephemeral()).await;
    assert_eq!(*h.gateway.activity.borrow(), None);
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    h.gateway.activity(IDLE).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_turn_makes_the_node_busy_until_it_ends() {
    let mut h = harness(Fake::new(), ephemeral()).await;
    let finish = Arc::new(Notify::new());
    h.fake.on_prompt(script({
        let finish = finish.clone();
        move |turn: Turn| {
            let finish = finish.clone();
            async move {
                finish.notified().await;
                complete(&turn).await;
            }
        }
    }));
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    h.gateway.activity(IDLE).await;

    let mut events = accepted(&h.gateway.link, &session, "hello").await;
    h.gateway.activity(working(1, 0)).await;

    finish.notify_one();
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.gateway.activity(IDLE).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn work_outside_a_turn_keeps_the_node_busy() {
    let mut h = harness(Fake::new(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let first = SessionKey::parse(&new_session(&h.gateway.link).await).unwrap();
    let second = SessionKey::parse(&new_session(&h.gateway.link).await).unwrap();
    let host = h.fake.host().await;
    h.gateway.activity(IDLE).await;

    host.activity.set_background(&first, 2);
    h.gateway.activity(working(0, 2)).await;
    host.activity.set_background(&second, 1);
    h.gateway.activity(working(0, 3)).await;

    // A session's count replaces its last, and zero forgets the session.
    host.activity.set_background(&first, 0);
    h.gateway.activity(working(0, 1)).await;
    host.activity.set_background(&second, 0);
    h.gateway.activity(IDLE).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn every_gateway_hears_the_whole_nodes_counts() {
    let mut h = harness(Fake::new(), ephemeral()).await;
    let finish = Arc::new(Notify::new());
    h.fake.on_prompt(script({
        let finish = finish.clone();
        move |turn: Turn| {
            let finish = finish.clone();
            async move {
                finish.notified().await;
                complete(&turn).await;
            }
        }
    }));
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let mut work = second_gateway(&h.node, "work").await;
    initialize(&work.gateway.link, GatewayCapabilities::default()).await;
    work.gateway.activity(IDLE).await;

    // The turn belongs to the first gateway, and the second still hears the node is busy.
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "hello").await;
    h.gateway.activity(working(1, 0)).await;
    work.gateway.activity(working(1, 0)).await;

    finish.notify_one();
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.gateway.activity(IDLE).await;
    work.gateway.activity(IDLE).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_gateway_too_old_to_read_activity_is_not_told_again() {
    let h = harness(Fake::new(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let (server, mut new_links) = gateway(gateway_config()).await;
    let (handle, events) = NodeLink::start(node_config(server.local_addr())).unwrap();
    let serving = tokio::spawn({
        let (node, handle) = (h.node.server.clone(), handle.clone());
        async move { node.serve_gateway("old", handle, events).await }
    });
    let NewLink { link, mut events } = within(new_links.recv()).await.unwrap();
    initialize(&link, GatewayCapabilities::default()).await;

    let LinkEvent::Request {
        id,
        call: NodeCall::Activity(_),
    } = within(events.recv()).await.unwrap()
    else {
        panic!("expected the node's first activity report")
    };
    let unsupported = rax::Error::new(ErrorKind::Unsupported, "no such method");
    link.fault(id, unsupported).await.unwrap();

    let session = SessionKey::parse(&new_session(&link).await).unwrap();
    h.fake.host().await.activity.set_background(&session, 1);
    assert!(
        quiet(events.recv()).await,
        "the node reported activity to a gateway that cannot read it"
    );
    h.node.shut_down().await;
    drop(serving);
}
