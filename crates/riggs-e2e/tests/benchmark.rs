//! Times the real `riggs` binary against the gateway simulator with the scripted fake agents.
//! Ignored by default; run it with `cargo test -p riggs-e2e --test benchmark -- --ignored`.

#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

#[path = "support/mod.rs"]
mod support;

#[allow(dead_code)]
#[path = "daemon/harness.rs"]
mod harness;

use std::collections::BTreeMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use nix::sys::signal::Signal;
use rax::interaction::{DisplayAnswer, DisplayOutcome};
use rax::{Decision, Event};
use rax_sim::{
    AnswerPolicy, LinkChange, Match, ResumeCounts, SimNode, ToolCallReport, TurnReport,
    VerdictPolicy,
};
use serde::Serialize;
use serde_json::{Value, json};

use harness::{Agent, Rig, Riggs};
use support::{TOKEN, all_caps, text};

const DEFAULT_RUNS: usize = 20;
const ATTACHMENT_BYTES: usize = 10 << 20;

fn runs() -> usize {
    std::env::var("RIGGS_BENCH_RUNS")
        .ok()
        .and_then(|runs| runs.parse().ok())
        .filter(|runs| *runs > 0)
        .unwrap_or(DEFAULT_RUNS)
}

#[derive(Debug, Clone, Default, Serialize)]
struct Stats {
    samples: usize,
    p50_ms: f64,
    p95_ms: f64,
    max_ms: f64,
}

fn millis(value: Duration) -> f64 {
    (value.as_micros() as f64) / 1000.0
}

fn stats(mut values: Vec<Duration>) -> Stats {
    values.sort_unstable();
    let at = |fraction: f64| {
        let last = values.len().saturating_sub(1);
        let index = ((values.len() as f64) * fraction).ceil() as usize;
        millis(values[index.saturating_sub(1).min(last)])
    };
    if values.is_empty() {
        return Stats::default();
    }
    Stats {
        samples: values.len(),
        p50_ms: at(0.5),
        p95_ms: at(0.95),
        max_ms: at(1.0),
    }
}

#[derive(Debug, Default)]
struct Outcome {
    startups: Vec<Duration>,
    turns: Vec<TurnReport>,
    tool_calls: Vec<ToolCallReport>,
    resumes: ResumeCounts,
}

impl Outcome {
    fn absorb(&mut self, rig: &Rig) {
        let report = rig.sim.report();
        self.turns.extend(report.turns);
        self.tool_calls.extend(report.tool_calls);
        self.resumes.fresh += report.resumes.fresh;
        self.resumes.accepted += report.resumes.accepted;
        self.resumes.refused += report.resumes.refused;
    }

    fn merge(&mut self, other: &Outcome) {
        self.startups.extend(other.startups.iter().copied());
        self.turns.extend(other.turns.iter().cloned());
        self.tool_calls.extend(other.tool_calls.iter().cloned());
        self.resumes.fresh += other.resumes.fresh;
        self.resumes.accepted += other.resumes.accepted;
        self.resumes.refused += other.resumes.refused;
    }

    fn measured(&self, name: &str) -> Measured {
        Measured {
            scenario: name.to_owned(),
            turns: self.turns.len(),
            startup: stats(self.startups.clone()),
            to_reply: stats(self.turns.iter().filter_map(|turn| turn.to_reply).collect()),
            to_first_event: stats(
                self.turns
                    .iter()
                    .filter_map(|turn| turn.to_first_event)
                    .collect(),
            ),
            to_end: stats(self.turns.iter().filter_map(|turn| turn.to_end).collect()),
            verdict_latency: stats(
                self.tool_calls
                    .iter()
                    .filter_map(|call| call.verdict_latency)
                    .collect(),
            ),
            verdict_to_update: stats(
                self.tool_calls
                    .iter()
                    .filter_map(|call| call.verdict_to_update)
                    .collect(),
            ),
            resumes: self.resumes.clone(),
        }
    }
}

#[derive(Debug, Clone, Serialize)]
struct Measured {
    scenario: String,
    turns: usize,
    startup: Stats,
    to_reply: Stats,
    to_first_event: Stats,
    to_end: Stats,
    verdict_latency: Stats,
    verdict_to_update: Stats,
    resumes: ResumeCounts,
}

