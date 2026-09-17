use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock, PoisonError};
use std::time::Duration;

use rax::ToolCall;
use rax::content::ContentBlock;
use rax::event::StopReason;
use rax::id::{RequestId, ToolCallId};
use rax::tool::{Decision, DeniedBy, ToolCallStatus, ToolCallUpdate};
use riggs_node::{
    BackendError, BackendEvent, HostHandles, SessionKey, ToolGate, TurnHandle, TurnPrompts,
};
use riggs_process::{Descendants, Leader, Pipes, Tail};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::{ChildStdin, ChildStdout, Command};
use tokio::sync::{mpsc, oneshot};
use tokio::task::AbortHandle;
use tokio::time::{Instant, sleep, timeout, timeout_at};
use tokio_util::sync::CancellationToken;

use crate::args::{self, Launch};
use crate::config::ClaudeCodeConfig;
use crate::emit::{self, Emit, Route};
use crate::error::{ClaudeCodeError, StderrTail};
use crate::outcome::{self, Outcome};
use crate::wire::{self, GATE_CALLBACK, RESUME_MISS, ResultFrame, id_of, text_of};
use crate::{mcp, tool};

const WAIT_DELAY: Duration = Duration::from_secs(5);
const STDERR_BYTES: usize = 8 << 10;
const INTERRUPT_WAIT: Duration = Duration::from_secs(5);

pub(crate) struct Ctx {
    pub(crate) config: ClaudeCodeConfig,
    pub(crate) host: OnceLock<HostHandles>,
}

#[derive(Clone)]
pub(crate) struct TurnCtl {
    pub(crate) stream: RequestId,
    pub(crate) prompts: TurnPrompts,
    gate: ToolGate,
    cancelled: CancellationToken,
    interrupted: bool,
    interrupt_sent: CancellationToken,
    closed: CancellationToken,
}

enum Tool {
    Running(Route),
    Denied,
}

enum Answer {
    Hook,
    Permission(Value),
}

struct Gated {
    id: String,
    route: Route,
    settled: Arc<AtomicBool>,
}

struct Inflight {
    task: AbortHandle,
    gated: Option<Gated>,
}

#[derive(Default)]
struct State {
    ours: HashMap<String, oneshot::Sender<Value>>,
    theirs: HashMap<String, Inflight>,
    turn: Option<TurnCtl>,
    tools: HashMap<String, Tool>,
    api_error: Option<String>,
    dead: bool,
}

enum Ended {
    Eof,
    Oversize,
}

