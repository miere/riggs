#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::collections::BTreeMap;
use std::time::Duration;

use rax::content::ContentBlock;
use rax::event::{BackgroundEvent, StopReason};
use rax::interaction::{DisplayAnswer, DisplayOutcome, PlanChoice};
use rax::session::{SessionDurability, ToolGate};
use rax::tool::{DeniedBy, ToolCallStatus, ToolKind};
use rax::{Decision, ErrorKind, Event, Open};
use rax_sim::{LinkChange, Match, Transfer, VerdictPolicy};
use serde_json::{Value, json};
use support::*;

fn answer(id: rax::id::PromptId, outcome: DisplayOutcome) -> DisplayAnswer {
    DisplayAnswer {
        id,
        outcome,
        answers: BTreeMap::new(),
        choice: None,
        user_id: None,
        note: None,
        code: None,
    }
}

fn is_complete(event: &Open<Event>) -> bool {
    matches!(event, Open::Known(Event::Complete { .. }))
}

#[tokio::test(flavor = "multi_thread")]
async fn initialize_declares_every_call_durable_sessions_and_images() {
    let mut world = World::new("basic").await;
    let (_node, initialized) = world.start_node_initialized().await;
    let caps = initialized.capabilities;
    assert_eq!(caps.tool_gate, ToolGate::EveryCall);
    assert_eq!(caps.sessions, SessionDurability::Durable);
    assert_eq!(caps.interruptible, Some(true));
    assert!(caps.prompt.image);
    assert!(!caps.prompt.audio && !caps.prompt.embedded_resource);
    world.stop_node().await;
}

