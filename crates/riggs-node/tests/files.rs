#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use rax::content::ContentBlock;
use rax::id::SessionId;
use rax::open::{Subject, UnhandledReason};
use rax::session::{GatewayCapabilities, Prompt, SessionRef};
use rax::{Error, ErrorKind, GatewayCall, GatewayReply, NodeCall, Open};
use rax_tokio::gateway::{LinkEvent, PendingCall};
use support::*;
use url::Url;

const PDF: &[u8] = b"%PDF-1.7 quarterly figures";

fn serving_files() -> GatewayCapabilities {
    GatewayCapabilities {
        readable_schemes: vec!["gateway".into()],
        ..all_caps()
    }
}

async fn with_files(caps: GatewayCapabilities) -> (Harness, tempfile::TempDir, SessionId) {
    let files = tempfile::tempdir().unwrap();
    let mut config = ephemeral();
    config.files_dir = Some(files.path().to_path_buf());
    let harness = harness(Fake::new(), config).await;
    initialize(&harness.gateway.link, caps).await;
    let session = new_session(&harness.gateway.link).await;
    (harness, files, session)
}

async fn prompt_with_link(harness: &Harness, session: &SessionId, uri: &str) -> PendingCall {
    let request = GatewayCall::Prompt(Prompt {
        session_id: session.clone(),
        content: vec![
            ContentBlock::text("summarise this").into(),
            ContentBlock::link(uri, "Q3 report.pdf").into(),
        ],
    });
    within(harness.gateway.link.call(request)).await.unwrap()
}

async fn next_read(harness: &mut Harness) -> (rax::id::RequestId, rax::resource::ReadResource) {
    loop {
        match next_link(&mut harness.gateway.events).await {
            LinkEvent::Request {
                id,
                call: NodeCall::ReadResource(read),
            } => return (id, read),
            LinkEvent::Request { .. } => {}
            other => panic!("expected resource.read, got {other:?}"),
        }
    }
}

async fn accepted_unhandled(pending: PendingCall) -> Vec<rax::Unhandled> {
    match within(pending.reply).await {
        Ok(GatewayReply::Prompt(accepted)) => accepted.unhandled,
        other => panic!("expected an accepted prompt, got {other:?}"),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_linked_file_reaches_the_agent_as_a_local_copy_and_leaves_with_its_session() {
    let (mut harness, files, session) = with_files(serving_files()).await;
    let pending = prompt_with_link(&harness, &session, "gateway://files/F1").await;

    let (id, read) = next_read(&mut harness).await;
    assert_eq!(read.uri, "gateway://files/F1");
    harness
        .gateway
        .link
        .serve_resource(id, &read, PDF, Some("application/pdf".into()))
        .await
        .unwrap();
    assert!(accepted_unhandled(pending).await.is_empty());

    let content = harness.fake.last_prompt();
    let Open::Known(ContentBlock::ResourceLink {
        uri,
        name,
        mime_type,
        size,
        ..
    }) = &content[1]
    else {
        panic!("expected a link, got {content:?}")
    };
    assert_eq!(name, "Q3 report.pdf");
    assert_eq!(mime_type.as_deref(), Some("application/pdf"));
    assert_eq!(*size, Some(PDF.len() as u64));
    let path = Url::parse(uri).unwrap().to_file_path().unwrap();
    assert!(path.starts_with(files.path()), "{}", path.display());
    assert_eq!(std::fs::read(&path).unwrap(), PDF);

    let close = GatewayCall::CloseSession(SessionRef {
        session_id: session.clone(),
    });
    call(&harness.gateway.link, close).await.unwrap();
    assert!(!path.exists(), "the file outlived its session");
    harness.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_refused_file_becomes_a_note_for_the_agent_and_is_reported() {
    let (mut harness, _files, session) = with_files(serving_files()).await;
    let pending = prompt_with_link(&harness, &session, "gateway://files/F2").await;

    let (id, _) = next_read(&mut harness).await;
    let refusal = Error::new(ErrorKind::Forbidden, "not yours");
    harness.gateway.link.fault(id, refusal).await.unwrap();

    let unhandled = accepted_unhandled(pending).await;
    assert_eq!(unhandled.len(), 1);
    assert_eq!(unhandled[0].subject, Subject::Block { index: 1 });
    assert_eq!(unhandled[0].reason, UnhandledReason::Forbidden);
    let content = harness.fake.last_prompt();
    let Open::Known(ContentBlock::Text { text }) = &content[1] else {
        panic!("expected a note, got {content:?}")
    };
    assert!(text.contains("could not be fetched"), "{text}");
    harness.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn links_the_gateway_does_not_serve_pass_through_untouched() {
    let (mut harness, _files, session) = with_files(all_caps()).await;
    let pending = prompt_with_link(&harness, &session, "gateway://files/F3").await;

    assert!(accepted_unhandled(pending).await.is_empty());
    assert_eq!(
        harness.fake.last_prompt()[1],
        ContentBlock::link("gateway://files/F3", "Q3 report.pdf").into()
    );
    assert!(quiet(harness.gateway.events.recv()).await);
    harness.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_node_offers_to_follow_the_schemes_its_gateway_serves() {
    let files = tempfile::tempdir().unwrap();
    let mut config = ephemeral();
    config.files_dir = Some(files.path().to_path_buf());
    let harness = harness(Fake::new(), config).await;
    let initialized = initialize(&harness.gateway.link, serving_files()).await;
    assert_eq!(
        initialized.capabilities.resource_schemes,
        ["chat", "gateway"]
    );
    harness.node.shut_down().await;
}
