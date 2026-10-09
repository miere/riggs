#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};

use rax::attachment::MAX_ATTACHMENT_BYTES;
use rax::content::ContentBlock;
use rax::event::BackgroundEvent;
use rax::session::GatewayCapabilities;
use rax::{Event, Open};
use rax_tokio::gateway::LinkEvent;
use riggs_node::{AttachmentMeta, AttachmentSource, BackendEvent, SessionKey};
use support::*;
use tokio::io::{AsyncRead, AsyncReadExt, ReadBuf};

const REPORT_BYTES: usize = 300 << 10;

fn report() -> Vec<u8> {
    (0..REPORT_BYTES).map(|i| (i % 251) as u8).collect()
}

fn source(size: u64, reader: impl AsyncRead + Send + Unpin + 'static) -> AttachmentSource {
    AttachmentSource {
        meta: AttachmentMeta {
            filename: Some("report.bin".into()),
            mimetype: Some("application/octet-stream".into()),
            ..Default::default()
        },
        size,
        reader: Box::new(reader),
    }
}

struct Broken;

impl AsyncRead for Broken {
    fn poll_read(
        self: Pin<&mut Self>,
        _: &mut Context<'_>,
        _: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Poll::Ready(Err(io::Error::other("disk went away")))
    }
}

fn attaching(fake: &Fake) {
    fake.on_prompt(script(|turn: Turn| async move {
        let attachment = match turn.text.as_str() {
            "too large" => source(MAX_ATTACHMENT_BYTES + 1, tokio::io::empty()),
            "broken" => source(1024, (&b"the first bytes"[..]).chain(Broken)),
            _ => source(REPORT_BYTES as u64, io::Cursor::new(report())),
        };
        let events = &turn.handle.events;
        events
            .send(BackendEvent::Attachment(attachment))
            .await
            .unwrap();
        let after = BackendEvent::Message(ContentBlock::text("carried on").into());
        events.send(after).await.unwrap();
        complete(&turn).await;
    }));
}

async fn carried_on(events: &mut rax_tokio::gateway::StreamEvents) {
    assert_eq!(
        next_event(events).await,
        Event::Message {
            content: ContentBlock::text("carried on").into()
        }
    );
    assert!(matches!(next_event(events).await, Event::Complete { .. }));
    ended(events).await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_attachment_arrives_whole_before_the_event_that_names_it() {
    let fake = Fake::new();
    attaching(&fake);
    let mut h = harness(fake, ephemeral()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "report").await;

    let LinkEvent::Attachment { transfer_id, bytes } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the transfer")
    };
    assert!(bytes == report(), "the attachment was altered in transit");
    let Event::Attachment { attachment } = next_event(&mut events).await else {
        panic!("expected the attachment event")
    };
    assert_eq!(attachment.transfer_id, transfer_id);
    assert_eq!(attachment.size, REPORT_BYTES as u64);
    assert_eq!(attachment.filename.as_deref(), Some("report.bin"));
    carried_on(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_attachment_over_the_ceiling_becomes_an_error_and_the_turn_continues() {
    let fake = Fake::new();
    attaching(&fake);
    let h = harness(fake, ephemeral()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "too large").await;

    let Event::Error { error } = next_event(&mut events).await else {
        panic!("expected an error event")
    };
    assert!(error.message.contains("report.bin"), "{}", error.message);
    carried_on(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_source_that_fails_mid_read_fails_the_transfer_and_the_turn_continues() {
    let fake = Fake::new();
    attaching(&fake);
    let mut h = harness(fake, ephemeral()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "broken").await;

    let LinkEvent::TransferFailed { reason, .. } = next_link(&mut h.gateway.events).await else {
        panic!("expected a failed transfer")
    };
    assert!(reason.contains("disk went away"), "{reason}");
    let Event::Error { error } = next_event(&mut events).await else {
        panic!("expected an error event")
    };
    assert!(
        error.message.contains("disk went away"),
        "{}",
        error.message
    );
    carried_on(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn output_with_no_turn_open_goes_out_as_background_events() {
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let content = ContentBlock::text("sub-agent finished").into();
    host.background
        .send(&key, BackgroundEvent::Message { content })
        .await
        .unwrap();
    let LinkEvent::Background { session_id, event } = next_link(&mut h.gateway.events).await else {
        panic!("expected a background event")
    };
    assert_eq!(session_id, session);
    assert_eq!(
        event,
        Open::Known(BackgroundEvent::Message {
            content: ContentBlock::text("sub-agent finished").into()
        })
    );

    host.background
        .attach(&key, source(REPORT_BYTES as u64, io::Cursor::new(report())))
        .await
        .unwrap();
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Attachment { .. }
    ));
    assert!(matches!(
        next_link(&mut h.gateway.events).await,
        LinkEvent::Background {
            event: Open::Known(BackgroundEvent::Attachment { .. }),
            ..
        }
    ));
    h.node.shut_down().await;
}
