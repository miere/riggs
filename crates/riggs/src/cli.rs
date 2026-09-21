use std::path::PathBuf;

use clap::{Args, Parser, Subcommand};

use crate::config::DEFAULT_ALIAS;

#[derive(Debug, Parser)]
#[command(
    long_about = None,
    name = "riggs",
    about = "Runs one agent on this machine and connects it to a RAX gateway",
    arg_required_else_help = true,
    disable_version_flag = true
)]
pub struct Cli {
    /// Configuration file [default: ~/.config/riggs/<alias>/riggs.toml]
    #[arg(long, global = true, value_name = "PATH")]
    pub config: Option<PathBuf>,
    /// Which riggs to act on: the launchd job riggs.<alias> and ~/.config/riggs/<alias>
    #[arg(long, global = true, default_value = DEFAULT_ALIAS, value_name = "NAME")]
    pub alias: String,
    #[command(subcommand)]
    pub command: Command,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Connect to the gateway and serve the agent until stopped
    Run(RunArgs),
    /// Check the configuration and the token file without connecting
    Validate,
    /// Manage the macOS LaunchAgent that keeps `riggs run` alive
    #[command(subcommand)]
    Launchd(LaunchdCommand),
    /// Print the version, or check GitHub for a newer release
    Version(VersionArgs),
}

#[derive(Debug, Subcommand)]
pub enum LaunchdCommand {
    /// Write the LaunchAgent for this alias
    Install(InstallArgs),
    /// Stop the job and delete its LaunchAgent
    Uninstall,
    /// Hand the job to launchd, which starts it and keeps it alive
    Start,
    /// Take the job off launchd, letting riggs shut down cleanly
    Stop,
    /// Shut riggs down cleanly and hand the job back to launchd
    Restart(RestartArgs),
    /// Say whether launchd has the job, whether it is running, and where its logs are
    Status,
}

#[derive(Debug, Args)]
pub struct RunArgs {
    /// Gateway address, replacing gateway.urls; repeat it to give fallbacks in order
    #[arg(long, value_name = "URL")]
    pub gateway: Vec<String>,
    /// Node token file, replacing gateway.token_file
    #[arg(long, value_name = "PATH")]
    pub token_file: Option<PathBuf>,
    /// Skip TLS certificate checks (not supported yet; riggs refuses to start with it)
    #[arg(long)]
    pub insecure_skip_verify: bool,
}

#[derive(Debug, Args)]
pub struct InstallArgs {
    /// The riggs binary launchd runs [default: this binary]
    #[arg(long, value_name = "PATH")]
    pub binary_path: Option<PathBuf>,
    /// Replace an existing plist
    #[arg(long)]
    pub update_existing: bool,
}

#[derive(Debug, Args)]
pub struct RestartArgs {
    /// Kill riggs instead of waiting for it to shut down cleanly
    #[arg(long)]
    pub force: bool,
}

#[derive(Debug, Args)]
pub struct VersionArgs {
    /// Ask GitHub whether a newer release exists; set GH_TOKEN, as the repository is private
    #[arg(long)]
    pub check: bool,
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

    use clap::CommandFactory;
    use clap::error::ErrorKind;

    use super::*;

    fn parse(args: &[&str]) -> Result<Cli, clap::Error> {
        Cli::try_parse_from(std::iter::once("riggs").chain(args.iter().copied()))
    }

    #[test]
    fn the_definition_is_consistent() {
        Cli::command().debug_assert();
    }

    #[test]
    fn bare_riggs_asks_for_a_command_with_a_usage_exit() {
        let err = parse(&[]).unwrap_err();
        assert_eq!(err.exit_code(), 2);
    }

    #[test]
    fn help_needs_no_configuration() {
        let err = parse(&["help"]).unwrap_err();
        assert_eq!(err.kind(), ErrorKind::DisplayHelp);
        assert_eq!(err.exit_code(), 0);
    }

    #[test]
    fn run_takes_its_flags_and_a_global_config() {
        let cli = parse(&[
            "run",
            "--config",
            "/tmp/r.toml",
            "--gateway",
            "wss://g",
            "--gateway",
            "wss://h",
            "--token-file",
            "t",
        ])
        .unwrap();
        assert_eq!(cli.config, Some(PathBuf::from("/tmp/r.toml")));
        let Command::Run(run) = cli.command else {
            panic!("expected run")
        };
        assert_eq!(run.gateway, ["wss://g", "wss://h"]);
        assert_eq!(run.token_file, Some(PathBuf::from("t")));
        assert!(!run.insecure_skip_verify);
    }

    #[test]
    fn config_given_twice_is_an_error() {
        assert!(parse(&["--config", "a", "--config", "b", "validate"]).is_err());
    }

    #[test]
    fn the_alias_defaults_and_is_global() {
        assert_eq!(parse(&["validate"]).unwrap().alias, "default");
        assert_eq!(
            parse(&["--alias", "work", "validate"]).unwrap().alias,
            "work"
        );
        assert_eq!(
            parse(&["launchd", "restart", "--alias", "work"])
                .unwrap()
                .alias,
            "work"
        );
    }

    #[test]
    fn launchd_install_takes_its_own_flags() {
        let Command::Launchd(LaunchdCommand::Install(args)) =
            parse(&["launchd", "install", "--update-existing"])
                .unwrap()
                .command
        else {
            panic!("expected launchd install")
        };
        assert!(args.update_existing);
    }

    #[test]
    fn a_restart_is_graceful_unless_forced() {
        for (args, forced) in [
            (&["launchd", "restart"][..], false),
            (&["launchd", "restart", "--force"][..], true),
        ] {
            let Command::Launchd(LaunchdCommand::Restart(restart)) = parse(args).unwrap().command
            else {
                panic!("expected launchd restart")
            };
            assert_eq!(restart.force, forced);
        }
    }

    #[test]
    fn launchd_on_its_own_asks_for_a_verb() {
        let err = parse(&["launchd"]).unwrap_err();
        assert_eq!(err.exit_code(), 2);
    }
}
