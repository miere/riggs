use std::path::{Path, PathBuf};
use std::sync::Arc;

use rax::attachment::MAX_ATTACHMENT_BYTES;
use riggs_node::{AttachmentMeta, AttachmentSource, BackendEvent};
use serde_json::{Value, json};

use crate::emit::Route;
use crate::interaction::{self, ToolResult};
use crate::process::{Proc, TurnCtl};
use crate::wire::{self, MCP_SERVER, text_of};

const PROTOCOL_VERSION: &str = "2025-11-25";

pub(crate) async fn serve(
    proc: Arc<Proc>,
    request_id: String,
    request: Value,
    turn: Option<TurnCtl>,
) {
    let message = request.get("message").cloned().unwrap_or(Value::Null);
    let id = message.get("id").filter(|id| !id.is_null()).cloned();
    let Some(id) = id else {
        proc.respond(&request_id, wire::success(&request_id, json!({})));
        return;
    };
    let reply = if text_of(&request, "server_name") != Some(MCP_SERVER) {
        Err((-32602, "unknown MCP server".to_owned()))
    } else {
        let params = message.get("params").cloned().unwrap_or(Value::Null);
        match text_of(&message, "method") {
            Some("initialize") => Ok(initialize(&params)),
            Some("tools/list") => Ok(json!({"tools": tools()})),
            Some("tools/call") => Ok(render(call(&proc, &params, turn).await)),
            Some("ping") => Ok(json!({})),
            _ => Err((-32601, "method not found".to_owned())),
        }
    };
    let response = match reply {
        Ok(result) => json!({"jsonrpc": "2.0", "id": id, "result": result}),
        Err((code, message)) => {
            json!({"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}})
        }
    };
    proc.respond(&request_id, wire::mcp_answer(&request_id, response));
}

fn initialize(params: &Value) -> Value {
    let version = text_of(params, "protocolVersion").unwrap_or(PROTOCOL_VERSION);
    json!({
        "protocolVersion": version,
        "capabilities": {"tools": {}},
        "serverInfo": {"name": MCP_SERVER, "version": env!("CARGO_PKG_VERSION")},
    })
}

fn tools() -> Value {
    json!([
        {
            "name": "ask",
            "description": "Ask the person you are working for one to four multiple-choice questions and wait for their answers. Use it whenever you need a decision from them.",
            "inputSchema": {
                "type": "object",
                "required": ["questions"],
                "properties": {"questions": {
                    "type": "array", "minItems": 1, "maxItems": 4,
                    "items": {
                        "type": "object",
                        "required": ["header", "question", "options"],
                        "properties": {
                            "header": {"type": "string", "maxLength": 12},
                            "question": {"type": "string"},
                            "multiSelect": {"type": "boolean"},
                            "options": {
                                "type": "array", "minItems": 2, "maxItems": 4,
                                "items": {
                                    "type": "object",
                                    "required": ["label"],
                                    "properties": {
                                        "label": {"type": "string"},
                                        "description": {"type": "string"},
                                    },
                                },
                            },
                        },
                    },
                }},
            },
        },
        {
            "name": "present_plan",
            "description": "Show the person a plan and wait until they approve it, ask for changes or cancel it. Do not start the work before they approve.",
            "inputSchema": {
                "type": "object",
                "required": ["plan"],
                "properties": {
                    "plan": {"type": "string", "description": "The plan, in Markdown."},
                    "title": {"type": "string"},
                },
            },
        },
        {
            "name": "attach",
            "description": "Attach a file from the working directory to your reply, so the person receives the file itself. It must be inside the working directory and at most 100 MiB.",
            "inputSchema": {
                "type": "object",
                "required": ["path"],
                "properties": {
                    "path": {"type": "string", "description": "Relative to the working directory, or absolute inside it."},
                    "title": {"type": "string"},
                    "comment": {"type": "string"},
                },
            },
        },
    ])
}

fn render(result: ToolResult) -> Value {
    let content = json!([{"type": "text", "text": result.text}]);
    if result.is_error {
        json!({"content": content, "isError": true})
    } else {
        json!({"content": content})
    }
}

async fn call(proc: &Proc, params: &Value, turn: Option<TurnCtl>) -> ToolResult {
    let args = params.get("arguments").cloned().unwrap_or(Value::Null);
    match text_of(params, "name") {
        Some("ask") => ask(&args, turn).await,
        Some("present_plan") => plan(&args, turn).await,
        Some("attach") => attach(proc, &args, turn).await,
        Some(other) => ToolResult::error(format!("Error: there is no tool named {other}")),
        None => ToolResult::error("Error: a tool name is required"),
    }
}

async fn ask(args: &Value, turn: Option<TurnCtl>) -> ToolResult {
    let ask = match interaction::parse_ask(args) {
        Ok(ask) => ask,
        Err(refused) => return refused,
    };
    let Some(turn) = turn else {
        return ToolResult::error("Error: there is no conversation to ask in");
    };
    let answer = turn
        .prompts
        .question(ask.request(interaction::prompt_id()))
        .await;
    ask.result(answer)
}

async fn plan(args: &Value, turn: Option<TurnCtl>) -> ToolResult {
    let (title, plan) = match interaction::parse_plan(args) {
        Ok(parsed) => parsed,
        Err(refused) => return refused,
    };
    let Some(turn) = turn else {
        return ToolResult::error("Error: there is no conversation to present a plan in");
    };
    let request = interaction::plan_request(interaction::prompt_id(), title, plan);
    interaction::plan_result(turn.prompts.plan(request).await)
}

async fn attach(proc: &Proc, args: &Value, turn: Option<TurnCtl>) -> ToolResult {
    let Some(path) = text_of(args, "path").filter(|path| !path.trim().is_empty()) else {
        return ToolResult::error("Error: a path is required");
    };
    let Some(turn) = turn else {
        return ToolResult::error(format!(
            "error: there is no conversation in progress to attach {path:?} to"
        ));
    };
    let workdir = match tokio::fs::canonicalize(proc.workdir()).await {
        Ok(workdir) => workdir,
        Err(err) => {
            return ToolResult::error(format!(
                "Error: the working directory cannot be read: {err}"
            ));
        }
    };
    let candidate = if Path::new(path).is_absolute() {
        PathBuf::from(path)
    } else {
        workdir.join(path)
    };
    let resolved = match tokio::fs::canonicalize(&candidate).await {
        Ok(resolved) => resolved,
        Err(err) => return ToolResult::error(format!("Error: {path} cannot be opened: {err}")),
    };
    if !resolved.starts_with(&workdir) {
        return ToolResult::error(format!(
            "Error: {path} is outside the working directory, so it cannot be attached"
        ));
    }
    let size = match tokio::fs::metadata(&resolved).await {
        Ok(meta) if meta.is_dir() => {
            return ToolResult::error(format!("Error: {path} is a directory"));
        }
        Ok(meta) if !meta.is_file() => {
            return ToolResult::error(format!("Error: {path} is not a regular file"));
        }
        Ok(meta) if meta.len() == 0 => {
            return ToolResult::error(format!("Error: {path} is empty"));
        }
        Ok(meta) if meta.len() > MAX_ATTACHMENT_BYTES => {
            return ToolResult::error(format!(
                "Error: {path} is larger than the 100 MiB attachment limit"
            ));
        }
        Ok(meta) => meta.len(),
        Err(err) => return ToolResult::error(format!("Error: {path} cannot be read: {err}")),
    };
    let file = match tokio::fs::File::open(&resolved).await {
        Ok(file) => file,
        Err(err) => return ToolResult::error(format!("Error: {path} cannot be opened: {err}")),
    };
    let filename = resolved
        .file_name()
        .map(|name| name.to_string_lossy().into_owned());
    let name = filename.clone().unwrap_or_else(|| path.to_owned());
    let meta = AttachmentMeta {
        mimetype: mimetype(&resolved).map(str::to_owned),
        filename,
        title: text_of(args, "title").map(str::to_owned),
        comment: text_of(args, "comment").map(str::to_owned),
    };
    let source = AttachmentSource {
        meta,
        size,
        reader: Box::new(file),
    };
    proc.emit(Route::Turn(turn.stream), BackendEvent::Attachment(source));
    ToolResult::text(format!("Attached {name} ({size} bytes) to your reply."))
}

fn mimetype(path: &Path) -> Option<&'static str> {
    let extension = path.extension()?.to_str()?.to_ascii_lowercase();
    Some(match extension.as_str() {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "pdf" => "application/pdf",
        "json" => "application/json",
        "zip" => "application/zip",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        "md" => "text/markdown",
        "txt" | "log" => "text/plain",
        _ => return None,
    })
}
