#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

mod support;

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use futures_util::FutureExt;
use rax::content::ContentBlock;
use rax::credential::CredentialHealth;
use rax::event::StopReason;
use rax::interaction::{
    DisplayAnswer, DisplayOutcome, Question, QuestionRequest, SignInRequest, SignInSettled,
    SignInState,
};
use rax::session::GatewayCapabilities;
use rax::{Event, NodeCall, NodeReply};
use rax_tokio::gateway::LinkEvent;
use riggs_node::{BackendEvent, SignInRefused};
use support::*;
use tokio::sync::{Mutex, mpsc, oneshot};

fn question(id: &str) -> QuestionRequest {
    QuestionRequest {
        id: id.into(),
        title: None,
        questions: vec![Question {
            key: "go".into(),
            header: None,
            question: "Shall I go ahead?".into(),
            options: vec![],
            multi_select: false,
        }],
    }
}

fn answered(id: &str, answer: &str) -> DisplayAnswer {
    DisplayAnswer {
        id: id.into(),
        outcome: DisplayOutcome::Answered,
        answers: BTreeMap::from([("go".to_owned(), vec![answer.to_owned()])]),
        choice: None,
        user_id: None,
        note: None,
        code: None,
    }
}

fn sign_in(id: &str) -> SignInRequest {
    SignInRequest {
        id: id.into(),
        tool: "Claude Code".into(),
        url: Some("https://claude.com/oauth/authorize?x".into()),
        needs_code: true,
        command: None,
    }
}

fn settled(id: &str, state: SignInState) -> SignInSettled {
    SignInSettled {
        id: id.into(),
        state,
        reason: None,
        url: None,
    }
}

fn health(degraded: bool) -> CredentialHealth {
    CredentialHealth {
        credential: "claude".into(),
        degraded,
        reason: degraded.then(|| "token expired".to_owned()),
        since: None,
        expires_at: None,
    }
}

type Gate = Arc<Mutex<Option<oneshot::Receiver<()>>>>;

async fn opened(gate: &Gate) {
    if let Some(open) = gate.lock().await.take() {
        let _ = open.await;
    }
}

struct Asking {
    answers: mpsc::UnboundedReceiver<DisplayAnswer>,
    finish: oneshot::Sender<()>,
}

fn asking(fake: &Fake) -> Asking {
    let (answered, answers) = mpsc::unbounded_channel();
    let (finish, finished) = oneshot::channel();
    let gate: Gate = Arc::new(Mutex::new(Some(finished)));
    fake.on_prompt(script(move |turn: Turn| {
        let (answered, gate) = (answered.clone(), gate.clone());
        async move {
            let prompts = turn.handle.prompts.clone();
            let asked = tokio::spawn(async move { prompts.question(question("q1")).await });
            if turn.text == "until cancelled" {
                turn.handle.cancelled.cancelled().await;
                let _ = answered.send(asked.await.unwrap());
                let stop = BackendEvent::Complete(Some(StopReason::Cancelled));
                let _ = turn.handle.events.send(stop).await;
                return;
            }
            opened(&gate).await;
            complete(&turn).await;
            drop(turn);
            let _ = answered.send(asked.await.unwrap());
        }
    }));
    Asking { answers, finish }
}

