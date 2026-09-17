//! A scripted stand-in for the Claude Code CLI, so every protocol path Riggs depends on can be
//! driven without the real model. Scripts come from `FAKE_CLAUDE_SCRIPT`; logs go to `FAKE_CLAUDE_STATE`.

#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::panic,
    clippy::zombie_processes
)]

mod script;
mod shapes;

use std::collections::HashMap;
use std::fs::{self, File, OpenOptions};
use std::future::Future;
use std::io::Write as _;
use std::os::unix::process::CommandExt as _;
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use script::{Script, Step};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::{mpsc, oneshot};

enum Flow {
    Continue,
    Exit(i32),
}

struct Fake {
    pid: u32,
    session: String,
    cwd: String,
    state: PathBuf,
    log: Mutex<File>,
    out: mpsc::UnboundedSender<(String, Option<oneshot::Sender<()>>)>,
    waiters: Mutex<HashMap<String, oneshot::Sender<Value>>>,
    hooks: Mutex<HashMap<String, String>>,
    vars: Mutex<HashMap<String, String>>,
    tools: Mutex<Vec<String>>,
    hook_timeout: Mutex<Duration>,
    seq: AtomicU64,
    results: AtomicU64,
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let state = PathBuf::from(std::env::var("FAKE_CLAUDE_STATE").expect("FAKE_CLAUDE_STATE"));
    let script_path = std::env::var("FAKE_CLAUDE_SCRIPT").expect("FAKE_CLAUDE_SCRIPT");
    let script: Script = serde_json::from_slice(&fs::read(&script_path).expect("read the script"))
        .expect("parse the script");
    let flag = |name: &str| {
        args.iter()
            .position(|arg| arg == name)
            .and_then(|at| args.get(at + 1))
            .cloned()
    };
    let resume = flag("--resume");
    let session = resume
        .clone()
        .or_else(|| flag("--session-id"))
        .expect("a session flag");
    let cwd = std::env::current_dir().unwrap().display().to_string();
    fs::create_dir_all(state.join("sessions")).unwrap();
    let log = OpenOptions::new()
        .create(true)
        .append(true)
        .open(state.join("log.jsonl"))
        .unwrap();
    let fake = Arc::new(Fake {
        pid: std::process::id(),
        session: session.clone(),
        cwd: cwd.clone(),
        state: state.clone(),
        log: Mutex::new(log),
        out: stdout_writer(),
        waiters: Mutex::default(),
        hooks: Mutex::default(),
        vars: Mutex::default(),
        tools: Mutex::default(),
        hook_timeout: Mutex::new(Duration::from_secs(600)),
        seq: AtomicU64::new(1),
        results: AtomicU64::new(0),
    });
    fake.log(json!({
        "event": "start", "argv": args, "cwd": cwd,
        "claudecode": std::env::var("CLAUDECODE").ok(), "script": script.source,
    }));
    for (key, value) in [
        ("session_id", session.clone()),
        ("cwd", cwd),
        ("state", state.display().to_string()),
    ] {
        fake.set(key, value);
    }

    let (init_tx, init_rx) = oneshot::channel();
    let (prompt_tx, mut prompts) = mpsc::unbounded_channel();
    let (interrupt_tx, mut interrupts) = mpsc::unbounded_channel();
    tokio::spawn(read_stdin(fake.clone(), init_tx, prompt_tx, interrupt_tx));
    let Ok(init) = init_rx.await else {
        fake.exit(0).await;
    };

    let transcript = fake.transcript();
    if resume.is_some() && !transcript.exists() {
        eprintln!("No conversation found with session ID: {session}");
        fake.emit(shapes::resume_miss(&session)).await;
        fake.exit(1).await;
    }
    if resume.is_none() && transcript.exists() {
        eprintln!("Error: Session ID {session} is already in use.");
        fake.exit(1).await;
    }
    if let Some(ready) = &script.ready_file {
        wait_file(&fake.path(&fake.fill_str(ready))).await;
    }
    let request = &init["request"];
    if let Some(secs) = request
        .pointer("/hooks/PreToolUse/0/timeout")
        .and_then(Value::as_u64)
    {
        *fake.hook_timeout.lock().unwrap() = Duration::from_secs(secs);
    }
    let init_id = init["request_id"].as_str().unwrap_or("req-1").to_owned();
    let mcp = request["sdkMcpServers"]
        .as_array()
        .is_some_and(|servers| servers.iter().any(|server| server == "riggs"));
    if mcp {
        fake.mcp_handshake(&init_id).await;
    } else {
        fake.emit(shapes::control_success(
            &init_id,
            shapes::initialized(fake.pid),
        ))
        .await;
    }

