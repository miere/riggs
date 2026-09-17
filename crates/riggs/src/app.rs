use std::path::{Path, PathBuf};
use std::process::ExitCode;

use riggs_node::SessionsConfig;

use crate::cli::{Cli, Command, LaunchdArgs, RunArgs, VersionArgs};
use crate::config::{self, Config, ConfigError, DEFAULT_ALIAS, Overrides};
use crate::dial::Dialer;
use crate::launchd::{self, Plan};
use crate::logging::redact;
use crate::version::{self, GitHub, VERSION};
use crate::{agent, lock, logging, signals, token};

pub fn dispatch(cli: Cli) -> ExitCode {
    let config = cli.config;
    let result = match cli.command {
        Command::Run(args) => run(config, args),
        Command::Validate => validate(config),
        Command::Launchd(args) => install_launchd(config, args),
        Command::Version(args) => print_version(args),
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(message) => {
            eprintln!("riggs: {}", redact(&message));
            ExitCode::FAILURE
        }
    }
}

fn config_path(flag: Option<PathBuf>, alias: &str) -> Result<PathBuf, String> {
    match flag {
        Some(path) => Ok(config::absolute(&path)),
        None => config::default_path(alias).map_err(|err| err.to_string()),
    }
}

fn run(flag: Option<PathBuf>, args: RunArgs) -> Result<(), String> {
    let path = config_path(flag, DEFAULT_ALIAS)?;
    let overrides = Overrides {
        gateway: args.gateway,
        token_file: args.token_file,
        insecure_skip_verify: args.insecure_skip_verify,
    };
    let config = config::load(&path, &overrides).map_err(|err| err.to_string())?;
    logging::init(&config.log);
    let token = token::read(&config.token_file)
        .map_err(|err| format!("this node has no usable credential: {err}"))?;
    print_banner(&config);
    let _lock = lock::acquire(&config.dir).map_err(|err| err.to_string())?;
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|err| format!("cannot start the async runtime: {err}"))?;
    runtime.block_on(async move {
        let server = agent::server(&config.agent, config.sessions.clone())
            .map_err(|err| err.to_string())?;
        signals::install(server.shutdown_token())
            .map_err(|err| format!("cannot install signal handlers: {err}"))?;
        tracing::info!(gateway = %config.gateway, agent = %config.agent.describe(), "riggs {VERSION} starting");
        let dialer = Dialer {
            server,
            endpoint: config.gateway.clone(),
            token_file: config.token_file.clone(),
        };
        dialer.run(token).await.map_err(|err| err.to_string())?;
        tracing::info!("riggs stopped");
        Ok(())
    })
}

fn print_banner(config: &Config) {
    let sessions = match config.sessions {
        SessionsConfig::Durable { .. } => "durable",
        SessionsConfig::Ephemeral => "ephemeral",
    };
    println!("riggs {VERSION}");
    println!("config: {}", config.path.display());
    println!("node credential: {}", config.token_file.display());
    println!("gateway: {}", config.gateway);
    println!("agent: {}", config.agent.describe());
    println!("sessions: {sessions}");
    println!("tool_gate: {}", config.agent.tool_gate());
}

fn validate(flag: Option<PathBuf>) -> Result<(), String> {
    let path = config_path(flag, DEFAULT_ALIAS)?;
    match config::load(&path, &Overrides::default()) {
        Ok(config) => {
            token::read(&config.token_file).map_err(|err| {
                format!(
                    "{} has problems:\n  - gateway.token_file: {err}",
                    path.display()
                )
            })?;
            println!("{} is valid", config.path.display());
            print_banner(&config);
            Ok(())
        }
        Err(ConfigError::Invalid {
            path,
            problems,
            token_file,
        }) => {
            let mut message = ConfigError::Invalid {
                path,
                problems,
                token_file: token_file.clone(),
            }
            .to_string();
            if let Err(err) = token::read(&token_file) {
                message.push_str(&format!("\n  - gateway.token_file: {err}"));
            }
            Err(message)
        }
        Err(err) => Err(err.to_string()),
    }
}

fn install_launchd(flag: Option<PathBuf>, args: LaunchdArgs) -> Result<(), String> {
    launchd::ensure_macos().map_err(|err| err.to_string())?;
    let config = config_path(flag, &args.alias)?;
    let home =
        config::home().ok_or("HOME is not set, so there is no LaunchAgents folder to write to")?;
    let binary = match args.binary_path {
        Some(path) => config::absolute(&path),
        None => std::env::current_exe()
            .map_err(|err| format!("cannot find this riggs binary; pass --binary-path: {err}"))?,
    };
    let plan = Plan {
        alias: args.alias,
        binary,
        config,
        home,
    };
    let installed = launchd::install(&plan, args.update_existing).map_err(|err| err.to_string())?;
    let verb = if installed.replaced {
        "Replaced"
    } else {
        "Wrote"
    };
    println!(
        "{verb} {} (label {}).",
        installed.path.display(),
        installed.label
    );
    if !plan.config.exists() {
        println!(
            "Note: {} does not exist yet; create it before loading the job.",
            plan.config.display()
        );
    }
    println!(
        "Load it with: launchctl bootstrap gui/$(id -u) {}",
        quote(&installed.path)
    );
    Ok(())
}

fn quote(path: &Path) -> String {
    format!("'{}'", path.display().to_string().replace('\'', r"'\''"))
}

fn print_version(args: VersionArgs) -> Result<(), String> {
    if !args.check {
        println!("riggs {VERSION}");
        return Ok(());
    }
    let checked = version::check(VERSION, &GitHub).map_err(|err| err.to_string())?;
    println!("{checked}");
    Ok(())
}