pub(crate) struct Proc {
    key: SessionKey,
    ctx: Arc<Ctx>,
    writer: mpsc::UnboundedSender<Vec<u8>>,
    emits: mpsc::UnboundedSender<Emit>,
    kill: mpsc::Sender<()>,
    state: Mutex<State>,
    requests: AtomicU64,
    missed: CancellationToken,
    reaped: CancellationToken,
    exited: CancellationToken,
    status: Mutex<Option<String>>,
    stderr: Tail,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

pub(crate) async fn spawn(
    ctx: Arc<Ctx>,
    key: SessionKey,
    launch: Launch,
) -> Result<Arc<Proc>, ClaudeCodeError> {
    let config = &ctx.config;
    let spawn_error = |source| ClaudeCodeError::Spawn {
        command: config.command.display().to_string(),
        source,
    };
    let mut command = Command::new(&config.command);
    command
        .args(args::argv(config, &key, launch))
        .current_dir(&config.workdir)
        .env_remove("CLAUDECODE")
        .envs(&config.env);
    let (
        leader,
        Pipes {
            stdin,
            stdout,
            stderr,
        },
    ) = Leader::spawn(&mut command, STDERR_BYTES).map_err(spawn_error)?;
    let (writer, lines) = mpsc::unbounded_channel();
    let (emits, queue) = mpsc::unbounded_channel();
    let (kill, kills) = mpsc::channel(1);
    let proc = Arc::new(Proc {
        key,
        ctx: ctx.clone(),
        writer,
        emits,
        kill,
        state: Mutex::new(State::default()),
        requests: AtomicU64::new(1),
        missed: CancellationToken::new(),
        reaped: CancellationToken::new(),
        exited: CancellationToken::new(),
        status: Mutex::new(None),
        stderr,
    });
    tracing::debug!(session_id = %key, pid = leader.pid(), ?launch, "Claude Code started");
    tokio::spawn(write(stdin, lines));
    tokio::spawn(emit::run(ctx.host.get().cloned(), key, queue));
    tokio::spawn(reap(proc.clone(), leader, kills));
    tokio::spawn(read(proc.clone(), stdout));
    match proc.handshake().await {
        Ok(()) => Ok(proc),
        Err(err) => {
            proc.terminate().await;
            Err(err)
        }
    }
}

async fn write(mut stdin: ChildStdin, mut lines: mpsc::UnboundedReceiver<Vec<u8>>) {
    while let Some(line) = lines.recv().await {
        if stdin.write_all(&line).await.is_err() || stdin.flush().await.is_err() {
            break;
        }
    }
}

async fn reap(proc: Arc<Proc>, mut leader: Leader, mut kills: mpsc::Receiver<()>) {
    tokio::select! {
        _ = leader.wait() => {}
        Some(()) = kills.recv() => {}
    }
    let status = match leader.kill_tree(Descendants::default()).await {
        Ok(status) => status.to_string(),
        Err(err) => err.to_string(),
    };
    *lock(&proc.status) = Some(status);
    proc.reaped.cancel();
}

async fn read(proc: Arc<Proc>, stdout: ChildStdout) {
    let limit = proc.ctx.config.max_line_bytes;
    let cap = u64::try_from(limit).unwrap_or(u64::MAX).saturating_add(1);
    let mut reader = BufReader::new(stdout);
    let mut line = Vec::new();
    let gone = async {
        proc.reaped.cancelled().await;
        sleep(WAIT_DELAY).await;
    };
    tokio::pin!(gone);
    let ended = loop {
        line.clear();
        let mut bounded = (&mut reader).take(cap);
        let read = tokio::select! {
            read = bounded.read_until(b'\n', &mut line) => read,
            () = &mut gone => break Ended::Eof,
        };
        match read {
            Ok(0) => break Ended::Eof,
            Ok(_) => {}
            Err(err) => {
                tracing::debug!(session_id = %proc.key, error = %err, "reading Claude Code's output failed");
                break Ended::Eof;
            }
        }
        if line.len() > limit && line.last() != Some(&b'\n') {
            proc.lock().dead = true;
            break Ended::Oversize;
        }
        let frame = line.trim_ascii();
        if frame.is_empty() {
            continue;
        }
        match serde_json::from_slice::<Value>(frame) {
            Ok(frame) => proc.dispatch(frame),
            Err(err) => {
                tracing::warn!(session_id = %proc.key, error = %err, "skipping a line from Claude Code that is not JSON");
            }
        }
    };
    drop(reader);
    proc.finish(ended).await;
}

fn update(id: &str, status: ToolCallStatus, output: Option<Value>) -> BackendEvent {
    BackendEvent::ToolCallUpdate(ToolCallUpdate {
        id: ToolCallId(id.to_owned()),
        status,
        title: None,
        content: Vec::new(),
        output,
    })
}

impl Proc {
    fn lock(&self) -> MutexGuard<'_, State> {
        lock(&self.state)
    }

    pub(crate) fn workdir(&self) -> &Path {
        &self.ctx.config.workdir
    }

    pub(crate) fn is_alive(&self) -> bool {
        !self.lock().dead
    }

    pub(crate) fn emit(&self, route: Route, event: BackendEvent) {
        let _ = self.emits.send(Emit::Event { route, event });
    }

    fn send(&self, frame: &Value) -> Result<(), ClaudeCodeError> {
        self.writer
            .send(wire::line(frame))
            .map_err(|_| ClaudeCodeError::StdinClosed)
    }

    fn tail(&self) -> StderrTail {
        StderrTail(self.stderr.text())
    }

    fn status(&self) -> String {
        lock(&self.status)
            .clone()
            .unwrap_or_else(|| "still running".to_owned())
    }

    fn request(&self, request: Value) -> Result<oneshot::Receiver<Value>, ClaudeCodeError> {
        let id = format!("req-{}", self.requests.fetch_add(1, Ordering::Relaxed));
        let (answer, answered) = oneshot::channel();
        {
            let mut state = self.lock();
            if state.dead {
                return Err(ClaudeCodeError::StdinClosed);
            }
            state.ours.insert(id.clone(), answer);
        }
        self.send(&wire::control_request(&id, request))?;
        Ok(answered)
    }