#[derive(Debug, Serialize)]
struct Environment {
    git_sha: String,
    riggs_version: String,
    os: String,
    arch: String,
}

fn environment() -> Environment {
    let sha = std::process::Command::new("git")
        .args(["rev-parse", "HEAD"])
        .current_dir(env!("CARGO_MANIFEST_DIR"))
        .output()
        .ok()
        .filter(|found| found.status.success())
        .map(|found| String::from_utf8_lossy(&found.stdout).trim().to_owned())
        .unwrap_or_else(|| "unknown".to_owned());
    Environment {
        git_sha: sha,
        riggs_version: env!("CARGO_PKG_VERSION").to_owned(),
        os: std::env::consts::OS.to_owned(),
        arch: std::env::consts::ARCH.to_owned(),
    }
}

#[derive(Debug, Serialize)]
struct BackendBenchmark {
    backend: String,
    runs: usize,
    environment: Environment,
    scenarios: Vec<Measured>,
    total: Measured,
}

fn output_dir() -> PathBuf {
    let tmp = Path::new(env!("CARGO_TARGET_TMPDIR"));
    let target = tmp.parent().unwrap_or(tmp);
    target.join("riggs-bench")
}

fn write_json(benchmark: &BackendBenchmark) -> PathBuf {
    let dir = output_dir();
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join(format!("{}.json", benchmark.backend));
    let body = serde_json::to_string_pretty(benchmark).unwrap();
    std::fs::write(&path, body).unwrap();
    path
}

fn print_table(benchmark: &BackendBenchmark, path: &Path) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(
        out,
        "\n{} ({} runs per scenario, {} {})",
        benchmark.backend, benchmark.runs, benchmark.environment.os, benchmark.environment.arch
    );
    let _ = writeln!(
        out,
        "{:<12} {:>6} {:>10} {:>10} {:>10} {:>10} {:>10} {:>10}",
        "scenario", "turns", "reply p50", "reply p95", "first p50", "end p50", "end p95", "end max"
    );
    for row in benchmark.scenarios.iter().chain([&benchmark.total]) {
        let _ = writeln!(
            out,
            "{:<12} {:>6} {:>10.1} {:>10.1} {:>10.1} {:>10.1} {:>10.1} {:>10.1}",
            row.scenario,
            row.turns,
            row.to_reply.p50_ms,
            row.to_reply.p95_ms,
            row.to_first_event.p50_ms,
            row.to_end.p50_ms,
            row.to_end.p95_ms,
            row.to_end.max_ms
        );
    }
    let total = &benchmark.total;
    let _ = writeln!(
        out,
        "start-up p50 {:.1} ms, p95 {:.1} ms; verdict p50 {:.1} ms, verdict to update p50 {:.1} ms; resumes {:?}",
        total.startup.p50_ms,
        total.startup.p95_ms,
        total.verdict_latency.p50_ms,
        total.verdict_to_update.p50_ms,
        total.resumes
    );
    let _ = writeln!(out, "written to {}", path.display());
    let _ = out.flush();
}

async fn rig_for(agent: Agent) -> Rig {
    let rig = Rig::new().await;
    rig.configure(agent, "");
    rig.write_token(TOKEN, 0o600);
    rig
}

async fn attached(rig: &Rig, outcome: &mut Outcome) -> (Riggs, SimNode) {
    let started = Instant::now();
    let riggs = rig.start();
    let node = rig.sim.next_node().await.unwrap();
    outcome.startups.push(started.elapsed());
    node.initialize(all_caps()).await.unwrap();
    (riggs, node)
}

async fn severed(rig: &Rig, node: &SimNode) {
    rig.sim.sever();
    loop {
        if matches!(node.next_link_change().await.unwrap(), LinkChange::Resumed) {
            return;
        }
    }
}

fn answers_the_question() -> AnswerPolicy {
    AnswerPolicy::script(|event| {
        let Event::Question { question } = event else {
            return None;
        };
        let mut answers = BTreeMap::new();
        answers.insert("q0".to_owned(), vec!["Blue".to_owned()]);
        Some(DisplayAnswer {
            id: question.id.clone(),
            outcome: DisplayOutcome::Answered,
            answers,
            choice: None,
            user_id: Some("U1".into()),
            note: None,
            code: None,
        })
    })
}

