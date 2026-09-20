mod file;

use std::collections::BTreeMap;
use std::fmt;
use std::io;
use std::path::{Path, PathBuf};
use std::time::Duration;

use riggs_acp::AcpConfig;
use riggs_claude_code::{ClaudeCodeConfig, SandboxConfig, SandboxMode};
use riggs_node::{SESSION_RETENTION, SessionsConfig};

use crate::token::is_configured;

pub const FILE_NAME: &str = "riggs.toml";
pub const DEFAULT_ALIAS: &str = "default";
const TOKEN_FILE: &str = "node-token";
const SESSIONS_DIR: &str = "sessions";

#[derive(Debug, thiserror::Error)]
pub enum ConfigError {
    #[error("no configuration file at {path}; create it or pass --config PATH")]
    Missing { path: PathBuf },
    #[error("cannot read {path}: {source}")]
    Unreadable {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("{path} is not valid Riggs configuration: {message}")]
    Syntax { path: PathBuf, message: String },
    #[error("{path} has problems:\n{problems}")]
    Invalid {
        path: PathBuf,
        problems: Problems,
        /// Still known, so `validate` can check the token alongside the other problems.
        token_file: PathBuf,
    },
    #[error(
        "HOME is not set, so the default configuration path cannot be found; pass --config PATH"
    )]
    NoHome,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Problem {
    pub field: String,
    pub message: String,
}

/// All of them at once, so an operator fixes a config in one pass.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct Problems(pub Vec<Problem>);

impl Problems {
    fn add(&mut self, field: &str, message: impl Into<String>) {
        self.0.push(Problem {
            field: field.to_owned(),
            message: message.into(),
        });
    }
}

impl fmt::Display for Problems {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let lines: Vec<String> = self
            .0
            .iter()
            .map(|problem| format!("  - {}: {}", problem.field, problem.message))
            .collect();
        f.write_str(&lines.join("\n"))
    }
}

/// Command-line values that replace the file's, applied before validation.
#[derive(Debug, Default, Clone)]
pub struct Overrides {
    pub gateway: Vec<String>,
    pub token_file: Option<PathBuf>,
    pub insecure_skip_verify: bool,
}

#[derive(Debug, Clone)]
pub enum AgentConfig {
    /// Boxed: it carries the sandbox policy and dwarfs the ACP config beside it.
    ClaudeCode(Box<ClaudeCodeConfig>),
    Acp(AcpConfig),
}

impl AgentConfig {
    pub fn describe(&self) -> String {
        match self {
            Self::ClaudeCode(config) => format!("claude_code {}", config.command.display()),
            Self::Acp(config) => format!("acp {}", config.command),
        }
    }

