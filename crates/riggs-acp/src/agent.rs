use std::path::Path;
use std::process::ExitStatus;
use std::sync::Arc;
use std::time::Duration;

use agent_client_protocol::{self as acp, ConnectionTo, Responder, UntypedMessage};
use riggs_node::TurnHandle;
use riggs_process::{Descendants, Leader, Pipes, Tail};
use serde_json::{Value, json};
use tokio::sync::{oneshot, watch};
use tokio::task::JoinHandle;
use tokio::time::timeout;
use tokio_util::sync::CancellationToken;

use crate::config::AcpConfig;
use crate::error::AcpError;
use crate::proc;
use crate::routes::{Out, Owner, Routes};
use crate::transport::{self, AgentLines, MAX_LINE_BYTES};
use crate::turn::{self, TurnTask};
use crate::wire::{self, AgentInfo};

/// How long a closing or failed agent gets to exit, and to flush its output, before it is killed.
pub(crate) const EXIT_GRACE: Duration = Duration::from_secs(1);

#[derive(Debug, Clone)]
pub(crate) enum Death {
    Closed,
    Failed(String),
}

impl Death {
    pub(crate) fn error(&self) -> AcpError {
        match self {
            Self::Closed => AcpError::Closed,
            Self::Failed(reason) => AcpError::AgentGone(reason.clone()),
        }
    }
}

#[derive(Clone)]
pub(crate) struct Life {
    close: CancellationToken,
    death: watch::Receiver<Option<Death>>,
}

impl Life {
    pub(crate) async fn died(&self) -> Death {
        let mut death = self.death.clone();
        match death.wait_for(Option::is_some).await {
            Ok(death) => death.clone().unwrap_or(Death::Closed),
            Err(_) => Death::Failed("the agent supervisor stopped".to_owned()),
        }
    }

    pub(crate) fn close(&self) {
        self.close.cancel();
    }

    fn is_alive(&self) -> bool {
        !self.close.is_cancelled() && self.death.borrow().is_none()
    }
}

/// Dropping it stops the process, so an abandoned start never leaves an agent behind.
pub(crate) struct AgentProcess {
    pub(crate) cx: ConnectionTo<acp::Agent>,
    pub(crate) info: AgentInfo,
    pub(crate) routes: Arc<Routes>,
    pub(crate) life: Life,
}

impl Drop for AgentProcess {
    fn drop(&mut self) {
        self.life.close();
    }
}

pub(crate) async fn launch(
    config: &AcpConfig,
    workdir: &Path,
    owner: Option<Owner>,
) -> Result<AgentProcess, AcpError> {
    let (
        leader,
        Pipes {
            stdin,
            stdout,
            stderr,
        },
    ) = proc::spawn(config, workdir)?;
    let routes = Arc::new(Routes::new(owner, config.permission_timeout));
    let stop = CancellationToken::new();
    let (connected, on_connect) = oneshot::channel();
    let transport = transport::lines(stdin, stdout, MAX_LINE_BYTES);
    let connection = tokio::spawn(connect(transport, routes.clone(), stop.clone(), connected));
    let close = CancellationToken::new();
    let (death_tx, death) = watch::channel(None);
    tokio::spawn(supervise(Supervised {
        leader,
        stderr,
        connection,
        stop,
        close: close.clone(),
        death: death_tx,
    }));
    let life = Life { close, death };
    let mut guard = CloseOnDrop(Some(life.clone()));
    let cx = tokio::select! {
        cx = on_connect => cx.map_err(|_| AcpError::AgentGone("the agent connection did not start".to_owned()))?,
        death = life.died() => return Err(death.error()),
    };
    let params = json!({
        "protocolVersion": wire::SUPPORTED_PROTOCOL,
        "clientInfo": {"name": "riggs", "title": "Riggs", "version": env!("CARGO_PKG_VERSION")},
        "clientCapabilities": {
            "fs": {"readTextFile": false, "writeTextFile": false},
            "terminal": false,
            "auth": {"terminal": false},
        },
    });
    let info = wire::initialized(call(&cx, &life, "initialize", params, &[]).await?)?;
    guard.0 = None;
    Ok(AgentProcess {
        cx,
        info,
        routes,
        life,
    })
}