/// A boxed agent must still be able to work: the policy is only useful if the CLI runs under it.
#[cfg(target_os = "macos")]
#[tokio::test(flavor = "multi_thread")]
async fn a_boxed_agent_still_answers() {
    // A machine already inside someone else's box cannot nest another, and says so here.
    let probe = std::process::Command::new("/usr/bin/sandbox-exec")
        .args(["-p", "(version 1)(allow default)", "/usr/bin/true"])
        .output()
        .unwrap();
    if !probe.status.success() {
        return;
    }
    let mut world = World::with("basic", |config| {
        // The fake CLI keeps its state beside the binary rather than in the workspace.
        let state = config
            .env
            .get("FAKE_CLAUDE_STATE")
            .cloned()
            .unwrap_or_default();
        config.sandbox = riggs_claude_code::SandboxConfig {
            mode: riggs_claude_code::SandboxMode::Seatbelt,
            write: vec![
                std::path::PathBuf::from(state),
                std::path::PathBuf::from(env!("CARGO_TARGET_TMPDIR")),
            ],
            ..Default::default()
        };
    })
    .await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("say pong")).await.unwrap();
    turn.expect(Match::when("an answer", |event| {
        matches!(event, Event::Message { .. })
    }))
    .await
    .unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_prompt_is_accepted_before_its_events_and_completes() {
    let mut world = World::new("basic").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("say pong"))
        .await
        .unwrap();
    turn.expect(Match::message_contains("pong")).await.unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert_eq!(
        world.sim.report().turns[0].reply_before_first_event,
        Some(true)
    );

    assert_eq!(
        world.argv(0),
        [
            "-p",
            "--input-format",
            "stream-json",
            "--output-format",
            "stream-json",
            "--verbose",
            "--permission-mode",
            "dontAsk",
            "--permission-prompt-tool",
            "stdio",
            "--disallowedTools",
            "AskUserQuestion,EnterPlanMode,ExitPlanMode",
            "--session-id",
            session.0.as_str(),
        ]
    );
    assert_eq!(
        world.starts()[0]["cwd"],
        json!(world.work().display().to_string())
    );
    let initialize = world
        .stdin()
        .into_iter()
        .find(|frame| frame["request"]["subtype"] == "initialize")
        .unwrap();
    assert_eq!(
        initialize["request"]["hooks"],
        json!({"PreToolUse": [{"hookCallbackIds": ["gate"], "timeout": 3600}]})
    );
    assert_eq!(initialize["request"]["sdkMcpServers"], json!(["riggs"]));
    assert_eq!(
        world.events("mcp_tools")[0]["tools"],
        json!(["auth", "ask", "present_plan", "attach"])
    );
    assert_eq!(
        world.user_frames()[0]["message"]["content"],
        json!([{"type": "text", "text": "say pong"}])
    );
    world.assert_no_violations();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_image_and_a_resource_link_reach_claude_code_as_blocks_it_reads() {
    let mut world = World::new("echo-prompt").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let content = vec![
        ContentBlock::text("What colour is this?").into(),
        ContentBlock::Image {
            data: "iVBORw0KGgo=".into(),
            mime_type: "image/png".into(),
            uri: None,
        }
        .into(),
        ContentBlock::link("chat://thread/42", "thread").into(),
    ];
    let mut turn = node.prompt(session, content).await.unwrap();
    assert!(turn.accepted().unhandled.is_empty());
    turn.expect(Match::message_contains(
        "[image image/png 12 base64 chars]\n[thread: chat://thread/42]",
    ))
    .await
    .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert_eq!(
        world.user_frames()[0]["message"]["content"][1],
        json!({"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}})
    );
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_cancel_reaches_a_turn_whose_process_is_still_starting() {
    let mut world = World::new("ready-gate").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let pending = node
        .start_prompt(session.clone(), text("hello"))
        .await
        .unwrap();
    world
        .eventually("fake-claude to start", |world| !world.starts().is_empty())
        .await;
    node.cancel(session.clone()).await.unwrap();
    std::fs::write(world.state().join("ready"), "").unwrap();

    assert_eq!(fault(pending.accepted().await).kind, ErrorKind::Cancelled);
    assert!(
        world.user_frames().is_empty(),
        "the prompt reached Claude Code"
    );

    let mut turn = node.prompt(session, text("again")).await.unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 1);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn closing_a_session_kills_its_process_and_forgets_it() {
    let mut world = World::new("basic").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session.clone(), text("hi")).await.unwrap();
    turn.until_end().await.unwrap();
    let pid = world.pid(0);
    let record = world.store().join(format!("{session}.json"));
    assert!(alive(pid) && record.exists());

    node.close_session(session.clone()).await.unwrap();
    world
        .eventually("fake-claude to be gone", |_| !alive(pid))
        .await;
    assert!(!record.exists());
    let refused = fault(node.prompt(session, text("still there?")).await);
    assert_eq!(refused.kind, ErrorKind::UnknownSession);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_tool_call_does_not_run_until_the_gateway_allows_it() {
    let mut world = World::new("tool-gate").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("touch it")).await.unwrap();

    let Event::ToolCall { tool_call } = turn.expect(Match::tool_call("Bash")).await.unwrap() else {
        panic!("expected a tool call")
    };
    assert_eq!(tool_call.id.0, "toolu_01SBH4LLZqjynw3M6EsJcdnL");
    assert_eq!(tool_call.kind, ToolKind::Execute);
    assert_eq!(tool_call.title.as_deref(), Some("touch ran.txt"));
    assert_eq!(
        tool_call.input.as_ref().unwrap()["command"],
        "touch ran.txt"
    );
    let ran = world.work().join("ran.txt");
    assert!(!ran.exists(), "the tool ran before its verdict");
    assert!(!world.has_event("hook_allowed"));

    turn.verdict(tool_call.id, Decision::Allow).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::InProgress))
        .await
        .unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Completed))
        .await
        .unwrap();
    turn.expect(Match::message_contains("done")).await.unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert!(ran.exists());
    world.assert_no_violations();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_denied_tool_never_runs_and_the_model_reads_the_reason() {
    let mut world = World::new("two-denials").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("try both")).await.unwrap();

    let reason = "Writes are not allowed on this node. Do not retry it.";
    let Event::ToolCall { tool_call } = turn.expect(Match::tool_call("Bash")).await.unwrap() else {
        panic!("expected a tool call")
    };
    let deny = Decision::Deny {
        by: DeniedBy::Policy,
        reason: Some(reason.into()),
    };
    turn.verdict(tool_call.id, deny).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::message_contains(format!(
        "First I was told: {reason}"
    )))
    .await
    .unwrap();

    let Event::ToolCall { tool_call } = turn.expect(Match::tool_call("Write")).await.unwrap()
    else {
        panic!("expected a tool call")
    };
    assert_eq!(tool_call.kind, ToolKind::Edit);
    let deny = Decision::Deny {
        by: DeniedBy::User,
        reason: None,
    };
    turn.verdict(tool_call.id, deny).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::message_contains(
        "Then I was told: The person denied this tool call. Do not retry it — ask them how they would like to proceed.",
    ))
    .await
    .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();

    let failed = Match::tool_call_update(ToolCallStatus::Failed);
    assert!(
        !turn
            .seen()
            .iter()
            .any(|event| event.known().is_some_and(|event| failed.matches(event))),
        "a denied call was also reported failed"
    );
    assert!(!world.work().join("ran.txt").exists());
    assert!(!world.work().join("c.txt").exists());
    assert_eq!(world.events("hook_denied")[0]["reason"], reason);
    world.assert_no_violations();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn the_node_denies_a_held_call_before_claude_codes_hook_timeout() {
    let mut world = World::with("tool-gate", |config| {
        config.hook_timeout = Duration::from_secs(4);
        config.hook_margin = Duration::from_secs(2);
    })
    .await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("touch it")).await.unwrap();

    turn.expect(Match::tool_call("Bash")).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::message_contains(
        "I was told: Skipped: nobody answered the approval request in time.",
    ))
    .await
    .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert!(!world.has_event("hook_timed_out"));
    assert!(!world.work().join("ran.txt").exists());
    let initialize = world
        .stdin()
        .into_iter()
        .find(|frame| frame["request"]["subtype"] == "initialize")
        .unwrap();
    assert_eq!(
        initialize["request"]["hooks"]["PreToolUse"][0]["timeout"],
        4
    );
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_held_call_survives_a_dropped_socket_and_takes_its_verdict_after_resume() {
    let mut world = World::new("tool-gate").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("touch it")).await.unwrap();
    let Event::ToolCall { tool_call } = turn.expect(Match::tool_call("Bash")).await.unwrap() else {
        panic!("expected a tool call")
    };

    world.sim.sever();
    loop {
        match node.next_link_change().await.unwrap() {
            LinkChange::Resumed => break,
            LinkChange::Disconnected(_) => {}
            other => panic!("expected the link to resume, got {other:?}"),
        }
    }
    assert!(!world.work().join("ran.txt").exists());
    turn.verdict(tool_call.id, Decision::Allow).await.unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();

    assert!(world.work().join("ran.txt").exists());
    let tool_calls = turn
        .seen()
        .iter()
        .filter(|event| matches!(event, Open::Known(Event::ToolCall { .. })))
        .count();
    assert_eq!(tool_calls, 1);
    assert_eq!(
        turn.seen()
            .iter()
            .filter(|event| is_complete(event))
            .count(),
        1
    );
    assert_eq!(messages(turn.seen()), ["done"]);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_held_call_is_denied_when_the_gateway_forgets_the_link() {
    let mut world = World::new("tool-gate").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("touch it"))
        .await
        .unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();

    world.sim.restart().await.unwrap();
    let fresh = world.sim.next_node().await.unwrap();
    fresh.initialize(all_caps()).await.unwrap();
    world
        .eventually("Claude Code to drop the held call", |world| {
            world.has_event("interrupted") || world.has_event("hook_denied")
        })
        .await;
    assert!(turn.until_end().await.is_err(), "the old stream was ended");
    assert!(!world.has_event("hook_allowed"));
    assert!(!world.work().join("ran.txt").exists());

    let mut again = fresh.prompt(session, text("still there?")).await.unwrap();
    again.expect(Match::message_contains("pong")).await.unwrap();
    again.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 1);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_cancelled_turn_completes_cancelled_and_the_next_prompt_works() {
    let mut world = World::new("tool-gate").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("touch it"))
        .await
        .unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();

    node.cancel(session.clone()).await.unwrap();
    turn.expect(Match::tool_call_update(ToolCallStatus::Denied))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::Cancelled))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world
        .eventually("the fake to log the interrupt", |world| {
            world.has_event("interrupted")
        })
        .await;
    assert!(!world.has_event("hook_allowed"));
    assert!(!world.work().join("ran.txt").exists());

    let mut again = node.prompt(session, text("ping")).await.unwrap();
    again.expect(Match::message_contains("pong")).await.unwrap();
    again
        .expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    again.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 1);
    world.assert_no_violations();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_question_is_answered_through_the_ask_tool() {
    let mut world = World::new("ask").await;
    let node = world.start_node().await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("ask me")).await.unwrap();

    let Event::Question { question } = turn.expect(Match::question()).await.unwrap() else {
        panic!("expected a question")
    };
    let [asked] = question.questions.as_slice() else {
        panic!("expected one question: {question:?}")
    };
    assert_eq!(asked.key, "q0");
    assert_eq!(asked.header.as_deref(), Some("Colour"));
    assert_eq!(asked.question, "Favourite colour?");
    assert_eq!(asked.options[1].label, "Blue");
    assert_eq!(
        asked.options[1].description.as_deref(),
        Some("Like the sky")
    );

    let mut answered = answer(question.id, DisplayOutcome::Answered);
    answered.answers.insert("q0".into(), vec!["Blue".into()]);
    answered.user_id = Some("U123".into());
    turn.answer(answered).await.unwrap();
    let Event::Message {
        content: Open::Known(ContentBlock::Text { text: said }),
    } = turn
        .expect(Match::message_contains("The person said:"))
        .await
        .unwrap()
    else {
        panic!("expected a message")
    };
    let result: Value = serde_json::from_str(said.trim_start_matches("The person said: ")).unwrap();
    assert_eq!(
        result,
        json!({"answered": true, "answers": [{"question": "Favourite colour?", "choices": ["Blue"]}], "user_id": "U123"})
    );
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_question_pending_when_the_turn_is_cancelled_is_dismissed() {
    let mut world = World::new("ask").await;
    let node = world.start_node().await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session.clone(), text("ask me")).await.unwrap();
    turn.expect(Match::question()).await.unwrap();

    node.cancel(session).await.unwrap();
    turn.expect(Match::complete(StopReason::Cancelled))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world
        .eventually("Claude Code to hear the question was dismissed", |world| {
            world.stdin_contains("The questions were dismissed before the user answered.")
        })
        .await;
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_question_pending_when_the_gateway_forgets_the_link_is_dismissed() {
    let mut world = World::new("ask").await;
    let node = world.start_node().await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("ask me")).await.unwrap();
    turn.expect(Match::question()).await.unwrap();

    world.sim.restart().await.unwrap();
    let fresh = world.sim.next_node().await.unwrap();
    fresh.initialize(all_caps()).await.unwrap();
    world
        .eventually("Claude Code to hear the question was dismissed", |world| {
            world.stdin_contains("The questions were dismissed before the user answered.")
        })
        .await;
    assert!(turn.until_end().await.is_err(), "the old stream was ended");
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_plan_is_approved_through_the_present_plan_tool() {
    let mut world = World::new("plan").await;
    let node = world.start_node().await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("plan it")).await.unwrap();

    let Event::Plan { plan } = turn.expect(Match::plan()).await.unwrap() else {
        panic!("expected a plan")
    };
    assert_eq!(plan.title.as_deref(), Some("Fix the flaky test"));
    assert_eq!(plan.plan, "1. Read the logs\n2. Fix the bug");
    let mut approved = answer(plan.id, DisplayOutcome::Answered);
    approved.choice = Some(PlanChoice::Proceed);
    turn.answer(approved).await.unwrap();
    turn.expect(Match::message_contains(
        "Approved — proceed with the plan as presented.",
    ))
    .await
    .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_background_subagent_reports_after_the_turn_and_its_tool_waits_for_a_verdict() {
    let mut world = World::new("background-subagent").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("work in the background"))
        .await
        .unwrap();

    let Event::ToolCall { tool_call } = turn.expect(Match::tool_call("Agent")).await.unwrap()
    else {
        panic!("expected a tool call")
    };
    assert_eq!(tool_call.kind, ToolKind::Think);
    let agent = tool_call.id.clone();
    turn.verdict(tool_call.id, Decision::Allow).await.unwrap();
    turn.expect(Match::message_contains("launched"))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    // Its launch receipt is not its result: the sub-agent is still working when the turn ends.
    assert!(!turn.seen().iter().any(|event| matches!(
        event,
        Open::Known(Event::ToolCallUpdate { tool_call_update })
            if tool_call_update.id == agent && tool_call_update.status != ToolCallStatus::InProgress
    )));

    let (session_id, event) = node.next_background().await.unwrap();
    assert_eq!(session_id, session);
    let Open::Known(BackgroundEvent::ToolCall { tool_call }) = event else {
        panic!("expected a background tool call, got {event:?}")
    };
    assert_eq!(tool_call.id.0, "toolu_01CNnYcU1S8btC7xyxETGamL");
    let sub = world.work().join("sub.txt");
    assert!(!sub.exists(), "the subagent's tool ran before its verdict");
    node.verdict(tool_call.id, Decision::Allow).await.unwrap();

    let mut background = Vec::new();
    loop {
        let (_, event) = node.next_background().await.unwrap();
        let done = matches!(event, Open::Known(BackgroundEvent::Complete { .. }));
        background.push(event);
        if done {
            break;
        }
    }
    let updates: Vec<(&str, ToolCallStatus)> = background
        .iter()
        .filter_map(|event| match event {
            Open::Known(BackgroundEvent::ToolCallUpdate { tool_call_update }) => {
                Some((tool_call_update.id.0.as_str(), tool_call_update.status))
            }
            _ => None,
        })
        .collect();
    assert_eq!(
        updates,
        [
            ("toolu_01CNnYcU1S8btC7xyxETGamL", ToolCallStatus::InProgress),
            ("toolu_01CNnYcU1S8btC7xyxETGamL", ToolCallStatus::Completed),
            (agent.0.as_str(), ToolCallStatus::Completed),
        ]
    );
    let texts: Vec<String> = background
        .iter()
        .filter_map(|event| match event {
            Open::Known(BackgroundEvent::Message {
                content: Open::Known(ContentBlock::Text { text }),
            }) => Some(text.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(
        texts,
        ["Background agent completed. The subagent replied \"sub done\"."]
    );
    assert!(matches!(
        background.last(),
        Some(Open::Known(BackgroundEvent::Complete {
            stop_reason: Some(StopReason::EndTurn)
        }))
    ));
    assert!(sub.exists());
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_file_is_attached_to_the_reply_and_one_outside_the_workdir_is_refused() {
    let mut world = World::new("attach").await;
    let node = world.start_node().await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    let bytes: Vec<u8> = (0..300u32 << 10).map(|i| (i % 251) as u8).collect();
    std::fs::write(world.work().join("report.bin"), &bytes).unwrap();
    std::fs::write(world.root().join("outside.txt"), "secret").unwrap();
    let session = node.new_session(vec![]).await.unwrap().session_id;

    let mut turn = node
        .prompt(session.clone(), text("send the report"))
        .await
        .unwrap();
    let Event::Attachment { attachment } = turn.expect(Match::attachment()).await.unwrap() else {
        panic!("expected an attachment")
    };
    assert_eq!(attachment.size, bytes.len() as u64);
    assert_eq!(attachment.filename.as_deref(), Some("report.bin"));
    assert_eq!(attachment.title.as_deref(), Some("Report"));
    let received = node.transfer(&attachment.transfer_id).await.unwrap();
    assert!(received == Transfer::Complete(bytes), "{received:?}");
    turn.expect(Match::message_contains(
        "Attached report.bin (307200 bytes) to your reply.",
    ))
    .await
    .unwrap();
    turn.until_end().await.unwrap();

    let mut refused = node.prompt(session, text("send the secret")).await.unwrap();
    refused
        .expect(Match::message_contains("is outside the working directory"))
        .await
        .unwrap();
    refused.until_end().await.unwrap();
    assert!(
        !refused
            .seen()
            .iter()
            .any(|event| matches!(event, Open::Known(Event::Attachment { .. })))
    );
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_attachment_that_fails_mid_read_is_an_error_and_the_turn_completes() {
    let mut world = World::new("attach-truncated").await;
    let node = world.start_node().await;
    std::fs::write(world.work().join("big.bin"), vec![7u8; 40 << 20]).unwrap();
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("send it")).await.unwrap();
    let Event::ToolCall { tool_call } = turn
        .expect(Match::tool_call("mcp__riggs__attach"))
        .await
        .unwrap()
    else {
        panic!("expected a tool call")
    };

    world.sim.pause_reading();
    turn.verdict(tool_call.id, Decision::Allow).await.unwrap();
    world
        .eventually("fake-claude to truncate the file", |world| {
            world.has_event("truncated")
        })
        .await;
    world.sim.resume_reading();

    let (_, transfer) = node.next_transfer().await.unwrap();
    assert!(matches!(transfer, Transfer::Failed(_)), "{transfer:?}");
    turn.expect(Match::error(ErrorKind::Unknown)).await.unwrap();
    turn.expect(Match::message_contains("Attached big.bin"))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_api_401_fails_the_turn_as_a_credential_error_and_restarts_the_process() {
    let mut world = World::new("api-401").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session.clone(), text("hi")).await.unwrap();
    let Event::Error { error } = turn
        .expect(Match::error(ErrorKind::Credential))
        .await
        .unwrap()
    else {
        panic!("expected an error")
    };
    assert!(error.message.contains("Invalid API key"), "{error}");
    turn.until_end().await.unwrap();
    let retried = turn
        .seen()
        .iter()
        .any(|event| matches!(event, Open::Known(Event::Status { text }) if text.contains("401")));
    assert!(retried, "no retry status in {:?}", turn.seen());
    assert!(messages(turn.seen()).is_empty());

    let mut again = node
        .prompt(session.clone(), text("hi again"))
        .await
        .unwrap();
    again.expect(Match::message_contains("pong")).await.unwrap();
    again.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 2);
    assert_eq!(
        world.argv(1)[world.argv(1).len() - 2..],
        ["--resume", session.0.as_str()]
    );
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_api_429_is_a_retryable_provider_error() {
    let mut world = World::new("api-429").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session.clone(), text("hi")).await.unwrap();
    let placeholder = ErrorKind::Provider {
        provider: rax::error::ProviderFailure {
            kind: String::new(),
            provider: None,
            status_code: None,
            message: None,
            retryable: false,
        },
    };
    let Event::Error { error } = turn.expect(Match::error(placeholder)).await.unwrap() else {
        panic!("expected an error")
    };
    let ErrorKind::Provider { provider } = error.kind else {
        panic!("expected a provider error")
    };
    assert_eq!(provider.kind, "rate_limit");
    assert_eq!(provider.status_code, Some(429));
    assert_eq!(provider.provider.as_deref(), Some("anthropic"));
    assert!(provider.retryable);
    turn.until_end().await.unwrap();

    let mut again = node.prompt(session, text("hi again")).await.unwrap();
    again.expect(Match::message_contains("pong")).await.unwrap();
    again.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 1);
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn an_oversize_line_fails_the_turn_and_the_next_prompt_gets_a_fresh_process() {
    let mut world = World::with("oversize", |config| {
        config.max_line_bytes = 1 << 20;
    })
    .await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session.clone(), text("flood")).await.unwrap();
    turn.expect(Match::message_contains("about to flood"))
        .await
        .unwrap();
    let Event::Error { error } = turn.expect(Match::error(ErrorKind::Unknown)).await.unwrap()
    else {
        panic!("expected an error")
    };
    assert!(error.message.contains("over 1048576 bytes"), "{error}");
    turn.until_end().await.unwrap();
    assert_eq!(messages(turn.seen()), ["about to flood"]);
    let first = world.pid(0);
    world
        .eventually("the flooding process to be gone", |_| !alive(first))
        .await;

    let mut again = node.prompt(session.clone(), text("hello")).await.unwrap();
    again
        .expect(Match::message_contains("fresh: flood"))
        .await
        .unwrap();
    again
        .expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    again.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 2);
    assert!(world.argv(1).contains(&"--resume".to_owned()));
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_restarted_node_resumes_a_durable_session_with_its_context() {
    let mut world = World::new("resume").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("remember 42"))
        .await
        .unwrap();
    turn.expect(Match::message_contains("noted")).await.unwrap();
    turn.until_end().await.unwrap();
    let first = world.pid(0);
    world.stop_node().await;
    assert!(!alive(first), "shutdown left Claude Code running");

    let node = world.start_node().await;
    let mut turn = node
        .prompt(session.clone(), text("what did I say?"))
        .await
        .unwrap();
    turn.expect(Match::message_contains("recalled: remember 42"))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    assert_eq!(world.starts().len(), 2);
    assert_eq!(
        world.argv(1)[world.argv(1).len() - 2..],
        ["--resume", session.0.as_str()]
    );
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_stray_result_after_resume_does_not_end_the_turn() {
    let mut world = World::new("stray-after-resume").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("remember 42"))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;

    let node = world.start_node().await;
    let mut turn = node.prompt(session, text("what did I say?")).await.unwrap();
    turn.until_end().await.unwrap();
    let seen = turn.seen();
    assert_eq!(messages(seen), ["recalled: remember 42"]);
    assert_eq!(seen.iter().filter(|event| is_complete(event)).count(), 1);
    assert!(is_complete(seen.last().unwrap()), "{seen:?}");
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_resume_miss_is_an_unknown_session_and_its_record_is_removed() {
    let mut world = World::new("resume").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session.clone(), text("remember 42"))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;
    std::fs::remove_file(
        world
            .state()
            .join("sessions")
            .join(format!("{session}.jsonl")),
    )
    .unwrap();

    let node = world.start_node().await;
    let refused = fault(node.prompt(session.clone(), text("what did I say?")).await);
    assert_eq!(refused.kind, ErrorKind::UnknownSession);
    assert!(!world.store().join(format!("{session}.json")).exists());
    let recreated = world
        .starts()
        .iter()
        .skip(1)
        .filter(|start| {
            start["argv"]
                .as_array()
                .unwrap()
                .contains(&json!("--session-id"))
        })
        .count();
    assert_eq!(recreated, 0, "the missing conversation was recreated");
    let missed = world.events("stdout").iter().any(|entry| {
        entry["frame"]["errors"][0]
            .as_str()
            .is_some_and(|error| error.starts_with("No conversation found with session ID"))
    });
    assert!(
        missed,
        "fake-claude never reported the missing conversation"
    );

    let fresh = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(fresh, text("hello")).await.unwrap();
    turn.expect(Match::message_contains("noted")).await.unwrap();
    turn.until_end().await.unwrap();
    world.stop_node().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn shutdown_kills_claude_code_and_its_grandchild_and_the_held_tool_never_runs() {
    let mut world = World::new("grandchild").await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node.prompt(session, text("spawn")).await.unwrap();
    turn.expect(Match::tool_call("Bash")).await.unwrap();
    let pids = world.state().join("pids");
    world
        .eventually("the grandchild to start", |_| pids.exists())
        .await;
    let pids: Vec<u32> = std::fs::read_to_string(&pids)
        .unwrap()
        .lines()
        .map(|pid| pid.parse().unwrap())
        .collect();
    assert!(pids.iter().all(|pid| alive(*pid)));

    assert_eq!(world.stop_node().await, riggs_node::Stopped::Shutdown);
    world
        .eventually("fake-claude and its grandchild to be gone", |_| {
            pids.iter().all(|pid| !alive(*pid))
        })
        .await;
    assert!(!world.work().join("ran.txt").exists());
    assert!(!world.has_event("hook_allowed"));
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "runs the real claude binary against the real model; set RIGGS_LIVE_CLAUDE=1"]
async fn live_claude_answers_a_trivial_prompt() {
    if std::env::var("RIGGS_LIVE_CLAUDE").as_deref() != Ok("1") {
        return;
    }
    let mut world = World::with("basic", |config| {
        config.command = "claude".into();
        config.model = Some("haiku".into());
        config.env.clear();
        config.handshake_timeout = Duration::from_secs(60);
    })
    .await;
    let node = world.start_node().await;
    let session = node.new_session(vec![]).await.unwrap().session_id;
    let mut turn = node
        .prompt(session, text("Reply with exactly the word: pong"))
        .await
        .unwrap();
    turn.expect(Match::complete(StopReason::EndTurn))
        .await
        .unwrap();
    turn.until_end().await.unwrap();
    let said = messages(turn.seen()).join(" ").to_lowercase();
    assert!(said.contains("pong"), "{:?}", turn.seen());
    world.stop_node().await;
}