    pub fn tool_gate(&self) -> &'static str {
        match self {
            Self::ClaudeCode(_) => "every_call",
            Self::Acp(_) => "permission_prompts",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LogFormat {
    Text,
    Json,
}

#[derive(Debug, Clone)]
pub struct LogConfig {
    pub level: tracing::Level,
    pub format: LogFormat,
}

#[derive(Debug, Clone)]
pub struct Config {
    pub path: PathBuf,
    pub dir: PathBuf,
    /// Already checked by `resolve_endpoint`, so a bad scheme fails at startup, not in a redial loop.
    /// Tried in order: a node stays on whichever answered last and moves on when it stops.
    pub gateways: Vec<String>,
    pub token_file: PathBuf,
    pub agent: AgentConfig,
    pub sessions: SessionsConfig,
    pub log: LogConfig,
}

pub fn default_path(alias: &str) -> Result<PathBuf, ConfigError> {
    let home = home().ok_or(ConfigError::NoHome)?;
    Ok(home.join(".config/riggs").join(alias).join(FILE_NAME))
}

pub fn home() -> Option<PathBuf> {
    std::env::var_os("HOME")
        .filter(|home| !home.is_empty())
        .map(PathBuf::from)
}

pub fn absolute(path: &Path) -> PathBuf {
    std::path::absolute(path).unwrap_or_else(|_| path.to_path_buf())
}

pub fn load(path: &Path, overrides: &Overrides) -> Result<Config, ConfigError> {
    let path = absolute(path);
    let text = match std::fs::read_to_string(&path) {
        Ok(text) => text,
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            return Err(ConfigError::Missing { path });
        }
        Err(source) => return Err(ConfigError::Unreadable { path, source }),
    };
    let parsed: file::File = toml::from_str(&text).map_err(|err| ConfigError::Syntax {
        path: path.clone(),
        message: err.to_string().trim().to_owned(),
    })?;
    let dir = path
        .parent()
        .map(Path::to_path_buf)
        .unwrap_or_else(|| PathBuf::from("/"));
    let mut problems = Problems::default();
    let token_file = token_file(&dir, &parsed.gateway, overrides);
    let config = build(
        &path,
        &dir,
        token_file.clone(),
        parsed,
        overrides,
        &mut problems,
    );
    match config {
        Some(config) if problems.0.is_empty() => Ok(config),
        _ => Err(ConfigError::Invalid {
            path,
            problems,
            token_file,
        }),
    }
}

fn token_file(dir: &Path, section: &file::Gateway, overrides: &Overrides) -> PathBuf {
    match (&overrides.token_file, &section.token_file) {
        (Some(flag), _) => absolute(&expand_home(flag)),
        (None, Some(raw)) if is_configured(raw) => resolve(dir, raw),
        (None, _) => dir.join(TOKEN_FILE),
    }
}

fn build(
    path: &Path,
    dir: &Path,
    token_file: PathBuf,
    file: file::File,
    overrides: &Overrides,
    problems: &mut Problems,
) -> Option<Config> {
    let gateway = gateway(&file.gateway, overrides, problems);
    let dotenv = file
        .env_file
        .as_deref()
        .and_then(|raw| dotenv(&resolve(dir, raw), problems));
    let agent = agent(
        dir,
        file.agent,
        dotenv.unwrap_or_default(),
        &token_file,
        problems,
    );
    let sessions = sessions(dir, &file.sessions, problems);
    let log = log(&file.log, problems);
    Some(Config {
        path: path.to_path_buf(),
        dir: dir.to_path_buf(),
        gateways: gateway?,
        token_file,
        agent: agent?,
        sessions: sessions?,
        log: log?,
    })
}

fn gateway(
    section: &file::Gateway,
    overrides: &Overrides,
    problems: &mut Problems,
) -> Option<Vec<String>> {
    if overrides.insecure_skip_verify || section.insecure_skip_verify == Some(true) {
        problems.add(
            "gateway.insecure_skip_verify",
            "is not supported: the RAX dialler always verifies the gateway's TLS certificate",
        );
    }
    let urls = match (&overrides.gateway, &section.urls) {
        (flags, _) if !flags.is_empty() => flags.clone(),
        (_, Some(urls)) => urls.clone(),
        (_, None) => Vec::new(),
    };
    let urls: Vec<&String> = urls.iter().filter(|url| is_configured(url)).collect();
    if urls.is_empty() {
        problems.add(
            "gateway.urls",
            "is not set; give the gateway address, such as [\"wss://gateway.example.com\"]",
        );
        return None;
    }
    let mut resolved = Some(Vec::new());
    for (index, url) in urls.into_iter().enumerate() {
        match rax::transport::resolve_endpoint(url) {
            Ok(endpoint) => {
                if let Some(resolved) = &mut resolved {
                    resolved.push(endpoint.to_string());
                }
            }
            Err(err) => {
                let reason = err.to_string();
                let reason = reason.strip_prefix("rax: ").unwrap_or(&reason);
                problems.add(&format!("gateway.urls[{index}]"), reason);
                resolved = None;
            }
        }
    }
    resolved
}

enum Kind {
    ClaudeCode,
    Acp,
}

/// The box the agent runs in. Its own credential is always denied, whatever the profile lists,
/// because a node that can read its token can pretend to be this machine.
fn sandbox(
    dir: &Path,
    section: file::Sandbox,
    token_file: &Path,
    problems: &mut Problems,
) -> SandboxConfig {
    let mode = match section.mode.as_deref().map(str::trim) {
        None | Some("off") => SandboxMode::Off,
        Some("seatbelt") if cfg!(target_os = "macos") => SandboxMode::Seatbelt,
        Some("seatbelt") => {
            problems.add(
                "agent.sandbox.mode",
                "\"seatbelt\" only works on macOS; use \"off\" here",
            );
            SandboxMode::Off
        }
        Some(other) => {
            problems.add(
                "agent.sandbox.mode",
                format!("{other:?} is not a sandbox; use \"seatbelt\" or \"off\""),
            );
            SandboxMode::Off
        }
    };
    let paths = |raw: Vec<String>| -> Vec<PathBuf> {
        raw.iter()
            .map(|path| resolve(dir, path))
            .collect::<Vec<_>>()
    };
    SandboxConfig {
        mode,
        write: paths(section.write),
        deny_read: section.deny_read.map(paths),
        node_token: Some(token_file.to_path_buf()),
    }
}

const CLAUDE_CODE_ONLY: &str = "only applies when agent.kind is \"claude_code\"";
const ACP_ONLY: &str = "only applies when agent.kind is \"acp\"";

fn agent(
    dir: &Path,
    section: file::Agent,
    dotenv: BTreeMap<String, String>,
    token_file: &Path,
    problems: &mut Problems,
) -> Option<AgentConfig> {
    let kind = match section.kind.as_deref().map(str::trim) {
        Some("claude_code") => Some(Kind::ClaudeCode),
        Some("acp") => Some(Kind::Acp),
        Some(other) if is_configured(other) => {
            problems.add(
                "agent.kind",
                format!("{other:?} is not a kind Riggs runs; use \"claude_code\" or \"acp\""),
            );
            None
        }
        _ => {
            problems.add("agent.kind", "is not set; use \"claude_code\" or \"acp\"");
            None
        }
    };
    let command = match section.command.as_deref() {
        Some(command) if is_configured(command) => Some(command_path(dir, command)),
        _ => {
            problems.add(
                "agent.command",
                "is not set; give the agent's executable, such as \"claude\"",
            );
            None
        }
    };
    let mut env = BTreeMap::new();
    for (key, value) in dotenv {
        if std::env::var_os(&key).is_none() {
            env.insert(key, value);
        }
    }
    for (key, value) in section.env {
        if key.trim().is_empty() || key.contains('=') {
            problems.add("agent.env", format!("{key:?} is not a valid variable name"));
            continue;
        }
        env.insert(key, value);
    }
    let workdir = section
        .workdir
        .as_deref()
        .map_or_else(|| dir.to_path_buf(), |raw| resolve(dir, raw));
    let Some(kind) = kind else {
        for (field, raw) in [
            ("agent.hook_timeout", &section.hook_timeout),
            ("agent.handshake_timeout", &section.handshake_timeout),
            ("agent.interrupt_grace", &section.interrupt_grace),
            ("agent.startup_timeout", &section.startup_timeout),
            ("agent.cancel_grace_period", &section.cancel_grace_period),
            ("agent.permission_timeout", &section.permission_timeout),
        ] {
            if let Some(raw) = raw {
                duration(field, raw, problems);
            }
        }
        return None;
    };
    let mut durations = Durations { problems };
    let config = match kind {
        Kind::ClaudeCode => {
            for (field, set) in [
                ("agent.interruptible", section.interruptible.is_some()),
                ("agent.startup_timeout", section.startup_timeout.is_some()),
                (
                    "agent.cancel_grace_period",
                    section.cancel_grace_period.is_some(),
                ),
                (
                    "agent.permission_timeout",
                    section.permission_timeout.is_some(),
                ),
            ] {
                if set {
                    durations.problems.add(field, ACP_ONLY);
                }
            }
            let mut config = ClaudeCodeConfig::new(workdir);
            config.args = section.args;
            config.env = env;
            config.model = section.model.filter(|model| is_configured(model));
            durations.set(
                "agent.hook_timeout",
                &section.hook_timeout,
                &mut config.hook_timeout,
            );
            durations.set(
                "agent.handshake_timeout",
                &section.handshake_timeout,
                &mut config.handshake_timeout,
            );
            durations.set(
                "agent.interrupt_grace",
                &section.interrupt_grace,
                &mut config.interrupt_grace,
            );
            config.command = command?;
            config.sandbox = sandbox(dir, section.sandbox, token_file, problems);
            AgentConfig::ClaudeCode(Box::new(config))
        }
        Kind::Acp => {
            for (field, set) in [
                ("agent.model", section.model.is_some()),
                ("agent.hook_timeout", section.hook_timeout.is_some()),
                (
                    "agent.handshake_timeout",
                    section.handshake_timeout.is_some(),
                ),
                ("agent.interrupt_grace", section.interrupt_grace.is_some()),
            ] {
                if set {
                    durations.problems.add(field, CLAUDE_CODE_ONLY);
                }
            }
            let mut config = AcpConfig::new(command?.to_string_lossy().into_owned());
            config.args = section.args;
            config.env = env
                .into_iter()
                .map(|(key, value)| (key, Some(value)))
                .collect();
            config.workdir = Some(workdir);
            config.interruptible = section.interruptible;
            durations.set(
                "agent.startup_timeout",
                &section.startup_timeout,
                &mut config.startup_timeout,
            );
            durations.set(
                "agent.cancel_grace_period",
                &section.cancel_grace_period,
                &mut config.cancel_grace_period,
            );
            durations.set(
                "agent.permission_timeout",
                &section.permission_timeout,
                &mut config.permission_timeout,
            );
            AgentConfig::Acp(config)
        }
    };
    Some(config)
}

struct Durations<'a> {
    problems: &'a mut Problems,
}

