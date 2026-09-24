use std::collections::BTreeMap;

use serde::Deserialize;

/// The TOML as written. Every value is optional here so validation can name every missing field
/// at once instead of stopping at the first.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct File {
    #[serde(default)]
    pub gateway: Gateway,
    #[serde(default)]
    pub agent: Agent,
    #[serde(default)]
    pub sessions: Sessions,
    #[serde(default)]
    pub log: Log,
    pub env_file: Option<String>,
    /// Forwarded to the gateway unread, so any shape is accepted here.
    #[serde(default)]
    pub metadata: toml::Table,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Gateway {
    pub urls: Option<Vec<String>>,
    pub token_file: Option<String>,
    pub insecure_skip_verify: Option<bool>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Agent {
    #[serde(default)]
    pub sandbox: Sandbox,
    pub kind: Option<String>,
    pub command: Option<String>,
    #[serde(default)]
    pub args: Vec<String>,
    #[serde(default)]
    pub env: BTreeMap<String, String>,
    pub workdir: Option<String>,
    pub model: Option<String>,
    pub hook_timeout: Option<String>,
    pub handshake_timeout: Option<String>,
    pub interrupt_grace: Option<String>,
    pub interruptible: Option<bool>,
    pub startup_timeout: Option<String>,
    pub cancel_grace_period: Option<String>,
    pub permission_timeout: Option<String>,
}

/// How the agent is confined while it runs.
#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Sandbox {
    pub mode: Option<String>,
    #[serde(default)]
    pub write: Vec<String>,
    pub deny_read: Option<Vec<String>>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Sessions {
    pub durable: Option<bool>,
    pub dir: Option<String>,
    pub retain: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Log {
    pub level: Option<String>,
    pub format: Option<String>,
}