    async fn handshake(&self) -> Result<(), ClaudeCodeError> {
        let config = &self.ctx.config;
        let answered = self.request(wire::initialize(config.hook_timeout.as_secs().max(1)))?;
        let deadline = sleep(config.handshake_timeout);
        tokio::select! {
            biased;
            () = self.missed.cancelled() => Err(ClaudeCodeError::ResumeMiss(self.key.to_string())),
            answer = answered => match answer {
                Ok(response) if text_of(&response, "subtype") == Some("error") => {
                    let error = text_of(&response, "error").unwrap_or("no reason given");
                    Err(ClaudeCodeError::InitializeRejected(error.to_owned()))
                }
                Ok(_) => Ok(()),
                Err(_) => Err(self.exited_early()),
            },
            () = self.exited.cancelled() => Err(self.exited_early()),
            () = deadline => Err(ClaudeCodeError::HandshakeTimeout {
                secs: config.handshake_timeout.as_secs(),
                stderr: self.tail(),
            }),
        }
    }

    fn exited_early(&self) -> ClaudeCodeError {
        let stderr = self.tail();
        if self.missed.is_cancelled() || stderr.0.contains(RESUME_MISS) {
            return ClaudeCodeError::ResumeMiss(self.key.to_string());
        }
        ClaudeCodeError::ExitedEarly {
            status: self.status(),
            stderr,
        }
    }

    pub(crate) fn kill(&self) {
        let _ = self.kill.try_send(());
    }

    pub(crate) async fn terminate(&self) {
        self.kill();
        if timeout(WAIT_DELAY, self.exited.cancelled()).await.is_err() {
            tracing::warn!(session_id = %self.key, "Claude Code did not exit after it was killed");
        }
    }

    pub(crate) fn begin_turn(
        &self,
        turn: TurnHandle,
        blocks: Vec<Value>,
    ) -> Result<(), ClaudeCodeError> {
        let TurnHandle {
            stream,
            events,
            cancelled,
            gate,
            prompts,
        } = turn;
        let mut state = self.lock();
        if state.dead {
            return Err(ClaudeCodeError::StdinClosed);
        }
        if state.turn.is_some() {
            return Err(ClaudeCodeError::Busy(self.key.to_string()));
        }
        state.turn = Some(TurnCtl {
            stream: stream.clone(),
            prompts,
            gate,
            cancelled,
            interrupted: false,
            interrupt_sent: CancellationToken::new(),
            closed: CancellationToken::new(),
        });
        let _ = self.emits.send(Emit::Begin {
            stream: stream.clone(),
            sink: events,
        });
        if let Err(err) = self.send(&wire::user(blocks)) {
            state.turn = None;
            let _ = self.emits.send(Emit::Abandon { stream });
            return Err(err);
        }
        Ok(())
    }

    pub(crate) fn interrupt(self: &Arc<Self>) {
        let (stream, sent, closed) = {
            let mut state = self.lock();
            let Some(turn) = state.turn.as_mut() else {
                return;
            };
            if turn.interrupted {
                return;
            }
            turn.interrupted = true;
            (
                turn.stream.clone(),
                turn.interrupt_sent.clone(),
                turn.closed.clone(),
            )
        };
        if let Err(err) = self.request(json!({"subtype": "interrupt"})) {
            tracing::debug!(session_id = %self.key, error = %err, "could not interrupt Claude Code");
        }
        sent.cancel();
        let proc = self.clone();
        let grace = self.ctx.config.interrupt_grace;
        tokio::spawn(async move {
            tokio::select! {
                () = closed.cancelled() => {}
                () = proc.exited.cancelled() => {}
                () = sleep(grace) => {
                    tracing::warn!(session_id = %proc.key, "an interrupted turn did not stop in time; killing Claude Code");
                    proc.lock().dead = true;
                    proc.close_turn(&stream, BackendEvent::Complete(Some(StopReason::Cancelled)));
                    proc.kill();
                }
            }
        });
    }

