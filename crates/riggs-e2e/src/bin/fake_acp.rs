//! A scripted ACP agent for process-level tests. It speaks newline-delimited JSON-RPC on stdio
//! and follows the JSON script named by `FAKE_ACP_SCRIPT`.

#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::panic,
    clippy::zombie_processes
)]

use std::collections::{BTreeMap, VecDeque};
use std::fs::OpenOptions;
use std::io::{BufRead, Write};
use std::os::unix::process::CommandExt;
use std::sync::mpsc;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64;
use serde::Deserialize;
use serde_json::{Value, json};

const SCRIPT_ENV: &str = "FAKE_ACP_SCRIPT";

#[derive(Debug, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct Script {
    load_session: bool,
    image: bool,
    log: Option<String>,
    pid_file: Option<String>,
    grandchild_pid_file: Option<String>,
    ignore_eof: bool,
    new_session_error: Option<Value>,
    load_error: Option<Value>,
    load_hang: bool,
    replay: Vec<Value>,
    turns: BTreeMap<String, Vec<Value>>,
}

struct Agent {
    script: Script,
    incoming: mpsc::Receiver<Value>,
    pending: VecDeque<Value>,
    session: String,
    cancelled: bool,
    next_id: u64,
}

fn main() {
    let path = std::env::var(SCRIPT_ENV).expect("the script variable is not set");
    let script: Script =
        serde_json::from_str(&std::fs::read_to_string(&path).expect("read script"))
            .expect("parse script");
    if let Some(pid_file) = &script.pid_file {
        append(pid_file, &format!("{}\n", std::process::id()));
    }
    if let Some(pid_file) = &script.grandchild_pid_file {
        let grandchild = std::process::Command::new("sleep")
            .arg("600")
            .process_group(0)
            .spawn()
            .expect("spawn grandchild");
        append(pid_file, &format!("{}\n", grandchild.id()));
    }
    let (sender, incoming) = mpsc::channel();
    let log = script.log.clone();
    std::thread::spawn(move || {
        for line in std::io::stdin().lock().lines() {
            let Ok(line) = line else { break };
            if line.trim().is_empty() {
                continue;
            }
            let message: Value = serde_json::from_str(&line).expect("client sent bad JSON");
            if let Some(log) = &log {
                let entry = json!({"pid": std::process::id(), "message": message});
                append(log, &format!("{entry}\n"));
            }
            if sender.send(message).is_err() {
                break;
            }
        }
    });
    let mut agent = Agent {
        script,
        incoming,
        pending: VecDeque::new(),
        session: "sess-1".to_owned(),
        cancelled: false,
        next_id: 0,
    };
    agent.serve();
}

fn append(path: &str, text: &str) {
    let mut file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .expect("open log");
    file.write_all(text.as_bytes()).expect("write log");
}

fn send(message: &Value) {
    let mut out = std::io::stdout().lock();
    if writeln!(out, "{message}")
        .and_then(|()| out.flush())
        .is_err()
    {
        std::process::exit(0);
    }
}

impl Agent {
    fn next(&mut self) -> Value {
        match self.pending.pop_front() {
            Some(message) => message,
            None => self.recv(),
        }
    }

    fn recv(&self) -> Value {
        match self.incoming.recv() {
            Ok(message) => message,
            Err(_) if self.script.ignore_eof => loop {
                std::thread::park();
            },
            Err(_) => std::process::exit(0),
        }
    }

    fn serve(&mut self) {
        loop {
            let message = self.next();
            let method = message["method"].as_str().unwrap_or_default().to_owned();
            let id = message.get("id").cloned();
            match (method.as_str(), id) {
                ("initialize", Some(id)) => respond(
                    &id,
                    json!({
                        "protocolVersion": 1,
                        "agentCapabilities": {
                            "loadSession": self.script.load_session,
                            "promptCapabilities": {"image": self.script.image},
                        },
                        "authMethods": [{"id": "fake-login", "name": "Fake login"}],
                    }),
                ),
                ("session/new", Some(id)) => match &self.script.new_session_error {
                    Some(error) => fail(&id, error.clone()),
                    None => respond(&id, json!({"sessionId": self.session})),
                },
                ("session/load", Some(id)) => self.load(&id, &message["params"]),
                ("session/prompt", Some(id)) => self.turn(&id, &message["params"]),
                ("session/cancel", None) => self.cancelled = true,
                (_, Some(id)) => fail(&id, json!({"code": -32601, "message": "Method not found"})),
                _ => {}
            }
        }
    }

