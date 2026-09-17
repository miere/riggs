//! `claude auth status`, `claude auth login` and `claude --version`, with the output shapes of the
//! 2.1.271 binary.

use std::fs::{self, OpenOptions};
use std::io::{BufRead as _, Write as _};
use std::os::unix::fs::PermissionsExt as _;
use std::os::unix::process::CommandExt as _;
use std::path::{Path, PathBuf};

use serde_json::{Value, json};

use crate::script::{Auth, LoginStep};

pub const VERSION: &str = "2.1.271 (Claude Code)";

pub fn run(args: &[String], auth: &Auth, state: &Path) -> i32 {
    fs::create_dir_all(state).unwrap();
    match args.get(1).map(String::as_str) {
        Some("status") => status(args, auth, state),
        Some("login") => login(args, auth, state),
        _ => {
            eprintln!("error: unknown command '{}'", args.join(" "));
            1
        }
    }
}

fn log(state: &Path, mut entry: Value) {
    entry["pid"] = json!(std::process::id());
    let mut file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(state.join("log.jsonl"))
        .unwrap();
    let mut line = entry.to_string();
    line.push('\n');
    file.write_all(line.as_bytes()).unwrap();
}

fn status(args: &[String], auth: &Auth, state: &Path) -> i32 {
    log(state, json!({"event": "auth_status", "argv": args}));
    let printed = auth.status.clone().unwrap_or_else(|| {
        json!({
            "loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty",
            "analyticsDisabled": false, "email": "owner@example.com",
            "orgId": "00000000-0000-4000-8000-000000000001", "orgName": "Example",
            "subscriptionType": "max",
        })
    });
    println!("{}", serde_json::to_string_pretty(&printed).unwrap());
    if printed["loggedIn"] == true { 0 } else { 1 }
}

fn login(args: &[String], auth: &Auth, state: &Path) -> i32 {
    let runs = fs::read_to_string(state.join("log.jsonl"))
        .unwrap_or_default()
        .lines()
        .filter(|line| line.contains("\"auth_login\""))
        .count();
    let path = std::env::var("PATH").unwrap_or_default();
    let open = resolve(&path, "open");
    log(
        state,
        json!({
            "event": "auth_login", "argv": args, "run": runs,
            "path_head": path.split(':').next(), "browser": std::env::var("BROWSER").ok(),
            "open": open.as_ref().map(|open| open.display().to_string()),
            "open_is_stand_in": open.as_deref().is_some_and(stand_in),
        }),
    );
    let steps = auth
        .logins
        .get(runs.min(auth.logins.len().saturating_sub(1)))
        .cloned()
        .unwrap_or_default();
    let mut stdout = std::io::stdout();
    let mut stdin = std::io::stdin().lock();
    for step in steps {
        match step {
            LoginStep::Say(text) => {
                writeln!(stdout, "{text}").unwrap();
                stdout.flush().unwrap();
            }
            LoginStep::Prompt(text) => {
                write!(stdout, "{text}").unwrap();
                stdout.flush().unwrap();
            }
            LoginStep::Stderr(text) => eprintln!("{text}"),
            LoginStep::AwaitCode { accept } => loop {
                let mut line = String::new();
                if stdin.read_line(&mut line).unwrap_or(0) == 0 {
                    log(state, json!({"event": "auth_stdin_closed"}));
                    return 1;
                }
                let code = line.trim().to_owned();
                log(state, json!({"event": "auth_code", "code": code}));
                if !code.contains('#') {
                    eprintln!("Invalid code. Please make sure the full code was copied.");
                    continue;
                }
                if code == accept {
                    writeln!(stdout, "Login successful.").unwrap();
                    stdout.flush().unwrap();
                    return 0;
                }
                eprintln!("Login failed: Request failed with status code 400");
                return 1;
            },
            LoginStep::SpawnGrandchild(pidfile) => {
                let child = std::process::Command::new("/bin/sleep")
                    .arg("600")
                    .process_group(0)
                    .stdin(std::process::Stdio::null())
                    .stdout(std::process::Stdio::null())
                    .stderr(std::process::Stdio::null())
                    .spawn()
                    .unwrap();
                let pidfile = state.join(pidfile);
                fs::write(
                    &pidfile,
                    format!("{}\n{}\n", std::process::id(), child.id()),
                )
                .unwrap();
                log(state, json!({"event": "grandchild", "pid": child.id()}));
            }
            LoginStep::Hang => loop {
                std::thread::park();
            },
            LoginStep::Exit(code) => return code,
        }
    }
    eprintln!("Login failed: the script ended");
    1
}

fn resolve(path: &str, name: &str) -> Option<PathBuf> {
    path.split(':')
        .map(|dir| Path::new(dir).join(name))
        .find(|candidate| {
            fs::metadata(candidate)
                .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
        })
}

fn stand_in(open: &Path) -> bool {
    fs::read_to_string(open).is_ok_and(|script| script == "#!/bin/sh\nexit 0\n")
}