async fn plain_turns(agent: Agent, prompt: &str, runs: usize) -> Outcome {
    let rig = rig_for(agent).await;
    let mut outcome = Outcome::default();
    let (_riggs, node) = attached(&rig, &mut outcome).await;
    for _ in 0..runs {
        let session = node.new_session(vec![]).await.unwrap().session_id;
        let mut turn = node.prompt(session, text(prompt)).await.unwrap();
        turn.until_end().await.unwrap();
    }
    outcome.absorb(&rig);
    outcome
}

async fn allowed_tool_turns(agent: Agent, prompt: &str, runs: usize) -> Outcome {
    let rig = rig_for(agent).await;
    let mut outcome = Outcome::default();
    let (_riggs, node) = attached(&rig, &mut outcome).await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    for _ in 0..runs {
        let session = node.new_session(vec![]).await.unwrap().session_id;
        let mut turn = node.prompt(session, text(prompt)).await.unwrap();
        turn.until_end().await.unwrap();
    }
    outcome.absorb(&rig);
    outcome
}

async fn attachment_turns(agent: Agent, prompt: &str, runs: usize, on_disk: bool) -> Outcome {
    let rig = rig_for(agent).await;
    if on_disk {
        let bytes: Vec<u8> = (0..ATTACHMENT_BYTES)
            .map(|byte| (byte % 251) as u8)
            .collect();
        std::fs::write(rig.work().join("report.bin"), &bytes).unwrap();
    }
    let mut outcome = Outcome::default();
    let (_riggs, node) = attached(&rig, &mut outcome).await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    for _ in 0..runs {
        let session = node.new_session(vec![]).await.unwrap().session_id;
        let mut turn = node.prompt(session, text(prompt)).await.unwrap();
        let Event::Attachment { attachment } = turn.expect(Match::attachment()).await.unwrap()
        else {
            unreachable!()
        };
        assert_eq!(attachment.size, ATTACHMENT_BYTES as u64);
        node.transfer(&attachment.transfer_id).await.unwrap();
        turn.until_end().await.unwrap();
    }
    outcome.absorb(&rig);
    outcome
}

async fn severed_turns(agent: Agent, prompt: &str, runs: usize) -> Outcome {
    let rig = rig_for(agent).await;
    let mut outcome = Outcome::default();
    let (_riggs, node) = attached(&rig, &mut outcome).await;
    for _ in 0..runs {
        let session = node.new_session(vec![]).await.unwrap().session_id;
        let mut turn = node.prompt(session, text(prompt)).await.unwrap();
        let held = Match::when("a tool call", |event| {
            matches!(event, Event::ToolCall { .. })
        });
        let Event::ToolCall { tool_call } = turn.expect(held).await.unwrap() else {
            unreachable!()
        };
        severed(&rig, &node).await;
        severed(&rig, &node).await;
        turn.verdict(tool_call.id.clone(), Decision::Allow)
            .await
            .unwrap();
        turn.until_end().await.unwrap();
    }
    outcome.absorb(&rig);
    outcome
}

async fn restart_turns(
    agent: impl Fn() -> Agent,
    first: &str,
    second: &str,
    runs: usize,
) -> Outcome {
    let rig = rig_for(agent()).await;
    let mut outcome = Outcome::default();
    for _ in 0..runs {
        let (mut riggs, node) = attached(&rig, &mut outcome).await;
        let session = node.new_session(vec![]).await.unwrap().session_id;
        let mut opening = node.prompt(session.clone(), text(first)).await.unwrap();
        opening.until_end().await.unwrap();
        riggs.signal(Signal::SIGKILL);
        riggs.exited().await;

        let (_restarted, node) = attached(&rig, &mut outcome).await;
        let mut resumed = node.prompt(session, text(second)).await.unwrap();
        resumed.until_end().await.unwrap();
    }
    outcome.absorb(&rig);
    outcome
}

