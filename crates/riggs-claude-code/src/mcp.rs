use std::path::{Path, PathBuf};
use std::sync::Arc;

use rax::attachment::MAX_ATTACHMENT_BYTES;
use rax::tool::ToolCatalogue;
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
    let params = message.get("params").cloned().unwrap_or(Value::Null);
    let method = text_of(&message, "method").unwrap_or_default().to_owned();
    let server = text_of(&request, "server_name")
        .unwrap_or_default()
        .to_owned();
    // Read fresh rather than cached at spawn: the catalogue belongs to the link, so a session that
    // resumed under another gateway must serve that gateway's tools, not the ones it started with.
    let catalogue = proc
        .gateway_tools()
        .filter(|catalogue| catalogue.namespace == server);
    let reply = if server == MCP_SERVER {
        match method.as_str() {
            "initialize" => Ok(initialize(&params, MCP_SERVER)),
            "tools/list" => Ok(json!({"tools": tools()})),
            "tools/call" => Ok(render(call(&proc, &params, turn).await)),
            "ping" => Ok(json!({})),
            _ => Err((-32601, "method not found".to_owned())),
        }
    } else if let Some(catalogue) = catalogue {
        match method.as_str() {
            "initialize" => Ok(initialize(&params, &catalogue.namespace)),
            "tools/list" => Ok(json!({"tools": published(&catalogue)})),
            "tools/call" => Ok(render(relay(&proc, &params).await)),
            "ping" => Ok(json!({})),
            _ => Err((-32601, "method not found".to_owned())),
        }
    } else {
        Err((-32602, "unknown MCP server".to_owned()))
    };
    let response = match reply {
        Ok(result) => json!({"jsonrpc": "2.0", "id": id, "result": result}),
        Err((code, message)) => {
            json!({"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}})
        }
    };
    proc.respond(&request_id, wire::mcp_answer(&request_id, response));
}

fn initialize(params: &Value, server: &str) -> Value {
    let version = text_of(params, "protocolVersion").unwrap_or(PROTOCOL_VERSION);
    json!({
        "protocolVersion": version,
        "capabilities": {"tools": {}},
        "serverInfo": {"name": server, "version": env!("CARGO_PKG_VERSION")},
    })
}

fn tools() -> Value {
    json!([
        {
            "name": "auth",
            "description": "Ask for credentials you do not have and WAIT until they are granted. Use it when a call failed for missing or expired authentication — never guess, retry blindly, or ask the person to run auth commands themselves. Pass `tool` as the capability DIRECTLY affected, as the person knows it (e.g. `gcp-mcp`, `postgres-mcp`), not the binary it shells out to (e.g. `gcloud`); name the binary only when you are running it yourself. The sign-in runs on this machine, and this machine's owner is sent a direct message to complete it — not whoever you are talking to. It returns an error if they decline, it times out, or it fails: treat any error as a hard stop and do not retry the original call.",
            "inputSchema": {
                "type": "object",
                "required": ["tool", "profile"],
                "properties": {
                    "tool": {"type": "string", "description": "The capability that needs authentication, as the person knows it."},
                    "profile": {
                        "type": "string",
                        "enum": crate::profile::NAMES,
                        "description": "Which sign-in to run: `claude-code` re-authenticates the Claude Code CLI this agent runs on; `gcloud` signs in the user credential; `gcloud-adc` writes the application-default credentials that client libraries and MCP servers read; `custom` runs the command you supply, which the owner must approve first.",
                    },
                    "command": {"type": "string", "description": "Only with `custom`: the command line to run. Rejected for the built-in profiles."},
                    "needs_code": {"type": "boolean", "description": "Only with `custom`: true when the flow ends by pasting a verification code back. Defaults to false."},
                },
            },
        },
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

/// The gateway's tools, as the agent sees them. Names stay bare: Claude Code prefixes them with
/// the server itself, so they arrive as `mcp__<namespace>__<name>`.
fn published(catalogue: &ToolCatalogue) -> Value {
    let tools: Vec<Value> = catalogue
        .tools
        .iter()
        .map(|tool| {
            json!({
                "name": tool.name,
                "description": tool.description,
                "inputSchema": tool.input_schema.clone().unwrap_or_else(|| json!({"type": "object"})),
            })
        })
        .collect();
    Value::Array(tools)
}

/// Hands one call to the gateway and waits. Every way this can fail is a refusal the agent reads,
/// never silence: a turn must not hang on a gateway that is not coming back.
async fn relay(proc: &Arc<Proc>, params: &Value) -> ToolResult {
    let name = text_of(params, "name").unwrap_or_default().to_owned();
    let arguments = params
        .get("arguments")
        .cloned()
        .unwrap_or_else(|| json!({}));
    let Some(host) = proc.ctx().host.get() else {
        return ToolResult::error(
            "This node is not attached to a gateway, so that tool cannot be reached. \
             Do not retry it; say so and ask how to proceed.",
        );
    };
    match host.tools.call(proc.key(), &name, arguments).await {
        Ok(outcome) if outcome.is_error => ToolResult::error(outcome.content),
        Ok(outcome) => ToolResult::text(outcome.content),
        Err(unreachable) => ToolResult::error(format!(
            "{name} did not run: {unreachable}. Do not retry it blindly — say so and ask how to \
             proceed."
        )),
    }
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
        Some("auth") => auth(proc, &args).await,
        Some("ask") => ask(&args, turn).await,
        Some("present_plan") => plan(&args, turn).await,
        Some("attach") => attach(proc, &args, turn).await,
        Some(other) => ToolResult::error(format!("Error: there is no tool named {other}")),
        None => ToolResult::error("Error: a tool name is required"),
    }
}

/// Runs a sign-in on this machine and waits for its owner to finish it.
async fn auth(proc: &Proc, args: &Value) -> ToolResult {
    let ctx = proc.ctx();
    let ask = match interaction::parse_auth(args, &ctx.config) {
        Ok(ask) => ask,
        Err(refused) => return refused,
    };
    let Some(repair) = ctx.repair.get().and_then(std::sync::Weak::upgrade) else {
        return ToolResult::error("Error: this machine cannot run a sign-in right now".to_owned());
    };
    let tool = ask.tool.clone();
    match repair.sign_in(ask).await {
        Ok(()) => ToolResult::text(format!(
            "The owner of this machine completed the sign-in for {tool}. Try the call again."
        )),
        Err(reason) => ToolResult::error(format!(
            "Error: the sign-in for {tool} did not complete: {reason}"
        )),
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

#[cfg(test)]
mod tests {
    use rax::tool::{ToolDef, ToolKind};

    use super::*;

    fn catalogue() -> ToolCatalogue {
        ToolCatalogue {
            namespace: "murtaugh".to_owned(),
            tools: vec![
                ToolDef {
                    name: "slack_read_message".to_owned(),
                    description: "Read a Slack message.".to_owned(),
                    input_schema: Some(json!({"type": "object", "required": ["link"]})),
                    kind: ToolKind::Read,
                },
                ToolDef {
                    name: "speak".to_owned(),
                    description: "Say it out loud.".to_owned(),
                    input_schema: None,
                    kind: ToolKind::Other,
                },
            ],
        }
    }

    /// Names stay bare. Claude Code prefixes them with the server itself, so prefixing here would
    /// publish `mcp__murtaugh__mcp__murtaugh__…`.
    #[test]
    fn published_tools_keep_the_names_the_gateway_gave_them() {
        let published = published(&catalogue());
        let names: Vec<&str> = published
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|tool| tool["name"].as_str())
            .collect();
        assert_eq!(names, vec!["slack_read_message", "speak"]);
        assert_eq!(published[0]["inputSchema"]["required"][0], "link");
    }

    /// A tool taking no arguments still needs a schema: Claude Code rejects a tool without one.
    #[test]
    fn a_tool_with_no_schema_is_published_as_taking_an_empty_object() {
        let published = published(&catalogue());
        assert_eq!(published[1]["inputSchema"], json!({"type": "object"}));
    }

    #[test]
    fn a_gateway_server_names_itself_in_its_initialize() {
        let answer = initialize(&json!({}), "murtaugh");
        assert_eq!(answer["serverInfo"]["name"], "murtaugh");
        assert_eq!(
            initialize(&json!({}), MCP_SERVER)["serverInfo"]["name"],
            "riggs"
        );
    }
}