impl Durations<'_> {
    fn set(&mut self, field: &str, raw: &Option<String>, target: &mut Duration) {
        if let Some(parsed) = raw
            .as_deref()
            .and_then(|raw| duration(field, raw, self.problems))
        {
            *target = parsed;
        }
    }
}

fn duration(field: &str, raw: &str, problems: &mut Problems) -> Option<Duration> {
    match humantime::parse_duration(raw.trim()) {
        Ok(parsed) if !parsed.is_zero() => Some(parsed),
        Ok(_) => {
            problems.add(field, "must be longer than zero");
            None
        }
        Err(err) => {
            problems.add(
                field,
                format!("{raw:?} is not a duration ({err}); write it like \"30s\" or \"1h\""),
            );
            None
        }
    }
}

fn sessions(
    dir: &Path,
    section: &file::Sessions,
    problems: &mut Problems,
) -> Option<SessionsConfig> {
    let retain = match &section.retain {
        Some(raw) => duration("sessions.retain", raw, problems)?,
        None => SESSION_RETENTION,
    };
    let store = section
        .dir
        .as_deref()
        .filter(|raw| is_configured(raw))
        .map_or_else(|| dir.join(SESSIONS_DIR), |raw| resolve(dir, raw));
    if section.durable == Some(false) {
        return Some(SessionsConfig::Ephemeral);
    }
    if store == dir {
        problems.add(
            "sessions.dir",
            "must not be the configuration directory, which holds Riggs's own lock file",
        );
        return None;
    }
    Some(SessionsConfig::Durable { dir: store, retain })
}

