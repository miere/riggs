use std::sync::Arc;

use rax::ErrorKind;
use rax::event::{BackgroundEvent, StopReason};
use rax::id::RequestId;
use riggs_node::{BackendEvent, HostHandles, SessionKey, TurnSink};
use tokio::sync::{mpsc, oneshot};

use crate::error::classify;
use crate::process::Ctx;

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Route {
    Turn(RequestId),
    Background,
}

pub(crate) enum Emit {
    Begin {
        stream: RequestId,
        sink: TurnSink,
    },
    Event {
        route: Route,
        event: BackendEvent,
    },
    End {
        stream: RequestId,
        event: BackendEvent,
    },
    Abandon {
        stream: RequestId,
    },
    Flush(oneshot::Sender<()>),
}

struct Current {
    stream: RequestId,
    sink: Option<TurnSink>,
}

pub(crate) async fn run(ctx: Arc<Ctx>, key: SessionKey, mut queue: mpsc::UnboundedReceiver<Emit>) {
    let host = ctx.host.get();
    let mut current: Option<Current> = None;
    while let Some(item) = queue.recv().await {
        if let Emit::Event { event, .. } | Emit::End { event, .. } = &item {
            observe(&ctx, event);
        }
        match item {
            Emit::Begin { stream, sink } => {
                current = Some(Current {
                    stream,
                    sink: Some(sink),
                });
            }
            Emit::Event { route, event } => {
                let turn = match (&route, current.as_mut()) {
                    (Route::Turn(stream), Some(open)) if *stream == open.stream => Some(open),
                    _ => None,
                };
                match turn {
                    Some(open) => {
                        if let Some(sink) = &open.sink
                            && sink.send(event).await.is_err()
                        {
                            open.sink = None;
                        }
                    }
                    None => background(host, &key, event).await,
                }
            }
            Emit::End { stream, event } => match current.take() {
                Some(open) if open.stream == stream => {
                    if let Some(sink) = open.sink {
                        let _ = sink.send(event).await;
                    }
                }
                other => current = other,
            },
            Emit::Abandon { stream } => {
                if current.as_ref().is_some_and(|open| open.stream == stream) {
                    current = None;
                }
            }
            Emit::Flush(done) => {
                let _ = done.send(());
            }
        }
    }
}

fn observe(ctx: &Ctx, event: &BackendEvent) {
    match event {
        BackendEvent::Error(err) if classify(err) == ErrorKind::Credential => {
            ctx.health.degraded(&err.to_string());
        }
        BackendEvent::Complete(Some(stop)) if *stop != StopReason::Cancelled => {
            ctx.health.recovered();
        }
        _ => {}
    }
}

async fn background(host: Option<&HostHandles>, key: &SessionKey, event: BackendEvent) {
    let Some(host) = host else {
        return;
    };
    let sink = &host.background;
    let sent = match event {
        BackendEvent::Message(content) => {
            sink.send(key, BackgroundEvent::Message { content }).await
        }
        BackendEvent::Status(text) => sink.send(key, BackgroundEvent::Status { text }).await,
        BackendEvent::ToolCallUpdate(tool_call_update) => {
            let event = BackgroundEvent::ToolCallUpdate { tool_call_update };
            sink.send(key, event).await
        }
        BackendEvent::PlanUpdate(entries) => {
            sink.send(key, BackgroundEvent::PlanUpdate { entries })
                .await
        }
        BackendEvent::Attachment(source) => sink.attach(key, source).await,
        BackendEvent::Complete(stop_reason) => {
            sink.send(key, BackgroundEvent::Complete { stop_reason })
                .await
        }
        BackendEvent::Error(err) => {
            let error = rax::Error::new(classify(&err), err.to_string());
            sink.send(key, BackgroundEvent::Error { error }).await
        }
        BackendEvent::SignInSettled(_) => return,
    };
    if let Err(err) = sent {
        tracing::debug!(session_id = %key, error = %err, "a background event was not delivered");
    }
}