    fn close_turn(&self, stream: &RequestId, event: BackendEvent) {
        let mut state = self.lock();
        if state
            .turn
            .as_ref()
            .is_some_and(|turn| &turn.stream == stream)
            && let Some(turn) = state.turn.take()
        {
            turn.closed.cancel();
            let _ = self.emits.send(Emit::End {
                stream: turn.stream,
                event,
            });
        }
    }

    pub(crate) fn respond(&self, request_id: &str, frame: Value) {
        let mut state = self.lock();
        state.theirs.remove(request_id);
        let _ = self.send(&frame);
    }

    async fn finish(&self, ended: Ended) {
        let oversize = matches!(ended, Ended::Oversize);
        if oversize {
            tracing::warn!(session_id = %self.key, limit = self.ctx.config.max_line_bytes, "Claude Code wrote an oversized line; restarting its process");
        }
        self.kill();
        if timeout(WAIT_DELAY, self.reaped.cancelled()).await.is_err() {
            self.kill();
            let _ = timeout(WAIT_DELAY, self.reaped.cancelled()).await;
        }
        let _ = timeout(Duration::from_secs(1), self.stderr.closed()).await;
        let mut state = self.lock();
        state.dead = true;
        state.ours.clear();
        let turn = state.turn.take();
        let inflight: Vec<Inflight> = state.theirs.drain().map(|(_, inflight)| inflight).collect();
        for Inflight { task, gated } in inflight {
            task.abort();
            if let Some(gated) = gated
                && !gated.settled.swap(true, Ordering::SeqCst)
            {
                self.emit(gated.route, update(&gated.id, ToolCallStatus::Denied, None));
            }
        }
        state.tools.clear();
        if let Some(turn) = turn {
            let event = if turn.interrupted {
                BackendEvent::Complete(Some(StopReason::Cancelled))
            } else if oversize {
                BackendEvent::Error(BackendError::new(ClaudeCodeError::LineTooLong {
                    limit: self.ctx.config.max_line_bytes,
                }))
            } else {
                BackendEvent::Error(BackendError::new(ClaudeCodeError::StreamClosed {
                    status: self.status(),
                    stderr: self.tail(),
                }))
            };
            turn.closed.cancel();
            let _ = self.emits.send(Emit::End {
                stream: turn.stream,
                event,
            });
        }
        drop(state);
        tracing::debug!(session_id = %self.key, status = %self.status(), "Claude Code exited");
        self.exited.cancel();
    }

    fn route(state: &State) -> Route {
        state
            .turn
            .as_ref()
            .map_or(Route::Background, |turn| Route::Turn(turn.stream.clone()))
    }

    fn dispatch(self: &Arc<Self>, frame: Value) {
        match text_of(&frame, "type") {
            Some("control_response") => self.control_response(&frame),
            Some("control_request") => self.control_request(frame),
            Some("control_cancel_request") => self.cancel_request(&frame),
            Some("result") => self.result(frame),
            Some("assistant") => self.assistant(&frame),
            Some("user") => self.tool_results(&frame),
            Some("system") => self.system(&frame),
            _ => {}
        }
    }

    fn control_response(&self, frame: &Value) {
        let response = frame.get("response").cloned().unwrap_or(Value::Null);
        let Some(id) = response.get("request_id").and_then(id_of) else {
            return;
        };
        if let Some(answer) = self.lock().ours.remove(&id) {
            let _ = answer.send(response);
        }
    }

