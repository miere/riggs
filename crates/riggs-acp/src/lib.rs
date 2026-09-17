//! Bridges RAX to one Agent Client Protocol agent. Each RAX session runs its own agent process,
//! and only the calls the agent asks permission for are put to the gateway.

mod agent;
mod backend;
mod config;
mod error;
mod map;
mod permission;
mod proc;
mod routes;
mod transport;
mod turn;
mod wire;

pub use backend::AcpBackend;
pub use config::{AcpConfig, CANCEL_GRACE_PERIOD, PERMISSION_TIMEOUT, STARTUP_TIMEOUT};
pub use error::AcpError;
