use std::path::PathBuf;
use std::process::ExitCode;

use riggs_node::SessionsConfig;

use crate::cli::{Cli, Command, InstallArgs, LaunchdCommand, RunArgs, VersionArgs};
use crate::config::{self, Config, ConfigError, Overrides};
use crate::dial::Dialer;
use crate::launchd::{self, Job, Launchctl, Plan};
use crate::logging::redact;
use crate::version::{self, GitHub, VERSION};
use crate::watch::Watch;
use crate::{agent, lock, logging, signals, token};

pub fn dispatch(cli: Cli) -> ExitCode {
    let config = cli.config;
    let alias = cli.alias;
    let result = match cli.command {
        Command::Run(args) => run(config, &alias, args),
        Command::Validate => validate(config, &alias),
        Command::Launchd(command) => launchd_command(config, &alias, command),
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

fn run(flag: Option<PathBuf>, alias: &str, args: RunArgs) -> Result<(), String> {
    let path = config_path(flag, alias)?;
    let overrides = Overrides {
        gateway: args.gateway,
        token_file: args.token_file,
        insecure_skip_verify: args.insecure_skip_verify,
    };
    let watched = overrides.clone();
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
        let server = agent::server(
            &config.agent,
            config.sessions.clone(),
            config.dir.join("files"),
            config.metadata.clone(),
        )
        .map_err(|err| err.to_string())?;
        tokio::spawn(
            Watch {
                path: config.path.clone(),
                overrides: watched,
                server: server.clone(),
                metadata: config.metadata.clone(),
            }
            .run(),
        );
        signals::install(server.shutdown_token())
            .map_err(|err| format!("cannot install signal handlers: {err}"))?;
        tracing::info!(gateways = ?config.gateways, agent = %config.agent.describe(), "riggs {VERSION} starting");
        let dialer = Dialer {
            server,
            endpoints: config.gateways.clone(),
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
    println!("gateway: {}", config.gateways.join(", then "));
    println!("agent: {}", config.agent.describe());
    println!("sessions: {sessions}");
    println!("tool_gate: {}", config.agent.tool_gate());
    if !config.metadata.is_empty() {
        let keys: Vec<&str> = config.metadata.keys().map(String::as_str).collect();
        println!("metadata: {}", keys.join(", "));
    }
}

fn validate(flag: Option<PathBuf>, alias: &str) -> Result<(), String> {
    let path = config_path(flag, alias)?;
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

fn launchd_command(
    flag: Option<PathBuf>,
    alias: &str,
    command: LaunchdCommand,
) -> Result<(), String> {
    launchd::ensure_macos().map_err(|err| err.to_string())?;
    let home =
        config::home().ok_or("HOME is not set, so there is no LaunchAgents folder to write to")?;
    let job = Job::new(alias, home).map_err(|err| err.to_string())?;
    if let LaunchdCommand::Install(args) = command {
        return install_launchd(flag, job, args);
    }
    // Only install reads the configuration; the rest address the job --alias names.
    if flag.is_some() {
        return Err(format!(
            "--config only applies to `riggs launchd install`; {} acts on the job --alias names",
            job.label()
        ));
    }
    if let LaunchdCommand::Status = command {
        let status = launchd::status(&job, &Launchctl).map_err(|err| err.to_string())?;
        println!("{status}");
        return Ok(());
    }
    let outcome = match command {
        LaunchdCommand::Install(_) | LaunchdCommand::Status => unreachable!("handled above"),
        LaunchdCommand::Uninstall => launchd::uninstall(&job, &Launchctl),
        LaunchdCommand::Start => launchd::start(&job, &Launchctl),
        LaunchdCommand::Stop => launchd::stop(&job, &Launchctl),
        LaunchdCommand::Restart(args) => launchd::restart(&job, &Launchctl, args.force),
    }
    .map_err(|err| err.to_string())?;
    println!("{}", outcome.message(&job.label()));
    Ok(())
}

fn install_launchd(flag: Option<PathBuf>, job: Job, args: InstallArgs) -> Result<(), String> {
    let config = config_path(flag, job.alias())?;
    let binary = match args.binary_path {
        Some(path) => config::absolute(&path),
        None => std::env::current_exe()
            .map_err(|err| format!("cannot find this riggs binary; pass --binary-path: {err}"))?,
    };
    let plan = Plan {
        job,
        binary,
        config,
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
            "Note: {} does not exist yet; create it before starting the job.",
            plan.config.display()
        );
    }
    println!(
        "Start it with: riggs launchd start{}",
        alias_flag(&plan.job)
    );
    Ok(())
}

fn alias_flag(job: &Job) -> String {
    if job.alias() == config::DEFAULT_ALIAS {
        String::new()
    } else {
        format!(" --alias {}", job.alias())
    }
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