    fn control_request(self: &Arc<Self>, frame: Value) {
        let Some(request_id) = frame.get("request_id").and_then(id_of) else {
            return;
        };
        let request = frame.get("request").cloned().unwrap_or(Value::Null);
        let input = request.get("input").cloned().unwrap_or(Value::Null);
        match text_of(&request, "subtype") {
            Some("hook_callback") if text_of(&request, "callback_id") == Some(GATE_CALLBACK) => {
                let tool_use_id = text_of(&request, "tool_use_id")
                    .or_else(|| text_of(&input, "tool_use_id"))
                    .map_or_else(|| format!("hook-{request_id}"), str::to_owned);
                let name = text_of(&input, "tool_name").unwrap_or("unknown");
                let tool_input = input.get("tool_input").cloned().unwrap_or(Value::Null);
                let call = tool::call(&tool_use_id, name, tool_input);
                self.gate(request_id, call, Answer::Hook);
            }
            Some("can_use_tool") => {
                let tool_use_id = text_of(&request, "tool_use_id")
                    .map_or_else(|| format!("permission-{request_id}"), str::to_owned);
                let name = ["tool_name", "toolName", "name"]
                    .iter()
                    .find_map(|key| text_of(&request, key))
                    .unwrap_or("unknown");
                if matches!(self.lock().tools.get(&tool_use_id), Some(Tool::Running(_))) {
                    let _ = self.send(&wire::permission_answer(&request_id, &input, None));
                    return;
                }
                let call = tool::call(&tool_use_id, name, input.clone());
                self.gate(request_id, call, Answer::Permission(input));
            }
            Some("mcp_message") => {
                let mut state = self.lock();
                if state.dead {
                    return;
                }
                let turn = state.turn.clone();
                let serve = mcp::serve(self.clone(), request_id.clone(), request, turn);
                let task = tokio::spawn(serve).abort_handle();
                state
                    .theirs
                    .insert(request_id, Inflight { task, gated: None });
            }
            subtype => {
                let error = format!(
                    "riggs: unsupported control request \"{}\"",
                    subtype.unwrap_or_default()
                );
                let _ = self.send(&wire::failure(&request_id, &error));
            }
        }
    }

    fn gate(self: &Arc<Self>, request_id: String, call: ToolCall, answer: Answer) {
        let mut state = self.lock();
        if state.dead {
            return;
        }
        let turn = state.turn.clone();
        let route = Self::route(&state);
        let settled = Arc::new(AtomicBool::new(false));
        let gated = Gated {
            id: call.id.0.clone(),
            route: route.clone(),
            settled: settled.clone(),
        };
        let hold = self
            .clone()
            .hold(request_id.clone(), call, answer, turn, route, settled);
        let task = tokio::spawn(hold).abort_handle();
        state.theirs.insert(
            request_id,
            Inflight {
                task,
                gated: Some(gated),
            },
        );
    }

    async fn hold(
        self: Arc<Self>,
        request_id: String,
        call: ToolCall,
        answer: Answer,
        turn: Option<TurnCtl>,
        route: Route,
        settled: Arc<AtomicBool>,
    ) {
        let config = &self.ctx.config;
        let deadline = Instant::now() + config.hook_timeout.saturating_sub(config.hook_margin);
        self.flush(deadline).await;
        let id = call.id.0.clone();
        let decision = match (&turn, self.ctx.host.get()) {
            (Some(turn), _) => turn.gate.hold(call, deadline).await,
            (None, Some(host)) => host.gate.hold_background(&self.key, call, deadline).await,
            (None, None) => Decision::Deny {
                by: DeniedBy::Unavailable,
                reason: None,
            },
        };
        let deny = match decision {
            Decision::Allow => None,
            Decision::Deny { by, reason } => Some(tool::deny_reason(by, reason)),
        };
        if deny.is_some()
            && let Some(turn) = &turn
            && turn.cancelled.is_cancelled()
        {
            let _ = timeout(INTERRUPT_WAIT, turn.interrupt_sent.cancelled()).await;
        }
        let mut state = self.lock();
        if settled.swap(true, Ordering::SeqCst) {
            return;
        }
        state.theirs.remove(&request_id);
        let (status, running) = match deny {
            None => (ToolCallStatus::InProgress, Tool::Running(route.clone())),
            Some(_) => (ToolCallStatus::Denied, Tool::Denied),
        };
        state.tools.insert(id.clone(), running);
        self.emit(route, update(&id, status, None));
        let frame = match answer {
            Answer::Hook => wire::hook_answer(&request_id, deny.as_deref()),
            Answer::Permission(input) => {
                wire::permission_answer(&request_id, &input, deny.as_deref())
            }
        };
        let _ = self.send(&frame);
    }

    async fn flush(&self, deadline: Instant) {
        let (done, flushed) = oneshot::channel();
        if self.emits.send(Emit::Flush(done)).is_ok() {
            let _ = timeout_at(deadline, flushed).await;
        }
    }

