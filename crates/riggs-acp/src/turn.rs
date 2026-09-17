use std::future::pending;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use agent_client_protocol as acp;
use rax::event::StopReason;
use riggs_node::{BackendError, BackendEvent};
use serde_json::Value;
use tokio::sync::oneshot;
use tokio::time::{Sleep, sleep};
use tokio_util::sync::CancellationToken;

use crate::agent::{self, AgentProcess, Death};
use crate::error::{AcpError, REQUEST_CANCELLED};
use crate::map;
use crate::routes::TurnRoute;
use crate::wire;

pub(crate) type Finished = (Option<TurnRoute>, Result<Value, acp::Error>);

pub(crate) struct TurnTask {
    pub(crate) agent: Arc<AgentProcess>,
    pub(crate) generation: u64,
    pub(crate) finished: oneshot::Receiver<Finished>,
    pub(crate) cancelled: CancellationToken,
    pub(crate) interrupt: CancellationToken,
    pub(crate) cancel_grace: Duration,
}

pub(crate) async fn run(task: TurnTask) {
    let TurnTask {
        agent,
        generation,
        mut finished,
        cancelled,
        interrupt,
        cancel_grace,
    } = task;
    let mut watchdog: Option<Pin<Box<Sleep>>> = None;
    let (route, event) = loop {
        tokio::select! {
            biased;
            done = &mut finished => break match done {
                Ok((route, result)) => (route, answered(&agent, result, &interrupt).await),
                Err(_) => {
                    let death = agent.life.died().await;
                    (agent.routes.close_turn(generation), died(&death, &interrupt))
                }
            },
            death = agent.life.died() => {
                break (agent.routes.close_turn(generation), died(&death, &interrupt));
            }
            () = cancelled.cancelled(), if !interrupt.is_cancelled() => agent.cancel().await,
            () = interrupt.cancelled(), if watchdog.is_none() => {
                watchdog = Some(Box::pin(sleep(cancel_grace)));
            }
            () = expiry(&mut watchdog) => {
                tracing::warn!(grace = ?cancel_grace, "the agent did not answer a cancelled turn in time; stopping it");
                agent.life.close();
                break (agent.routes.close_turn(generation), cancelled_turn());
            }
        }
    };
    let Some(route) = route else {
        return;
    };
    if route.events.send(event).await.is_err() {
        tracing::debug!("the turn stopped being delivered before it ended");
    }
}

async fn expiry(watchdog: &mut Option<Pin<Box<Sleep>>>) {
    match watchdog {
        Some(sleeping) => sleeping.await,
        None => pending().await,
    }
}

async fn answered(
    agent: &AgentProcess,
    result: Result<Value, acp::Error>,
    interrupt: &CancellationToken,
) -> BackendEvent {
    match result {
        Ok(value) => match wire::stop_reason(value) {
            Ok(reason) => BackendEvent::Complete(Some(map::stop_reason(&reason))),
            Err(err) => failed(err),
        },
        Err(err) if agent::lost_connection(&err) => died(&agent.life.died().await, interrupt),
        Err(err)
            if interrupt.is_cancelled() || i64::from(i32::from(err.code)) == REQUEST_CANCELLED =>
        {
            cancelled_turn()
        }
        Err(err) => failed(AcpError::rpc(
            "session/prompt",
            &err,
            &agent.info.auth_methods,
        )),
    }
}

fn died(death: &Death, interrupt: &CancellationToken) -> BackendEvent {
    match death {
        Death::Closed if interrupt.is_cancelled() => cancelled_turn(),
        death => failed(death.error()),
    }
}

fn cancelled_turn() -> BackendEvent {
    BackendEvent::Complete(Some(StopReason::Cancelled))
}

fn failed(err: AcpError) -> BackendEvent {
    BackendEvent::Error(BackendError::new(err))
}