    loop {
        let prompt = tokio::select! {
            prompt = prompts.recv() => prompt,
            Some(interrupt) = interrupts.recv() => {
                fake.emit(shapes::control_success(&interrupt, json!({"still_queued": []}))).await;
                continue;
            }
        };
        let Some(prompt) = prompt else {
            fake.exit(0).await;
        };
        let turn = fake.begin_turn(&prompt);
        let steps = script
            .turns
            .get(turn.min(script.turns.len().saturating_sub(1)))
            .cloned()
            .unwrap_or_default();
        let tools = fake.tools.lock().unwrap().clone();
        let seq = fake.next();
        fake.emit(shapes::init(&fake.session, &fake.cwd, &tools, seq))
            .await;
        let outcome = {
            let run = run(fake.clone(), steps);
            tokio::pin!(run);
            tokio::select! {
                flow = &mut run => Ok(flow),
                Some(interrupt) = interrupts.recv() => Err(interrupt),
            }
        };
        let flow = match outcome {
            Ok(flow) => flow,
            Err(interrupt) => {
                fake.interrupt(&interrupt).await;
                Flow::Continue
            }
        };
        if let Flow::Exit(code) = flow {
            fake.exit(code).await;
        }
    }
}

async fn read_stdin(
    fake: Arc<Fake>,
    init: oneshot::Sender<Value>,
    prompts: mpsc::UnboundedSender<Value>,
    interrupts: mpsc::UnboundedSender<String>,
) {
    let mut init = Some(init);
    let mut lines = BufReader::new(tokio::io::stdin()).lines();
    while let Ok(Some(line)) = lines.next_line().await {
        let Ok(frame) = serde_json::from_str::<Value>(&line) else {
            fake.log(
                json!({"event": "violation", "detail": "stdin line is not JSON", "line": line}),
            );
            continue;
        };
        fake.log(json!({"event": "stdin", "frame": frame}));
        match frame["type"].as_str() {
            Some("control_request") => match frame["request"]["subtype"].as_str() {
                Some("initialize") => {
                    if let Some(init) = init.take() {
                        let _ = init.send(frame);
                    }
                }
                Some("interrupt") => {
                    let id = frame["request_id"].as_str().unwrap_or_default().to_owned();
                    let _ = interrupts.send(id);
                }
                _ => {}
            },
            Some("control_response") => {
                let response = frame["response"].clone();
                fake.check_answer(&response);
                let id = response["request_id"].as_str().unwrap_or_default();
                if let Some(waiter) = fake.waiters.lock().unwrap().remove(id) {
                    let _ = waiter.send(response);
                }
            }
            Some("user") => {
                let _ = prompts.send(frame);
            }
            _ => {}
        }
    }
    fake.log(json!({"event": "stdin_closed"}));
}

fn stdout_writer() -> mpsc::UnboundedSender<(String, Option<oneshot::Sender<()>>)> {
    let (lines, mut queue) = mpsc::unbounded_channel::<(String, Option<oneshot::Sender<()>>)>();
    tokio::spawn(async move {
        let mut out = tokio::io::stdout();
        while let Some((line, written)) = queue.recv().await {
            if !line.is_empty() {
                out.write_all(line.as_bytes()).await.unwrap();
                out.write_all(b"\n").await.unwrap();
            }
            out.flush().await.unwrap();
            if let Some(written) = written {
                let _ = written.send(());
            }
        }
    });
    lines
}