struct CloseOnDrop(Option<Life>);

impl Drop for CloseOnDrop {
    fn drop(&mut self) {
        if let Some(life) = &self.0 {
            life.close();
        }
    }
}

impl AgentProcess {
    pub(crate) fn is_alive(&self) -> bool {
        self.life.is_alive()
    }

    pub(crate) async fn new_session(&self, cwd: &Path) -> Result<String, AcpError> {
        let params = json!({"cwd": cwd, "mcpServers": []});
        let session = wire::session_created(self.call("session/new", params).await?)?;
        self.routes.set_session(&session, false);
        Ok(session)
    }

    /// The agent replays the whole conversation before it answers; none of it is relayed.
    pub(crate) async fn load_session(&self, session: &str, cwd: &Path) -> Result<(), AcpError> {
        self.routes.set_session(session, true);
        let params = json!({"sessionId": session, "cwd": cwd, "mcpServers": []});
        let loaded = self.call("session/load", params).await;
        self.routes.restored();
        loaded.map(|_| ())
    }

    pub(crate) fn prompt(
        self: &Arc<Self>,
        turn: TurnHandle,
        blocks: Vec<Value>,
        cancel_grace: Duration,
    ) -> Result<(), AcpError> {
        let TurnHandle {
            events,
            cancelled,
            gate,
            ..
        } = turn;
        let opened = self.routes.open_turn(events, gate, cancelled.clone())?;
        let generation = opened.generation;
        let unwind = |err: String| {
            drop(self.routes.close_turn(generation));
            AcpError::AgentGone(err)
        };
        let request = UntypedMessage::new(
            "session/prompt",
            json!({"sessionId": opened.session, "prompt": blocks}),
        )
        .map_err(|err| unwind(err.to_string()))?;
        let (done, finished) = oneshot::channel();
        let routes = self.routes.clone();
        self.cx
            .send_request(request)
            .on_receiving_result(move |result| {
                let route = routes.close_turn(generation);
                let _ = done.send((route, result));
                async { Ok(()) }
            })
            .map_err(|err| unwind(err.to_string()))?;
        tokio::spawn(turn::run(TurnTask {
            agent: self.clone(),
            generation,
            finished,
            cancelled,
            interrupt: opened.interrupt,
            cancel_grace,
        }));
        Ok(())
    }

    pub(crate) async fn cancel(&self) {
        for (target, update) in self.routes.interrupt(&self.cx) {
            self.routes.deliver(&target, Out::Tool(update)).await;
        }
    }

    pub(crate) async fn close(&self) {
        self.cancel().await;
        self.routes.cancel_held();
        self.life.close();
        self.life.died().await;
    }

    async fn call(&self, method: &str, params: Value) -> Result<Value, AcpError> {
        call(
            &self.cx,
            &self.life,
            method,
            params,
            &self.info.auth_methods,
        )
        .await
    }
}

async fn call(
    cx: &ConnectionTo<acp::Agent>,
    life: &Life,
    method: &str,
    params: Value,
    auth: &[String],
) -> Result<Value, AcpError> {
    let request = UntypedMessage::new(method, params)
        .map_err(|err| AcpError::AgentGone(format!("could not send {method}: {err}")))?;
    let sent = cx.send_request(request);
    tokio::select! {
        response = sent.block_task() => match response {
            Ok(value) => Ok(value),
            Err(err) if lost_connection(&err) => Err(gone(life, &err).await),
            Err(err) => Err(AcpError::rpc(method, &err, auth)),
        },
        death = life.died() => Err(death.error()),
    }
}

pub(crate) fn lost_connection(err: &acp::Error) -> bool {
    acp::is_incoming_transport_closed(err) || err.message.contains("never received")
}