#[tokio::test(flavor = "multi_thread")]
async fn an_answered_question_reaches_the_agent() {
    let fake = Fake::new();
    let (answered_tx, mut answers) = mpsc::unbounded_channel();
    fake.on_prompt(script(move |turn: Turn| {
        let answered_tx = answered_tx.clone();
        async move {
            let answer = turn.handle.prompts.question(question("q1")).await;
            let said = format!("you chose {:?}", answer.answers.get("go"));
            let message = BackendEvent::Message(ContentBlock::text(said).into());
            turn.handle.events.send(message).await.unwrap();
            answered_tx.send(answer).unwrap();
            complete(&turn).await;
        }
    }));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, all_caps()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "ask me").await;

    let Event::Question { question: asked } = next_event(&mut events).await else {
        panic!("expected a question")
    };
    assert_eq!(asked, question("q1"));
    link.answer(answered("q1", "yes")).await.unwrap();
    let answer = within(answers.recv()).await.unwrap();
    assert_eq!(answer.outcome, DisplayOutcome::Answered);
    assert!(matches!(
        next_event(&mut events).await,
        Event::Message { .. }
    ));
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_question_still_pending_when_the_turn_ends_is_dismissed() {
    let fake = Fake::new();
    let mut asking = asking(&fake);
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, all_caps()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "ask me").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::Question { .. }
    ));

    asking.finish.send(()).unwrap();
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;
    let answer = within(asking.answers.recv()).await.unwrap();
    assert_eq!(answer.outcome, DisplayOutcome::Dismissed);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_question_pending_when_the_turn_is_cancelled_is_dismissed() {
    let fake = Fake::new();
    let mut asking = asking(&fake);
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, all_caps()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "until cancelled").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::Question { .. }
    ));

    cancel(link, &session).await;
    let answer = within(asking.answers.recv()).await.unwrap();
    assert_eq!(answer.outcome, DisplayOutcome::Dismissed);
    assert_eq!(
        next_event(&mut events).await,
        Event::Complete {
            stop_reason: Some(StopReason::Cancelled)
        }
    );
    ended(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_refused_resume_dismisses_pending_questions_without_ending_the_old_stream() {
    let fake = Fake::new();
    let mut seen = fake.seen();
    let mut asking = asking(&fake);
    let h = harness_behind_proxy(fake.clone(), ephemeral()).await;
    let (restarted, mut restarted_links) = gateway(gateway_config()).await;
    initialize(&h.gateway.link, all_caps()).await;
    let session = new_session(&h.gateway.link).await;
    let mut events = accepted(&h.gateway.link, &session, "until cancelled").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::Question { .. }
    ));

    let proxy = h.proxy.as_ref().unwrap();
    proxy.retarget(restarted.local_addr());
    proxy.sever();
    let answer = within(asking.answers.recv()).await.unwrap();
    assert_eq!(answer.outcome, DisplayOutcome::Dismissed);
    seen.wait_for(|seen| matches!(seen, Seen::Cancel(_))).await;

    let restarted = accept(&mut restarted_links).await;
    initialize(&restarted.link, all_caps()).await;
    fake.on_prompt(script(|turn: Turn| async move { complete(&turn).await }));
    let mut again = accepted(&restarted.link, &session, "hello again").await;
    assert!(matches!(
        next_event(&mut again).await,
        Event::Complete { .. }
    ));
    ended(&mut again).await;
    assert!(
        events.recv().now_or_never().is_none(),
        "the old stream must not be ended on a link that no longer exists"
    );
    drop(asking.finish);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_gateway_that_cannot_draw_questions_gets_none_and_the_agent_hears_unavailable() {
    let fake = Fake::new();
    let (answered_tx, mut answers) = mpsc::unbounded_channel();
    fake.on_prompt(script(move |turn: Turn| {
        let answered_tx = answered_tx.clone();
        async move {
            answered_tx
                .send(turn.handle.prompts.question(question("q1")).await)
                .unwrap();
            let sign_in = turn.handle.prompts.sign_in(sign_in("s1")).await;
            let mut sign_in_answers = sign_in.answers;
            answered_tx
                .send(sign_in_answers.recv().await.unwrap())
                .unwrap();
            let never_shown = BackendEvent::SignInSettled(settled("s1", SignInState::Cancelled));
            turn.handle.events.send(never_shown).await.unwrap();
            complete(&turn).await;
        }
    }));
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, GatewayCapabilities::default()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "ask me").await;

    assert_eq!(
        within(answers.recv()).await.unwrap().outcome,
        DisplayOutcome::Unavailable
    );
    assert_eq!(
        within(answers.recv()).await.unwrap().outcome,
        DisplayOutcome::Unavailable
    );
    assert!(
        matches!(next_event(&mut events).await, Event::Complete { .. }),
        "no question, sign-in or settle may reach a gateway that cannot draw them"
    );
    ended(&mut events).await;
    h.node.shut_down().await;
}

fn signing_in(fake: &Fake) {
    fake.on_prompt(script(|turn: Turn| async move {
        let events = &turn.handle.events;
        let mut prompt = turn.handle.prompts.sign_in(sign_in("s1")).await;
        while let Some(answer) = prompt.answers.recv().await {
            let settle = match (answer.outcome, answer.code) {
                (DisplayOutcome::Approved, _) => SignInSettled {
                    url: Some("https://claude.com/oauth/authorize?x".into()),
                    ..settled("s1", SignInState::Ready)
                },
                (DisplayOutcome::Answered, Some(_)) => settled("s1", SignInState::Success),
                _ => settled("s1", SignInState::Cancelled),
            };
            let terminal = settle.state.is_terminal();
            events
                .send(BackendEvent::SignInSettled(settle))
                .await
                .unwrap();
            if terminal {
                break;
            }
        }
        let stop = if turn.handle.cancelled.is_cancelled() {
            StopReason::Cancelled
        } else {
            StopReason::EndTurn
        };
        let _ = events.send(BackendEvent::Complete(Some(stop))).await;
    }));
}

