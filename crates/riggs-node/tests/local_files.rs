#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::path::Path;

use rax::id::LocalFileId;
use rax::local_file::ReadLocalFile;
use rax::session::GatewayCapabilities;
use rax::tool::{CallTool, ToolDef, ToolKind, ToolOutcome};
use rax::{ErrorKind, NodeCall, NodeReply};
use rax_tokio::CallError;
use rax_tokio::gateway::{GatewayLink, LinkEvent, LinkEvents};
use riggs_node::{HostHandles, SessionKey, ToolUnreachable};
use serde_json::{Value, json};
use support::*;
use tokio::task::JoinHandle;

/// The node knows this tool only by its schema: `file` is marked, `comment` is not.
fn attach() -> ToolDef {
    ToolDef {
        name: "attach".into(),
        description: "Post a file to the conversation.".into(),
        input_schema: Some(json!({"type": "object", "properties": {
            "file": {"type": "string", "format": "local-file"},
            "comment": {"type": "string"}}})),
        kind: ToolKind::Edit,
    }
}

fn calling(
    host: &HostHandles,
    key: SessionKey,
    arguments: Value,
    root: &Path,
) -> JoinHandle<Result<ToolOutcome, ToolUnreachable>> {
    let (host, root) = (host.clone(), root.to_owned());
    tokio::spawn(async move {
        host.tools
            .call(&key, "slack", &attach(), arguments, &root)
            .await
    })
}

async fn next_call(events: &mut LinkEvents) -> (rax::id::RequestId, Value) {
    let LinkEvent::Request {
        id,
        call: NodeCall::CallTool(CallTool { arguments, .. }),
    } = next_link(events).await
    else {
        panic!("expected a tool call")
    };
    (id, arguments.unwrap())
}

fn identifier(arguments: &Value) -> LocalFileId {
    let id = arguments["file"].as_str().unwrap();
    assert!(id.starts_with("lf_"), "{id}");
    id.into()
}

fn read(id: &LocalFileId) -> ReadLocalFile {
    ReadLocalFile {
        id: id.clone(),
        max_bytes: None,
    }
}

async fn not_found(link: &GatewayLink, id: &LocalFileId) {
    let Err(CallError::Fault(fault)) = within(link.read_local_file(read(id))).await else {
        panic!("expected a fault")
    };
    assert_eq!(fault.kind, ErrorKind::NotFound);
}

async fn answer(link: &GatewayLink, id: rax::id::RequestId, content: &str) {
    let outcome = ToolOutcome {
        content: content.into(),
        is_error: false,
    };
    link.reply(id, NodeReply::CallTool(outcome)).await.unwrap();
}