pub(crate) async fn gone(life: &Life, err: &acp::Error) -> AcpError {
    match timeout(EXIT_GRACE * 3, life.died()).await {
        Ok(death) => death.error(),
        Err(_) => AcpError::AgentGone(format!("the agent connection failed: {}", err.message)),
    }
}

async fn connect(
    transport: AgentLines,
    routes: Arc<Routes>,
    stop: CancellationToken,
    connected: oneshot::Sender<ConnectionTo<acp::Agent>>,
) -> Result<(), acp::Error> {
    let requests = routes.clone();
    acp::Client
        .builder()
        .name("riggs")
        .on_receive_request(
            async move |request: UntypedMessage,
                        responder: Responder<Value>,
                        cx: ConnectionTo<acp::Agent>| {
                Ok(requests.request(request, responder, &cx))
            },
            acp::on_receive_request!(),
        )
        .on_receive_notification(
            async move |notification: UntypedMessage, _cx: ConnectionTo<acp::Agent>| {
                routes.notification(notification).await;
                Ok(())
            },
            acp::on_receive_notification!(),
        )
        .connect_with(transport, async move |cx: ConnectionTo<acp::Agent>| {
            let _ = connected.send(cx.clone());
            tokio::select! {
                () = stop.cancelled() => {}
                () = cx.incoming_closed() => {}
            }
            Ok(())
        })
        .await
}

struct Supervised {
    leader: Leader,
    stderr: Tail,
    connection: JoinHandle<Result<(), acp::Error>>,
    stop: CancellationToken,
    close: CancellationToken,
    death: watch::Sender<Option<Death>>,
}

async fn supervise(supervised: Supervised) {
    let Supervised {
        mut leader,
        stderr,
        mut connection,
        stop,
        close,
        death,
    } = supervised;
    let pid = leader.pid();
    let ended = tokio::select! {
        status = leader.wait() => {
            let joined = timeout(EXIT_GRACE, &mut connection).await;
            let _ = timeout(EXIT_GRACE, stderr.closed()).await;
            let _ = leader.kill_tree(Descendants::default()).await;
            let exit = exited(status, &stderr);
            match joined {
                Ok(Ok(Err(err))) => {
                    Death::Failed(format!("the agent connection failed: {err}; {exit}"))
                }
                _ => Death::Failed(exit),
            }
        }
        joined = &mut connection => {
            let reason = match joined {
                Ok(Ok(())) => "the agent closed its output".to_owned(),
                Ok(Err(err)) => format!("the agent connection failed: {err}"),
                Err(err) => format!("the agent connection stopped: {err}"),
            };
            match timeout(EXIT_GRACE, leader.wait()).await {
                Ok(status) => {
                    let _ = timeout(EXIT_GRACE, stderr.closed()).await;
                    let _ = leader.kill_tree(Descendants::default()).await;
                    Death::Failed(format!("{reason}; {}", exited(status, &stderr)))
                }
                Err(_) => {
                    let _ = leader.kill_tree(Descendants::default()).await;
                    Death::Failed(with_stderr(reason, &stderr))
                }
            }
        }
        () = close.cancelled() => {
            let known = leader.descendants().await;
            stop.cancel();
            let _ = timeout(EXIT_GRACE, leader.wait()).await;
            let _ = leader.kill_tree(known).await;
            Death::Closed
        }
    };
    stop.cancel();
    connection.abort();
    if let Death::Failed(reason) = &ended {
        tracing::warn!(pid, %reason, "the agent process stopped");
    }
    death.send_replace(Some(ended));
}

fn exited(status: std::io::Result<ExitStatus>, stderr: &Tail) -> String {
    let status = match status {
        Ok(status) => status.to_string(),
        Err(err) => format!("status unknown: {err}"),
    };
    with_stderr(format!("the agent process exited ({status})"), stderr)
}

fn with_stderr(reason: String, stderr: &Tail) -> String {
    let tail = stderr.text();
    if tail.is_empty() {
        reason
    } else {
        format!("{reason}: {tail}")
    }
}