fn log(section: &file::Log, problems: &mut Problems) -> Option<LogConfig> {
    let level = match section.level.as_deref().map(str::trim) {
        None | Some("info") => Some(tracing::Level::INFO),
        Some("trace") => Some(tracing::Level::TRACE),
        Some("debug") => Some(tracing::Level::DEBUG),
        Some("warn") => Some(tracing::Level::WARN),
        Some("error") => Some(tracing::Level::ERROR),
        Some(other) => {
            problems.add(
                "log.level",
                format!("{other:?} is not a level; use trace, debug, info, warn or error"),
            );
            None
        }
    };
    let format = match section.format.as_deref().map(str::trim) {
        None | Some("text") => Some(LogFormat::Text),
        Some("json") => Some(LogFormat::Json),
        Some(other) => {
            problems.add(
                "log.format",
                format!("{other:?} is not a format; use text or json"),
            );
            None
        }
    };
    Some(LogConfig {
        level: level?,
        format: format?,
    })
}

fn dotenv(path: &Path, problems: &mut Problems) -> Option<BTreeMap<String, String>> {
    let entries = match dotenvy::from_path_iter(path) {
        Ok(entries) => entries,
        Err(err) => {
            problems.add("env_file", format!("cannot read {}: {err}", path.display()));
            return None;
        }
    };
    let mut values = BTreeMap::new();
    for entry in entries {
        match entry {
            Ok((key, value)) => {
                values.insert(key, value);
            }
            Err(err) => {
                problems.add(
                    "env_file",
                    format!("{} is malformed: {err}", path.display()),
                );
                return None;
            }
        }
    }
    Some(values)
}

fn expand_home(raw: &Path) -> PathBuf {
    let Ok(rest) = raw.strip_prefix("~") else {
        return raw.to_path_buf();
    };
    match home() {
        Some(home) => home.join(rest),
        None => raw.to_path_buf(),
    }
}

fn resolve(dir: &Path, raw: &str) -> PathBuf {
    let expanded = expand_home(Path::new(raw.trim()));
    dir.join(expanded).components().collect()
}

fn command_path(dir: &Path, raw: &str) -> PathBuf {
    let raw = raw.trim();
    if raw.starts_with('~') || raw.contains('/') {
        resolve(dir, raw)
    } else {
        PathBuf::from(raw)
    }
}

#[cfg(test)]
mod tests;