    fn cancel_request(&self, frame: &Value) {
        let Some(request_id) = frame.get("request_id").and_then(id_of) else {
            return;
        };
        let mut state = self.lock();
        let Some(Inflight { task, gated }) = state.theirs.remove(&request_id) else {
            return;
        };
        task.abort();
        if let Some(gated) = gated
            && !gated.settled.swap(true, Ordering::SeqCst)
        {
            state.tools.insert(gated.id.clone(), Tool::Denied);
            self.emit(gated.route, update(&gated.id, ToolCallStatus::Denied, None));
        }
    }

    fn result(&self, frame: Value) {
        let parsed: ResultFrame = serde_json::from_value(frame).unwrap_or_default();
        let outcome = outcome::of(parsed);
        let mut state = self.lock();
        let api_error = state.api_error.take();
        match &outcome {
            Outcome::Stray => {
                tracing::debug!(session_id = %self.key, "ignoring a result for a prompt Claude Code queued itself");
                return;
            }
            Outcome::ResumeMiss if state.turn.is_none() => {
                self.missed.cancel();
                return;
            }
            _ => {}
        }
        let result = outcome.failure(api_error);
        let credential = matches!(&result, Err(err) if err.is_credential());
        if credential {
            state.dead = true;
        }
        match state.turn.take() {
            Some(turn) => {
                let event = match result {
                    _ if turn.interrupted => BackendEvent::Complete(Some(StopReason::Cancelled)),
                    Ok(stop) => BackendEvent::Complete(Some(stop)),
                    Err(err) => BackendEvent::Error(BackendError::new(err)),
                };
                turn.closed.cancel();
                let _ = self.emits.send(Emit::End {
                    stream: turn.stream,
                    event,
                });
            }
            None => match result {
                Ok(stop) => self.emit(Route::Background, BackendEvent::Complete(Some(stop))),
                Err(err @ ClaudeCodeError::Api { .. }) => {
                    self.emit(
                        Route::Background,
                        BackendEvent::Error(BackendError::new(err)),
                    );
                }
                Err(err) => {
                    tracing::debug!(session_id = %self.key, error = %err, "a result with no turn open ended early");
                }
            },
        }
        drop(state);
        if credential {
            tracing::info!(session_id = %self.key, "Claude Code's credential was refused; restarting its process");
            self.kill();
        }
    }

    fn assistant(&self, frame: &Value) {
        let mut state = self.lock();
        if frame.get("is_api_error_message").and_then(Value::as_bool) == Some(true) {
            state.api_error = text_of(frame, "error").map(str::to_owned);
            return;
        }
        if frame
            .get("parent_tool_use_id")
            .is_some_and(|parent| !parent.is_null())
        {
            return;
        }
        let route = Self::route(&state);
        let blocks = frame
            .pointer("/message/content")
            .and_then(Value::as_array)
            .into_iter()
            .flatten();
        for block in blocks {
            if text_of(block, "type") == Some("text")
                && let Some(text) = text_of(block, "text").filter(|text| !text.is_empty())
            {
                let content = ContentBlock::text(text).into();
                self.emit(route.clone(), BackendEvent::Message(content));
            }
        }
    }

    fn tool_results(&self, frame: &Value) {
        let mut state = self.lock();
        let blocks = frame
            .pointer("/message/content")
            .and_then(Value::as_array)
            .into_iter()
            .flatten();
        for block in blocks {
            if text_of(block, "type") != Some("tool_result") {
                continue;
            }
            let Some(id) = text_of(block, "tool_use_id") else {
                continue;
            };
            if let Some(Tool::Running(route)) = state.tools.remove(id) {
                let failed = block.get("is_error").and_then(Value::as_bool) == Some(true);
                let status = if failed {
                    ToolCallStatus::Failed
                } else {
                    ToolCallStatus::Completed
                };
                let output = block.get("content").cloned();
                self.emit(route, update(id, status, output));
            }
        }
    }

    fn system(&self, frame: &Value) {
        if text_of(frame, "subtype") != Some("api_retry") {
            return;
        }
        let state = self.lock();
        let status = frame
            .get("error_status")
            .map_or_else(|| "an error".to_owned(), Value::to_string);
        let attempt = frame
            .get("attempt")
            .map_or_else(String::new, Value::to_string);
        let most = frame
            .get("max_retries")
            .map_or_else(String::new, Value::to_string);
        let text =
            format!("The model API answered {status}. Retrying, attempt {attempt} of {most}.");
        self.emit(Self::route(&state), BackendEvent::Status(text));
    }
}
