//! Runs Claude Code as the agent behind a Riggs node: one CLI process per session, with every tool
//! call held at a PreToolUse hook until the gateway rules on it.

mod args;
mod backend;
mod config;
mod content;
mod emit;
mod error;
mod health;
mod interaction;
mod login;
mod mcp;
mod outcome;
mod process;
mod profile;
mod repair;
mod sign_in;
mod tool;
mod wire;

pub use backend::{BACKEND_NAME, ClaudeCode, ClaudeCodeRecord};
pub use config::{
    ClaudeCodeConfig, HANDSHAKE_TIMEOUT, HOOK_MARGIN, HOOK_TIMEOUT, INTERRUPT_GRACE,
    MAX_LINE_BYTES, SIGN_IN_CONFIRM_WAIT, SIGN_IN_COOLDOWN, SIGN_IN_DRAIN, SIGN_IN_EXPIRY,
    SIGN_IN_LINK_WAIT, SIGN_IN_SHOW_WAIT, STATUS_TIMEOUT, SignInConfig,
};
pub use error::{ClaudeCodeError, StderrTail};
