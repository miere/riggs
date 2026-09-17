#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use futures_util::FutureExt;
use rax::content::ContentBlock;
use rax::event::StopReason;
use rax::open::{Subject, UnhandledReason};
use rax::session::{GatewayCapabilities, NewSession, SessionRef};
use rax::{ErrorKind, Event, GatewayCall, GatewayReply};
use riggs_node::BackendEvent;
use support::*;
use tokio::sync::{Mutex, oneshot};

#[tokio::test(flavor = "multi_thread")]
async fn a_prompt_is_accepted_before_its_first_event_and_the_turn_ends() {
    let h = harness(Fake::new(), ephemeral()).await;
    h.fake.on_prompt(script(|turn: Turn| async move {
        let reply = format!("you said {}", turn.text);
        let message = BackendEvent::Message(ContentBlock::text(reply).into());
        turn.handle.events.send(message).await.unwrap();
        complete(&turn).await;
    }));
    let link = &h.gateway.link;
    initialize(link, GatewayCapabilities::default()).await;
    let session = new_session(link).await;

    let mut pending = prompt(link, &session, "hello").await;
    let first = next_event(&mut pending.events).await;
    let accepted = (&mut pending.reply)
        .now_or_never()
        .expect("the prompt reply must arrive before the first event");
    assert!(matches!(accepted, Ok(GatewayReply::Prompt(_))));
    assert_eq!(
        first,
        Event::Message {
            content: ContentBlock::text("you said hello").into()
        }
    );
    assert_eq!(
        next_event(&mut pending.events).await,
        Event::Complete {
            stop_reason: Some(StopReason::EndTurn)
        }
    );
    ended(&mut pending.events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn context_the_agent_cannot_follow_is_reported_by_index() {
    let h = harness(Fake::new(), ephemeral()).await;
    let link = &h.gateway.link;
    let request = GatewayCall::NewSession(NewSession {
        context: vec![
            ContentBlock::link("chat://w/c/t", "thread").into(),
            ContentBlock::link("file:///tmp/x", "file").into(),
        ],
    });
    let Ok(GatewayReply::NewSession(created)) = call(link, request).await else {
        panic!("expected session.new")
    };
    assert_eq!(created.unhandled.len(), 1);
    assert_eq!(created.unhandled[0].subject, Subject::Block { index: 1 });
    assert_eq!(
        created.unhandled[0].reason,
        UnhandledReason::UnsupportedScheme
    );
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_cancel_reaches_a_backend_still_starting_the_turn() {
    let fake = Fake::new();
    fake.block_prompt_until_cancel();
    let mut seen = fake.seen();
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    let session = new_session(link).await;

    let pending = prompt(link, &session, "slow start").await;
    seen.wait_for(|seen| matches!(seen, Seen::Prompt(..))).await;
    cancel(link, &session).await;
    seen.wait_for(|seen| matches!(seen, Seen::Cancel(_))).await;
    let fault = faulted(pending).await;
    assert_eq!(fault.kind, ErrorKind::Cancelled);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn cancelling_an_unknown_session_succeeds() {
    let h = harness(Fake::new(), ephemeral()).await;
    let link = &h.gateway.link;
    cancel(link, &"not-a-session".into()).await;
    cancel(link, &uuid::Uuid::new_v4().to_string().into()).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn closing_a_session_replies_releases_the_agent_and_forgets_the_record() {
    let dir = tempfile::tempdir().unwrap();
    let fake = Fake::new();
    let mut seen = fake.seen();
    let h = harness(fake, durable(dir.path())).await;
    let link = &h.gateway.link;
    initialize(link, GatewayCapabilities::default()).await;
    let session = new_session(link).await;
    let record = dir.path().join(format!("{session}.json"));
    assert!(record.exists());

    let close = GatewayCall::CloseSession(SessionRef {
        session_id: session.clone(),
    });
    assert!(matches!(
        call(link, close).await,
        Ok(GatewayReply::CloseSession)
    ));
    seen.wait_for(|seen| matches!(seen, Seen::Close(_))).await;
    assert!(!record.exists());

    let fault = faulted(prompt(link, &session, "are you there").await).await;
    assert_eq!(fault.kind, ErrorKind::UnknownSession);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_second_prompt_on_a_busy_session_is_faulted_and_the_first_carries_on() {
    let h = harness(Fake::new(), ephemeral()).await;
    let (release, released) = oneshot::channel::<()>();
    let released = std::sync::Arc::new(Mutex::new(Some(released)));
    h.fake.on_prompt(script(move |turn: Turn| {
        let released = released.clone();
        async move {
            let status = BackendEvent::Status("working".into());
            turn.handle.events.send(status).await.unwrap();
            if let Some(released) = released.lock().await.take() {
                let _ = released.await;
            }
            complete(&turn).await;
        }
    }));
    let link = &h.gateway.link;
    let session = new_session(link).await;

    let mut first = accepted(link, &session, "first").await;
    assert!(matches!(next_event(&mut first).await, Event::Status { .. }));
    let fault = faulted(prompt(link, &session, "second").await).await;
    assert_eq!(fault.kind, ErrorKind::SessionBusy);

    release.send(()).unwrap();
    assert!(matches!(
        next_event(&mut first).await,
        Event::Complete { .. }
    ));
    ended(&mut first).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_gateway_that_stops_reading_parks_the_agent_instead_of_buffering() {
    const N: usize = 200;
    let mut config = ephemeral();
    config.turn_events_capacity = 2;
    let gateway_config = rax_tokio::gateway::GatewayConfig {
        events_capacity: 4,
        window_bytes: 512,
        ack_threshold: 2,
        ..gateway_config()
    };
    let fake = Fake::new();
    let (done, mut finished) = oneshot::channel::<()>();
    let done = std::sync::Arc::new(Mutex::new(Some(done)));
    fake.on_prompt(script(move |turn: Turn| {
        let done = done.clone();
        async move {
            for i in 1..=N {
                let status = BackendEvent::Status(i.to_string());
                turn.handle.events.send(status).await.unwrap();
            }
            if let Some(done) = done.lock().await.take() {
                let _ = done.send(());
            }
            complete(&turn).await;
        }
    }));
    let h = start_with(fake, config, false, gateway_config, |node| {
        node.window_bytes = 512;
        node.ack_threshold = 2;
    })
    .await;
    let link = &h.gateway.link;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "flood").await;

    assert!(
        quiet(&mut finished).await,
        "the agent must park while the gateway is not reading"
    );
    for i in 1..=N {
        assert_eq!(
            next_event(&mut events).await,
            Event::Status {
                text: i.to_string()
            }
        );
    }
    within(finished).await.unwrap();
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.node.shut_down().await;
}
