//! Runs Claude Code as the agent behind a Riggs node: one CLI process per session, with every tool
//! call held at a PreToolUse hook until the gateway rules on it.

mod args;
mod backend;
mod config;
mod content;
mod emit;
mod error;
mod interaction;
mod mcp;
mod outcome;
mod process;
mod tool;
mod wire;

pub use backend::{BACKEND_NAME, ClaudeCode, ClaudeCodeRecord};
pub use config::{
    ClaudeCodeConfig, HANDSHAKE_TIMEOUT, HOOK_MARGIN, HOOK_TIMEOUT, INTERRUPT_GRACE, MAX_LINE_BYTES,
};
pub use error::{ClaudeCodeError, StderrTail};