fn settle_state(event: Event) -> SignInState {
    match event {
        Event::SignInSettled { sign_in_settled } => sign_in_settled.state,
        other => panic!("expected sign_in_settled, got {other:?}"),
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn a_sign_in_inside_a_turn_settles_on_its_stream_in_order() {
    let fake = Fake::new();
    signing_in(&fake);
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, all_caps()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "sign me in").await;

    assert!(matches!(
        next_event(&mut events).await,
        Event::SignIn { .. }
    ));
    let approved = DisplayAnswer {
        outcome: DisplayOutcome::Approved,
        answers: BTreeMap::new(),
        ..answered("s1", "")
    };
    link.answer(approved).await.unwrap();
    assert_eq!(
        settle_state(next_event(&mut events).await),
        SignInState::Ready
    );
    let code = DisplayAnswer {
        code: Some("123".into()),
        ..answered("s1", "")
    };
    link.answer(code).await.unwrap();
    assert_eq!(
        settle_state(next_event(&mut events).await),
        SignInState::Success
    );
    assert_eq!(
        next_event(&mut events).await,
        Event::Complete {
            stop_reason: Some(StopReason::EndTurn)
        }
    );
    ended(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_cancelled_turn_can_still_settle_its_sign_in_before_it_ends() {
    let fake = Fake::new();
    signing_in(&fake);
    let h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, all_caps()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "sign me in").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::SignIn { .. }
    ));

    cancel(link, &session).await;
    assert_eq!(
        settle_state(next_event(&mut events).await),
        SignInState::Cancelled
    );
    assert_eq!(
        next_event(&mut events).await,
        Event::Complete {
            stop_reason: Some(StopReason::Cancelled)
        }
    );
    ended(&mut events).await;
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_prompt_raised_after_its_turn_ended_hears_no_conversation() {
    let fake = Fake::new();
    let (answered_tx, mut answers) = mpsc::unbounded_channel();
    let (ask, asked) = oneshot::channel::<()>();
    let gate: Gate = Arc::new(Mutex::new(Some(asked)));
    fake.on_prompt(script(move |turn: Turn| {
        let (answered_tx, gate) = (answered_tx.clone(), gate.clone());
        async move {
            let prompts = turn.handle.prompts.clone();
            complete(&turn).await;
            drop(turn);
            opened(&gate).await;
            answered_tx
                .send(prompts.question(question("late")).await)
                .unwrap();
        }
    }));
    let mut h = harness(fake, ephemeral()).await;
    let link = &h.gateway.link;
    initialize(link, all_caps()).await;
    let session = new_session(link).await;
    let mut events = accepted(link, &session, "ask me later").await;
    assert!(matches!(
        next_event(&mut events).await,
        Event::Complete { .. }
    ));
    ended(&mut events).await;

    ask.send(()).unwrap();
    assert_eq!(
        within(answers.recv()).await.unwrap().outcome,
        DisplayOutcome::NoConversation
    );
    assert!(quiet(h.gateway.events.recv()).await, "nothing may be sent");
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn a_sign_in_the_gateway_never_takes_is_withdrawn() {
    let fake = Fake::new();
    let mut config = ephemeral();
    config.call_timeout = Duration::from_millis(300);
    let mut h = harness(fake.clone(), config).await;
    initialize(&h.gateway.link, all_caps()).await;
    let host = fake.host().await;

    let raising = tokio::spawn(async move { host.sign_ins.raise(sign_in("s1")).await });
    let LinkEvent::Request {
        call: NodeCall::SignIn(request),
        ..
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected sign_in")
    };
    assert_eq!(request.id, "s1".into());
    let LinkEvent::Request {
        id,
        call: NodeCall::SignInSettled(withdrawn),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected sign_in.settled")
    };
    assert_eq!(withdrawn.id, "s1".into());
    assert_eq!(withdrawn.state, SignInState::Cancelled);
    h.gateway
        .link
        .reply(id, NodeReply::SignInSettled)
        .await
        .unwrap();
    assert!(matches!(
        within(raising).await.unwrap(),
        Err(SignInRefused::TimedOut)
    ));
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn sign_in_updates_go_one_at_a_time() {
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, all_caps()).await;
    let host = fake.host().await;

    let raising = tokio::spawn({
        let host = host.clone();
        async move { host.sign_ins.raise(sign_in("s1")).await }
    });
    let LinkEvent::Request { id, .. } = next_link(&mut h.gateway.events).await else {
        panic!("expected sign_in")
    };
    h.gateway.link.reply(id, NodeReply::SignIn).await.unwrap();
    let mut prompt = within(raising).await.unwrap().unwrap();

    h.gateway
        .link
        .answer(DisplayAnswer {
            code: Some("123".into()),
            ..answered("s1", "")
        })
        .await
        .unwrap();
    assert_eq!(
        within(prompt.answers.recv()).await.unwrap().code.as_deref(),
        Some("123")
    );

    let working = tokio::spawn({
        let host = host.clone();
        async move {
            host.sign_ins
                .settle(settled("s1", SignInState::Working))
                .await
        }
    });
    let LinkEvent::Request {
        id: first,
        call: NodeCall::SignInSettled(update),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the first settle")
    };
    assert_eq!(update.state, SignInState::Working);
    let success = tokio::spawn({
        let host = host.clone();
        async move {
            host.sign_ins
                .settle(settled("s1", SignInState::Success))
                .await
        }
    });
    assert!(
        quiet(h.gateway.events.recv()).await,
        "a second update was sent before the first was answered"
    );

    h.gateway
        .link
        .reply(first, NodeReply::SignInSettled)
        .await
        .unwrap();
    within(working).await.unwrap();
    let LinkEvent::Request {
        id: second,
        call: NodeCall::SignInSettled(update),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the second settle")
    };
    assert_eq!(update.state, SignInState::Success);
    h.gateway
        .link
        .reply(second, NodeReply::SignInSettled)
        .await
        .unwrap();
    within(success).await.unwrap();
    assert_eq!(within(prompt.answers.recv()).await, None);
    h.node.shut_down().await;
}

#[tokio::test(flavor = "multi_thread")]
async fn credential_health_is_pushed_after_initialize_and_kept_in_order() {
    let fake = Fake::new();
    let mut h = harness(fake.clone(), ephemeral()).await;
    initialize(&h.gateway.link, all_caps()).await;
    let host = fake.host().await;
    let reporting = tokio::spawn({
        let host = host.clone();
        async move { host.credentials.report(health(false)).await }
    });
    let LinkEvent::Request {
        id,
        call: NodeCall::CredentialHealth(live),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the live report")
    };
    assert_eq!(live, health(false));
    h.gateway
        .link
        .reply(id, NodeReply::CredentialHealth)
        .await
        .unwrap();
    within(reporting).await.unwrap();

    initialize(&h.gateway.link, all_caps()).await;
    let LinkEvent::Request {
        id,
        call: NodeCall::CredentialHealth(snapshot),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the snapshot")
    };
    assert_eq!(snapshot, health(false));
    h.gateway
        .link
        .reply(id, NodeReply::CredentialHealth)
        .await
        .unwrap();

    let degraded = tokio::spawn({
        let host = host.clone();
        async move { host.credentials.report(health(true)).await }
    });
    let LinkEvent::Request {
        id: first,
        call: NodeCall::CredentialHealth(report),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the degraded report")
    };
    assert!(report.degraded);
    let recovered = tokio::spawn({
        let host = host.clone();
        async move { host.credentials.report(health(false)).await }
    });
    assert!(
        quiet(h.gateway.events.recv()).await,
        "a recovery overtook the failure it ends"
    );
    h.gateway
        .link
        .reply(first, NodeReply::CredentialHealth)
        .await
        .unwrap();
    within(degraded).await.unwrap();
    let LinkEvent::Request {
        id: second,
        call: NodeCall::CredentialHealth(report),
    } = next_link(&mut h.gateway.events).await
    else {
        panic!("expected the recovery")
    };
    assert!(!report.degraded);
    h.gateway
        .link
        .reply(second, NodeReply::CredentialHealth)
        .await
        .unwrap();
    within(recovered).await.unwrap();
    h.node.shut_down().await;
}