async fn ask_turns(runs: usize) -> Outcome {
    let rig = rig_for(Agent::Claude("ask")).await;
    let mut outcome = Outcome::default();
    let (_riggs, node) = attached(&rig, &mut outcome).await;
    node.set_verdicts(VerdictPolicy::AllowAll);
    node.set_answers(answers_the_question());
    for _ in 0..runs {
        let session = node.new_session(vec![]).await.unwrap().session_id;
        let mut turn = node.prompt(session, text("ask me")).await.unwrap();
        turn.expect(Match::message_contains("The person said:"))
            .await
            .unwrap();
        turn.until_end().await.unwrap();
    }
    outcome.absorb(&rig);
    outcome
}

fn acp_script(turns: Value) -> Value {
    json!({"load_session": true, "turns": turns})
}

fn acp_permission(id: &str) -> Value {
    json!({"permission": {
        "tool_call": {
            "toolCallId": id,
            "title": "touch a file",
            "kind": "execute",
            "rawInput": {"command": "touch ran.txt"},
            "_meta": {"claudeCode": {"toolName": "Bash"}},
        },
        "options": [
            {"optionId": "once", "name": "Allow", "kind": "allow_once"},
            {"optionId": "no", "name": "Reject", "kind": "reject_once"},
        ],
    }})
}

async fn claude_code(runs: usize) -> BackendBenchmark {
    let scenarios = vec![
        (
            "text",
            plain_turns(Agent::Claude("basic"), "say pong", runs).await,
        ),
        (
            "tools",
            allowed_tool_turns(Agent::Claude("bench-tools"), "do three things", runs).await,
        ),
        ("ask", ask_turns(runs).await),
        (
            "attachment",
            attachment_turns(Agent::Claude("attach"), "send the report", runs, true).await,
        ),
        (
            "severed",
            severed_turns(Agent::Claude("tool-gate"), "touch it", runs).await,
        ),
        (
            "restart",
            restart_turns(
                || Agent::Claude("resume"),
                "remember 42",
                "what did I say?",
                runs,
            )
            .await,
        ),
    ];
    collect("claude_code", runs, scenarios)
}

async fn acp(runs: usize) -> BackendBenchmark {
    let three = json!([
        acp_permission("tc1"),
        acp_permission("tc2"),
        acp_permission("tc3"),
    ]);
    let scenarios = vec![
        (
            "text",
            plain_turns(
                Agent::Acp(acp_script(json!({"say pong": [{"say": "pong"}]}))),
                "say pong",
                runs,
            )
            .await,
        ),
        (
            "tools",
            allowed_tool_turns(
                Agent::Acp(acp_script(json!({"do three things": three}))),
                "do three things",
                runs,
            )
            .await,
        ),
        (
            "attachment",
            attachment_turns(
                Agent::Acp(acp_script(
                    json!({"send the report": [{"blob": ATTACHMENT_BYTES}]}),
                )),
                "send the report",
                runs,
                false,
            )
            .await,
        ),
        (
            "severed",
            severed_turns(
                Agent::Acp(acp_script(json!({"touch it": [acp_permission("tc1")]}))),
                "touch it",
                runs,
            )
            .await,
        ),
        (
            "restart",
            restart_turns(
                || {
                    Agent::Acp(acp_script(json!({
                        "remember 42": [{"say": "noted"}],
                        "what did I say?": [{"say": "recalled: 42"}],
                    })))
                },
                "remember 42",
                "what did I say?",
                runs,
            )
            .await,
        ),
    ];
    collect("acp", runs, scenarios)
}

fn collect(backend: &str, runs: usize, scenarios: Vec<(&str, Outcome)>) -> BackendBenchmark {
    let mut everything = Outcome::default();
    let mut measured = Vec::new();
    for (name, outcome) in &scenarios {
        measured.push(outcome.measured(name));
        everything.merge(outcome);
    }
    BackendBenchmark {
        backend: backend.to_owned(),
        runs,
        environment: environment(),
        scenarios: measured,
        total: everything.measured("all"),
    }
}

#[ignore = "a benchmark: run it with -- --ignored"]
#[tokio::test(flavor = "multi_thread")]
async fn riggs_and_rax_overhead_per_backend() {
    harness::riggs_binary();
    let runs = runs();
    for benchmark in [claude_code(runs).await, acp(runs).await] {
        let path = write_json(&benchmark);
        print_table(&benchmark, &path);
    }
}