    fn load(&mut self, id: &Value, params: &Value) {
        if self.script.load_hang {
            loop {
                self.next();
            }
        }
        if let Some(error) = &self.script.load_error {
            return fail(id, error.clone());
        }
        self.session = params["sessionId"].as_str().unwrap_or_default().to_owned();
        for update in self.script.replay.clone() {
            self.update(update);
        }
        respond(id, Value::Null);
    }

    fn update(&self, update: Value) {
        send(&json!({
            "jsonrpc": "2.0",
            "method": "session/update",
            "params": {"sessionId": self.session, "update": update},
        }));
    }

    fn turn(&mut self, id: &Value, params: &Value) {
        self.cancelled = false;
        let text: String = params["prompt"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|block| block["text"].as_str())
            .collect::<Vec<_>>()
            .join("\n");
        let steps = self
            .script
            .turns
            .get(&text)
            .or_else(|| self.script.turns.get("*"))
            .cloned()
            .unwrap_or_default();
        let mut answered = false;
        for step in steps {
            let (name, arg) = step
                .as_object()
                .and_then(|step| step.iter().next())
                .map(|(name, arg)| (name.clone(), arg.clone()))
                .expect("a step is an object with one key");
            match name.as_str() {
                "say" => self.update(json!({
                    "sessionUpdate": "agent_message_chunk",
                    "content": {"type": "text", "text": arg},
                })),
                "update" => self.update(arg),
                "permission" => self.permission(&arg),
                "wait_cancel" => self.wait_cancel(),
                "stop" => {
                    respond(id, json!({"stopReason": arg}));
                    answered = true;
                }
                "hang" => return,
                "stderr" => {
                    eprintln!("{}", arg.as_str().unwrap_or_default());
                }
                "crash" => {
                    eprintln!("{}", arg["stderr"].as_str().unwrap_or_default());
                    std::process::exit(i32::try_from(arg["code"].as_i64().unwrap_or(1)).unwrap());
                }
                "blob" => {
                    let size = usize::try_from(arg.as_u64().unwrap()).unwrap();
                    let bytes: Vec<u8> = (0..size).map(|byte| (byte % 251) as u8).collect();
                    self.update(json!({
                        "sessionUpdate": "agent_message_chunk",
                        "content": {"type": "resource", "resource": {
                            "uri": "file:///report.bin",
                            "blob": BASE64.encode(&bytes),
                            "mimeType": "application/octet-stream",
                        }},
                    }));
                }
                "oversize" => {
                    let size = usize::try_from(arg.as_u64().unwrap()).unwrap();
                    self.update(json!({
                        "sessionUpdate": "agent_message_chunk",
                        "content": {"type": "text", "text": "x".repeat(size)},
                    }));
                }
                other => panic!("unknown step {other}"),
            }
        }
        if !answered {
            respond(id, json!({"stopReason": "end_turn"}));
        }
    }

    fn permission(&mut self, arg: &Value) {
        self.next_id += 1;
        let request_id = json!(format!("fake-{}", self.next_id));
        send(&json!({
            "jsonrpc": "2.0",
            "id": request_id,
            "method": "session/request_permission",
            "params": {
                "sessionId": self.session,
                "toolCall": arg["tool_call"],
                "options": arg["options"],
            },
        }));
        let response = loop {
            let message = self.recv();
            if message.get("method").is_none() && message.get("id") == Some(&request_id) {
                break message;
            }
            if message["method"] == "session/cancel" {
                self.cancelled = true;
            } else {
                self.pending.push_back(message);
            }
        };
        let outcome = &response["result"]["outcome"];
        let chosen = outcome["optionId"].as_str().unwrap_or_default();
        let allowed = outcome["outcome"] == "selected"
            && arg["options"]
                .as_array()
                .into_iter()
                .flatten()
                .any(|option| {
                    option["optionId"] == chosen
                        && option["kind"]
                            .as_str()
                            .unwrap_or_default()
                            .starts_with("allow")
                });
        if allowed && let Some(effect) = arg["effect"].as_str() {
            append(effect, "ran\n");
        }
    }

    fn wait_cancel(&mut self) {
        while !self.cancelled {
            let message = self.recv();
            if message["method"] == "session/cancel" {
                self.cancelled = true;
            } else {
                self.pending.push_back(message);
            }
        }
    }
}

fn respond(id: &Value, result: Value) {
    send(&json!({"jsonrpc": "2.0", "id": id, "result": result}));
}

fn fail(id: &Value, error: Value) {
    send(&json!({"jsonrpc": "2.0", "id": id, "error": error}));
}
