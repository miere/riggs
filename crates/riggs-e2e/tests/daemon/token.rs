use std::time::Duration;

use rax_sim::{Handshake, Match, WireFrame};
use tokio::time::Instant;

use crate::harness::{Agent, OTHER_TOKEN, Rig, eventually};
use crate::support::{TOKEN, text};

const REJECTED: &str = "the gateway rejected this node's credential";

fn refused(handshakes: &[Handshake], status: u16) -> usize {
    handshakes
        .iter()
        .filter(|handshake| handshake.status == Some(status))
        .count()
}

async fn quiet_period() {
    tokio::time::sleep(Duration::from_secs(5)).await;
}

#[tokio::test(flavor = "multi_thread")]
async fn the_token_travels_in_a_bearer_header_and_resume_is_the_first_frame() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    let _riggs = rig.start();
    rig.attached().await;

    let handshake = rig.sim.handshakes().remove(0);
    assert_eq!(
        handshake.authorization.as_deref(),
        Some(format!("Bearer {TOKEN}").as_str())
    );
    assert_eq!(handshake.target, "/rax/v1/link");
    assert_eq!(handshake.path(), "/rax/v1/link");
    assert_eq!(
        handshake.first_frame,
        Some(WireFrame::Resume {
            last_seen: 0,
            epoch: 0
        })
    );
    let transcript = rig.sim.transcript();
    let before_resumed: Vec<_> = transcript
        .entries
        .iter()
        .filter(|entry| entry.connection == handshake.connection)
        .take_while(|entry| !matches!(entry.frame, WireFrame::Resumed { .. }))
        .collect();
    assert_eq!(before_resumed.len(), 1, "{before_resumed:?}");
}

#[tokio::test(flavor = "multi_thread")]
async fn a_rejected_token_stops_dialling_until_the_file_is_rotated_in_the_same_process() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(OTHER_TOKEN, 0o600);
    let mut riggs = rig.start();
    let pid = riggs.pid();
    riggs.logged(REJECTED).await;

    rig.rotate_token(OTHER_TOKEN, 0o600);
    quiet_period().await;
    let handshakes = rig.sim.handshakes();
    assert_eq!(handshakes.len(), 1, "{handshakes:?}");
    assert_eq!(refused(&handshakes, 401), 1);

    rig.rotate_token(TOKEN, 0o600);
    let rotated = Instant::now();
    rig.attached().await;
    assert!(
        rotated.elapsed() <= Duration::from_secs(10),
        "took {:?}",
        rotated.elapsed()
    );
    assert!(riggs.is_running());
    assert_eq!(riggs.pid(), pid);
    assert_eq!(
        riggs.stderr().matches(REJECTED).count(),
        1,
        "{}",
        riggs.stderr()
    );
    let latest = rig.sim.handshakes().pop().unwrap();
    assert_eq!(latest.authorization, Some(format!("Bearer {TOKEN}")));
}

#[tokio::test(flavor = "multi_thread")]
async fn a_rotated_token_others_can_read_is_not_sent_until_its_mode_is_fixed() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(OTHER_TOKEN, 0o600);
    let riggs = rig.start();
    riggs.logged(REJECTED).await;

    rig.rotate_token(TOKEN, 0o644);
    riggs.logged("could not read this node's credential").await;
    quiet_period().await;
    assert_eq!(rig.sim.handshakes().len(), 1);
    assert_eq!(
        riggs
            .stderr()
            .matches("could not read this node's credential")
            .count(),
        1,
        "{}",
        riggs.stderr()
    );

    std::fs::set_permissions(
        rig.token_path(),
        std::os::unix::fs::PermissionsExt::from_mode(0o600),
    )
    .unwrap();
    rig.attached().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unavailable_gateway_is_redialled_with_jittered_backoff() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    rig.sim.refuse_upgrades(503);
    let riggs = rig.start();
    let handshakes = rig
        .handshakes_until("three refused dials", |handshakes| {
            refused(handshakes, 503) >= 3
        })
        .await;
    for pair in handshakes.windows(2) {
        let gap = pair[1].at - pair[0].at;
        assert!(gap >= Duration::from_millis(500), "redialled after {gap:?}");
        assert!(gap <= Duration::from_secs(31), "waited {gap:?}");
    }
    rig.sim.accept_upgrades();
    rig.attached().await;
    assert!(!riggs.stderr().contains(REJECTED));
}

#[tokio::test(flavor = "multi_thread")]
async fn an_unreachable_gateway_backs_off_and_warns() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    rig.sim.hang_up_upgrades();
    let started = Instant::now();
    let riggs = rig.start();
    riggs.logged("rax dial failed").await;
    let handshakes = rig
        .handshakes_until("three unanswered dials", |handshakes| handshakes.len() >= 3)
        .await;
    assert!(
        handshakes
            .iter()
            .all(|handshake| handshake.status.is_none())
    );
    let elapsed = started.elapsed().as_secs_f64();
    assert!(
        (handshakes.len() as f64) <= 2.0 + elapsed,
        "{} dials in {elapsed}s",
        handshakes.len()
    );
    rig.sim.accept_upgrades();
    rig.attached().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_status_dialling_cannot_fix_ends_the_process_with_a_failure() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    rig.sim.refuse_upgrades(403);
    let mut riggs = rig.start();
    let status = riggs.exited().await;
    assert_eq!(status.code(), Some(1), "{}", riggs.stderr());
    assert!(riggs.stderr().contains("HTTP 403"), "{}", riggs.stderr());
    assert_eq!(rig.sim.handshakes().len(), 1);
}

#[tokio::test(flavor = "multi_thread")]
async fn revocation_ends_the_link_denies_the_held_call_and_stops_dialling() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("tool-gate"), "");
    rig.write_token(TOKEN, 0o600);
    let riggs = rig.start();
    let node = rig.attached().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("touch it")).await.unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();

    rig.sim.revoke(TOKEN).await;
    riggs.logged(REJECTED).await;
    eventually("Claude Code to drop the held call", || {
        !rig.fake_events("interrupted").is_empty() || !rig.fake_events("hook_denied").is_empty()
    })
    .await;
    quiet_period().await;
    let handshakes = rig.sim.handshakes();
    assert_eq!(refused(&handshakes, 401), 1, "{handshakes:?}");
    assert!(rig.fake_events("hook_allowed").is_empty());
    assert!(!rig.work().join("ran.txt").exists());
    assert!(
        turn.until_end().await.is_err(),
        "the revoked link still ended the turn"
    );
}
