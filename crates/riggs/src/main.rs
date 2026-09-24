//! The Riggs daemon: loads the config, reads the node token, dials the gateway and serves the one
//! configured agent.

mod agent;
mod app;
mod cli;
mod config;
mod dial;
mod launchd;
mod lock;
mod logging;
mod signals;
mod token;
mod version;
mod watch;

use std::process::ExitCode;

use clap::Parser;

fn main() -> ExitCode {
    match cli::Cli::try_parse() {
        Ok(cli) => app::dispatch(cli),
        Err(err) => {
            let _ = err.print();
            ExitCode::from(u8::try_from(err.exit_code()).unwrap_or(2))
        }
    }
}