async fn wait_file(path: &Path) {
    while !path.exists() {
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

fn prompt_text(frame: &Value) -> String {
    match &frame["message"]["content"] {
        Value::String(text) => text.clone(),
        Value::Array(blocks) => blocks
            .iter()
            .map(|block| match block["type"].as_str() {
                Some("text") => block["text"].as_str().unwrap_or_default().to_owned(),
                Some("image") => format!(
                    "[image {} {} base64 chars]",
                    block["source"]["media_type"].as_str().unwrap_or_default(),
                    block["source"]["data"].as_str().map_or(0, str::len)
                ),
                other => format!("[{} block]", other.unwrap_or("unknown")),
            })
            .collect::<Vec<_>>()
            .join("\n"),
        _ => String::new(),
    }
}

fn run(fake: Arc<Fake>, steps: Vec<Step>) -> Pin<Box<dyn Future<Output = Flow> + Send>> {
    Box::pin(async move {
        for step in steps {
            if let Flow::Exit(code) = fake.clone().step(step).await {
                return Flow::Exit(code);
            }
        }
        Flow::Continue
    })
}

impl Fake {
    fn log(&self, entry: Value) {
        let mut entry = entry;
        entry["pid"] = json!(self.pid);
        let mut line = entry.to_string();
        line.push('\n');
        let _ = self.log.lock().unwrap().write_all(line.as_bytes());
    }

    fn next(&self) -> u64 {
        self.seq.fetch_add(1, Ordering::Relaxed)
    }

    fn set(&self, key: &str, value: String) {
        self.vars.lock().unwrap().insert(key.to_owned(), value);
    }

    fn fill_str(&self, text: &str) -> String {
        let vars = self.vars.lock().unwrap();
        let mut text = text.to_owned();
        for (key, value) in vars.iter() {
            text = text.replace(&format!("{{{{{key}}}}}"), value);
        }
        text
    }

    fn fill(&self, value: &Value) -> Value {
        match value {
            Value::String(text) => Value::String(self.fill_str(text)),
            Value::Array(items) => Value::Array(items.iter().map(|item| self.fill(item)).collect()),
            Value::Object(fields) => Value::Object(
                fields
                    .iter()
                    .map(|(key, value)| (key.clone(), self.fill(value)))
                    .collect(),
            ),
            other => other.clone(),
        }
    }

    fn path(&self, path: &str) -> PathBuf {
        let path = Path::new(path);
        if path.is_absolute() {
            path.to_path_buf()
        } else {
            Path::new(&self.cwd).join(path)
        }
    }

    fn transcript(&self) -> PathBuf {
        self.state
            .join("sessions")
            .join(format!("{}.jsonl", self.session))
    }

    fn begin_turn(&self, frame: &Value) -> usize {
        let path = self.transcript();
        let history: Vec<String> = fs::read_to_string(&path)
            .unwrap_or_default()
            .lines()
            .filter_map(|line| serde_json::from_str::<Value>(line).ok())
            .filter_map(|entry| entry["prompt"].as_str().map(str::to_owned))
            .collect();
        let text = prompt_text(frame);
        let mut file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&path)
            .unwrap();
        writeln!(file, "{}", json!({"prompt": text})).unwrap();
        let turn = history.len();
        self.set("prompt", text);
        self.set("history", history.join(" | "));
        self.set("turn", turn.to_string());
        turn
    }

    async fn write_line(&self, line: String) {
        let (written, done) = oneshot::channel();
        if self.out.send((line, Some(written))).is_ok() {
            let _ = done.await;
        }
    }

    async fn emit(&self, frame: Value) {
        self.log(json!({"event": "stdout", "frame": frame}));
        self.write_line(frame.to_string()).await;
    }

    async fn exit(&self, code: i32) -> ! {
        self.log(json!({"event": "exit", "code": code}));
        self.write_line(String::new()).await;
        std::process::exit(code)
    }

    fn check_answer(&self, response: &Value) {
        let output = &response["response"]["hookSpecificOutput"];
        let blank = output["permissionDecisionReason"]
            .as_str()
            .is_none_or(|reason| reason.trim().is_empty());
        if output["permissionDecision"] == "deny" && blank {
            self.log(json!({"event": "violation", "detail": "a hook deny without a reason", "response": response}));
        }
        let behaviour = &response["response"];
        let message_blank = behaviour["message"]
            .as_str()
            .is_none_or(|message| message.trim().is_empty());
        if behaviour["behavior"] == "deny" && message_blank {
            self.log(json!({"event": "violation", "detail": "a permission deny without a message", "response": response}));
        }
    }

    fn wait_for(&self, request_id: &str) -> oneshot::Receiver<Value> {
        let (waiter, answered) = oneshot::channel();
        self.waiters
            .lock()
            .unwrap()
            .insert(request_id.to_owned(), waiter);
        answered
    }

    async fn mcp_call(&self, message: Value) -> Value {
        let request_id = format!("mcp-{:08x}-{}", self.pid, self.next());
        let answered = self.wait_for(&request_id);
        self.emit(shapes::mcp_request(&request_id, message)).await;
        answered.await.unwrap_or(Value::Null)
    }

    async fn mcp_handshake(&self, init_id: &str) {
        let initialize = json!({
            "method": "initialize",
            "params": {"protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {
                "name": "claude-code", "title": "Claude Code", "version": "2.1.271",
            }},
            "jsonrpc": "2.0", "id": 0,
        });
        let answer = self.mcp_call(initialize).await;
        if answer.pointer("/response/mcp_response/result").is_none() {
            self.log(
                json!({"event": "violation", "detail": "MCP initialize failed", "answer": answer}),
            );
        }
        self.emit(shapes::control_success(
            init_id,
            shapes::initialized(self.pid),
        ))
        .await;
        let notified = self
            .mcp_call(json!({"jsonrpc": "2.0", "method": "notifications/initialized"}))
            .await;
        if notified["subtype"] != "success" {
            self.log(json!({"event": "violation", "detail": "MCP notification not acknowledged", "answer": notified}));
        }
        let listed = self
            .mcp_call(json!({"method": "tools/list", "jsonrpc": "2.0", "id": 1}))
            .await;
        let tools: Vec<String> = listed
            .pointer("/response/mcp_response/result/tools")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .filter_map(|tool| tool["name"].as_str().map(str::to_owned))
            .collect();
        self.log(json!({"event": "mcp_tools", "tools": tools}));
        *self.tools.lock().unwrap() = tools;
    }

    async fn interrupt(&self, request_id: &str) {
        let held: Vec<(String, String)> = self.hooks.lock().unwrap().drain().collect();
        for (hook, _) in &held {
            self.waiters.lock().unwrap().remove(hook);
            self.emit(json!({"type": "control_cancel_request", "request_id": hook}))
                .await;
        }
        self.emit(shapes::control_success(
            request_id,
            json!({"still_queued": []}),
        ))
        .await;
        for (_, tool_use_id) in &held {
            let rejection = json!(shapes::INTERRUPT_REJECTION);
            let seq = self.next();
            self.emit(shapes::tool_result(
                &self.session,
                tool_use_id,
                &rejection,
                true,
                None,
                seq,
            ))
            .await;
        }
        let seq = self.next();
        self.emit(shapes::interrupt_marker(&self.session, seq))
            .await;
        let index = self.results.fetch_add(1, Ordering::Relaxed);
        self.emit(shapes::interrupted(&self.session, index)).await;
        self.log(json!({"event": "interrupted"}));
    }

    async fn step(self: Arc<Self>, step: Step) -> Flow {
        let session = self.session.clone();
        match step {
            Step::Emit(frame) => self.emit(self.fill(&frame)).await,
            Step::Say(text) => {
                let content = json!([{"type": "text", "text": self.fill_str(&text)}]);
                let seq = self.next();
                self.emit(shapes::assistant(&session, content, None, seq))
                    .await;
            }
            Step::Result { text, num_turns } => {
                let index = self.results.fetch_add(1, Ordering::Relaxed);
                let text = self.fill_str(&text);
                self.emit(shapes::result(&session, &text, num_turns, index))
                    .await;
            }
            Step::Hook {
                id,
                name,
                input,
                agent_id,
                parent,
                allow,
                deny,
            } => {
                return self
                    .hook(id, name, input, agent_id, parent, allow, deny)
                    .await;
            }
            Step::CallTool {
                id,
                name,
                arguments,
                var,
            } => self.call_tool(id, name, arguments, var).await,
            Step::ToolResult {
                id,
                content,
                is_error,
                parent,
            } => {
                let seq = self.next();
                let content = self.fill(&content);
                let frame =
                    shapes::tool_result(&session, &id, &content, is_error, parent.as_deref(), seq);
                self.emit(frame).await;
            }
            Step::Stderr(line) => {
                eprintln!("{}", self.fill_str(&line));
            }
            Step::BigLine(bytes) => {
                let content = json!([{"type": "text", "text": "x".repeat(bytes)}]);
                let seq = self.next();
                let frame = shapes::assistant(&session, content, None, seq);
                self.log(json!({"event": "big_line", "bytes": bytes}));
                self.write_line(frame.to_string()).await;
            }
            Step::Touch(path) => {
                let path = self.path(&self.fill_str(&path));
                fs::write(&path, "ran\n").unwrap();
                self.log(json!({"event": "touched", "path": path}));
            }
            Step::Truncate(path) => {
                let path = self.path(&self.fill_str(&path));
                OpenOptions::new()
                    .write(true)
                    .open(&path)
                    .unwrap()
                    .set_len(0)
                    .unwrap();
                self.log(json!({"event": "truncated", "path": path}));
            }
            Step::SpawnGrandchild(pidfile) => {
                let child = std::process::Command::new("/bin/sleep")
                    .arg("600")
                    .process_group(0)
                    .stdin(std::process::Stdio::null())
                    .stdout(std::process::Stdio::null())
                    .stderr(std::process::Stdio::null())
                    .spawn()
                    .unwrap();
                let pidfile = self.path(&self.fill_str(&pidfile));
                fs::write(&pidfile, format!("{}\n{}\n", self.pid, child.id())).unwrap();
                self.log(json!({"event": "grandchild", "pid": child.id()}));
            }
            Step::WaitFile(path) => wait_file(&self.path(&self.fill_str(&path))).await,
            Step::Hang => std::future::pending::<()>().await,
            Step::Exit(code) => return Flow::Exit(code),
        }
        Flow::Continue
    }

    #[allow(clippy::too_many_arguments)]
    async fn hook(
        self: Arc<Self>,
        id: String,
        name: String,
        input: Value,
        agent_id: Option<String>,
        parent: Option<String>,
        allow: Vec<Step>,
        deny: Vec<Step>,
    ) -> Flow {
        let input = self.fill(&input);
        let seq = self.next();
        let content = shapes::tool_use(&id, &name, &input);
        self.emit(shapes::assistant(
            &self.session,
            content,
            parent.as_deref(),
            seq,
        ))
        .await;
        let request_id = format!("hook-{:08x}-{}", self.pid, self.next());
        let answered = self.wait_for(&request_id);
        self.hooks
            .lock()
            .unwrap()
            .insert(request_id.clone(), id.clone());
        let callback = shapes::hook_callback(
            &request_id,
            &self.session,
            &self.cwd,
            &name,
            &input,
            &id,
            agent_id.as_deref(),
        );
        self.emit(callback).await;
        let limit = *self.hook_timeout.lock().unwrap();
        let answer = tokio::time::timeout(limit, answered).await;
        self.hooks.lock().unwrap().remove(&request_id);
        let reason = match answer {
            Ok(Ok(response)) => {
                let output = &response["response"]["hookSpecificOutput"];
                if response["subtype"] == "success" && output["permissionDecision"] == "allow" {
                    self.log(json!({"event": "hook_allowed", "tool_use_id": id}));
                    return run(self.clone(), allow).await;
                }
                output["permissionDecisionReason"]
                    .as_str()
                    .unwrap_or_default()
                    .to_owned()
            }
            Ok(Err(_)) => return std::future::pending::<Flow>().await,
            Err(_) => {
                self.waiters.lock().unwrap().remove(&request_id);
                self.emit(json!({"type": "control_cancel_request", "request_id": request_id}))
                    .await;
                self.log(json!({"event": "hook_timed_out", "tool_use_id": id}));
                shapes::HOOK_TIMEOUT_TEXT.to_owned()
            }
        };
        self.log(json!({"event": "hook_denied", "tool_use_id": id, "reason": reason}));
        self.set("deny_reason", reason.clone());
        let seq = self.next();
        let frame = shapes::tool_result(
            &self.session,
            &id,
            &json!(reason),
            true,
            parent.as_deref(),
            seq,
        );
        self.emit(frame).await;
        run(self.clone(), deny).await
    }

    async fn call_tool(&self, id: String, name: String, arguments: Value, var: String) {
        let number = self.next();
        let message = json!({
            "method": "tools/call",
            "params": {
                "name": name, "arguments": self.fill(&arguments),
                "_meta": {"claudecode/toolUseId": id, "progressToken": number},
            },
            "jsonrpc": "2.0", "id": number,
        });
        let answer = self.mcp_call(message).await;
        let result = answer
            .pointer("/response/mcp_response/result")
            .cloned()
            .unwrap_or(Value::Null);
        let text = result
            .pointer("/content/0/text")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        let is_error = result["isError"].as_bool().unwrap_or(false);
        self.log(json!({"event": "tool_called", "name": name, "text": text, "is_error": is_error}));
        self.set(&var, text.clone());
        let seq = self.next();
        let content = json!([{"type": "text", "text": text}]);
        let frame = shapes::tool_result(&self.session, &id, &content, is_error, None, seq);
        self.emit(frame).await;
    }
}