#[tokio::test(flavor = "multi_thread")]
async fn a_marked_path_reaches_the_gateway_as_an_identifier_it_reads_once() {
    let work = tempfile::tempdir().unwrap();
    std::fs::write(work.path().join("report.pdf"), b"the report").unwrap();
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let arguments = json!({"file": "report.pdf", "comment": "report.pdf"});
    let call = calling(&host, key, arguments, work.path());
    let (id, arguments) = next_call(&mut h.gateway.events).await;
    assert_eq!(arguments["comment"], "report.pdf");
    let file_id = identifier(&arguments);

    let file = within(h.gateway.link.read_local_file(read(&file_id)))
        .await
        .unwrap();
    assert_eq!(file.bytes, b"the report");
    assert_eq!(file.name.as_deref(), Some("report.pdf"));
    assert_eq!(file.mimetype.as_deref(), Some("application/pdf"));
    not_found(&h.gateway.link, &file_id).await;

    answer(&h.gateway.link, id, "Posted report.pdf.").await;
    let outcome = within(call).await.unwrap().unwrap();
    assert_eq!(outcome.content, "Posted report.pdf.");
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_identifier_is_withdrawn_when_its_call_returns() {
    let work = tempfile::tempdir().unwrap();
    std::fs::write(work.path().join("notes.txt"), b"notes").unwrap();
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let call = calling(&host, key, json!({"file": "notes.txt"}), work.path());
    let (id, arguments) = next_call(&mut h.gateway.events).await;
    let file_id = identifier(&arguments);
    answer(&h.gateway.link, id, "Skipped.").await;
    within(call).await.unwrap().unwrap();

    not_found(&h.gateway.link, &file_id).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn only_the_sessions_gateway_can_read_what_a_call_offered() {
    let work = tempfile::tempdir().unwrap();
    std::fs::write(work.path().join("notes.txt"), b"notes").unwrap();
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    let other = second_gateway(&h.node, "work").await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    initialize(&other.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let call = calling(&host, key, json!({"file": "notes.txt"}), work.path());
    let (id, arguments) = next_call(&mut h.gateway.events).await;
    let file_id = identifier(&arguments);

    not_found(&other.gateway.link, &file_id).await;
    let file = within(h.gateway.link.read_local_file(read(&file_id)))
        .await
        .unwrap();
    assert_eq!(file.bytes, b"notes");
    answer(&h.gateway.link, id, "Posted.").await;
    within(call).await.unwrap().unwrap();
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_path_the_node_will_not_hand_over_never_reaches_the_gateway() {
    let root = tempfile::tempdir().unwrap();
    let work = root.path().join("work");
    std::fs::create_dir(&work).unwrap();
    std::fs::write(root.path().join("secret.txt"), b"secret").unwrap();
    std::fs::write(work.join("empty.txt"), b"").unwrap();
    std::os::unix::fs::symlink(root.path().join("secret.txt"), work.join("link.txt")).unwrap();
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let outside = root.path().join("secret.txt").display().to_string();
    let refusals = [
        ("../secret.txt", "is outside the working directory"),
        (outside.as_str(), "is outside the working directory"),
        ("link.txt", "is outside the working directory"),
        ("missing.txt", "cannot be opened"),
        (".", "is a directory"),
        ("empty.txt", "is empty"),
        (" ", "a file path is required"),
    ];
    for (path, expected) in refusals {
        let call = calling(&host, key, json!({"file": path}), &work);
        let Err(ToolUnreachable::File(refused)) = within(call).await.unwrap() else {
            panic!("{path:?} was not refused")
        };
        assert!(
            refused.to_string().contains(expected),
            "{path:?}: {refused}"
        );
    }
    assert!(quiet(next_link(&mut h.gateway.events)).await);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_file_cut_short_after_it_was_offered_fails_the_read() {
    let work = tempfile::tempdir().unwrap();
    let path = work.path().join("big.bin");
    std::fs::write(&path, vec![7u8; 64 << 10]).unwrap();
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;

    let call = calling(&host, key, json!({"file": "big.bin"}), work.path());
    let (id, arguments) = next_call(&mut h.gateway.events).await;
    let file_id = identifier(&arguments);
    std::fs::OpenOptions::new()
        .write(true)
        .open(&path)
        .unwrap()
        .set_len(0)
        .unwrap();

    let Err(CallError::Transfer(reason)) =
        within(h.gateway.link.read_local_file(read(&file_id))).await
    else {
        panic!("expected a failed transfer")
    };
    assert!(reason.contains("declared 65536 bytes"), "{reason}");
    answer(&h.gateway.link, id, "It did not arrive.").await;
    within(call).await.unwrap().unwrap();
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tool_with_no_marked_argument_keeps_its_arguments() {
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, GatewayCapabilities::default()).await;
    let session = new_session(&h.gateway.link).await;
    let key = SessionKey::parse(&session).unwrap();
    let host = fake.host().await;
    let plain = ToolDef {
        input_schema: Some(json!({"type": "object", "properties": {"file": {"type": "string"}}})),
        ..attach()
    };

    let call = tokio::spawn(async move {
        let arguments = json!({"file": "/etc/hosts"});
        host.tools
            .call(&key, "slack", &plain, arguments, Path::new("/nowhere"))
            .await
    });
    let (id, arguments) = next_call(&mut h.gateway.events).await;
    assert_eq!(arguments, json!({"file": "/etc/hosts"}));
    answer(&h.gateway.link, id, "ok").await;
    within(call).await.unwrap().unwrap();
    h.node.shut_down().await;
}
