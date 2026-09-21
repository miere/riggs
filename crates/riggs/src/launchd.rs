use std::fmt;
use std::fs::{self, OpenOptions, Permissions};
use std::io::{self, Write};
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::process::Command;

const LABEL_PREFIX: &str = "riggs.";
const PATH_DIRS: [&str; 6] = [
    "/opt/homebrew/bin",
    "/usr/local/bin",
    "/usr/bin",
    "/bin",
    "/usr/sbin",
    "/sbin",
];

#[derive(Debug, thiserror::Error)]
pub enum LaunchdError {
    #[error(
        "riggs launchd is macOS only; this is {os}. Run riggs under whatever supervisor this system uses"
    )]
    NotMacos { os: &'static str },
    #[error(
        "alias {0:?} cannot be used in a launchd label; use letters, digits, dots, dashes or underscores"
    )]
    Alias(String),
    #[error(
        "{path} already exists (label {label}): pass --update-existing to replace it. It may be running a live daemon, so this command will not overwrite one by accident"
    )]
    Exists { path: PathBuf, label: String },
    #[error("no LaunchAgent at {path}; run `riggs launchd install` first")]
    NotInstalled { path: PathBuf },
    #[error("{path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("cannot run launchctl: {0}")]
    Spawn(#[source] io::Error),
    #[error("launchctl {verb} failed for {label}: {detail}")]
    Launchctl {
        verb: String,
        label: String,
        detail: String,
    },
    #[error("plutil rejected {path}: {output}")]
    Lint { path: PathBuf, output: String },
}

/// What launchctl said. `detail` stands in for stderr when launchctl says nothing.
#[derive(Debug, Clone)]
pub struct Ran {
    pub ok: bool,
    pub out: String,
    pub detail: String,
}

pub trait Control {
    fn run(&self, args: &[&str]) -> io::Result<Ran>;
}

pub struct Launchctl;

impl Control for Launchctl {
    fn run(&self, args: &[&str]) -> io::Result<Ran> {
        let output = Command::new("launchctl").args(args).output()?;
        let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
        let detail = if stderr.is_empty() {
            match output.status.code() {
                Some(code) => format!("launchctl exited {code}"),
                None => "launchctl was killed by a signal".to_owned(),
            }
        } else {
            stderr
        };
        Ok(Ran {
            ok: output.status.success(),
            out: String::from_utf8_lossy(&output.stdout).into_owned(),
            detail,
        })
    }
}

/// One installed daemon, named by its alias. Enough to address the job; not enough to write it.
#[derive(Debug, Clone)]
pub struct Job {
    alias: String,
    home: PathBuf,
}

impl Job {
    pub fn new(alias: &str, home: PathBuf) -> Result<Self, LaunchdError> {
        let valid = !alias.is_empty()
            && alias
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'));
        if !valid {
            return Err(LaunchdError::Alias(alias.to_owned()));
        }
        Ok(Self {
            alias: alias.to_owned(),
            home,
        })
    }

    pub fn alias(&self) -> &str {
        &self.alias
    }

    pub fn label(&self) -> String {
        format!("{LABEL_PREFIX}{}", self.alias)
    }

    pub fn path(&self) -> PathBuf {
        self.home
            .join("Library/LaunchAgents")
            .join(format!("{}.plist", self.label()))
    }

    fn log_dir(&self) -> PathBuf {
        self.home.join("Library/Logs/riggs")
    }

    pub fn out_log(&self) -> PathBuf {
        self.log_dir().join(format!("{}.out.log", self.label()))
    }

    pub fn err_log(&self) -> PathBuf {
        self.log_dir().join(format!("{}.err.log", self.label()))
    }

    /// The GUI domain of whoever owns the home these LaunchAgents live in.
    fn domain(&self) -> Result<String, LaunchdError> {
        let uid = fs::metadata(&self.home)
            .map_err(io_error(&self.home))?
            .uid();
        Ok(format!("gui/{uid}"))
    }

    fn target(&self) -> Result<String, LaunchdError> {
        Ok(format!("{}/{}", self.domain()?, self.label()))
    }
}

#[derive(Debug, Clone)]
pub struct Plan {
    pub job: Job,
    pub binary: PathBuf,
    pub config: PathBuf,
}

#[derive(Debug)]
pub struct Installed {
    pub path: PathBuf,
    pub label: String,
    pub replaced: bool,
}

#[derive(Debug, PartialEq, Eq)]
pub enum Outcome {
    Started,
    AlreadyRunning,
    Stopped,
    AlreadyStopped,
    Restarted { forced: bool },
    Uninstalled { was_running: bool },
}

impl Outcome {
    pub fn message(&self, label: &str) -> String {
        match self {
            Self::Started => format!("Started {label}."),
            Self::AlreadyRunning => format!("{label} is already running."),
            Self::Stopped => format!("Stopped {label}."),
            Self::AlreadyStopped => format!("{label} is not running."),
            Self::Restarted { forced: false } => {
                format!("Restarted {label}, letting it shut down cleanly first.")
            }
            Self::Restarted { forced: true } => {
                format!("Restarted {label}, killing the running process.")
            }
            Self::Uninstalled { was_running: true } => {
                format!("Stopped {label} and removed its LaunchAgent.")
            }
            Self::Uninstalled { was_running: false } => {
                format!("Removed the LaunchAgent for {label}.")
            }
        }
    }
}

/// What launchd currently thinks of the job, which is the thing `launchctl print` buries.
#[derive(Debug, PartialEq, Eq)]
pub struct Status {
    pub label: String,
    pub plist: PathBuf,
    pub installed: bool,
    pub loaded: bool,
    pub pid: Option<u32>,
    pub last_exit: Option<i32>,
    pub out_log: PathBuf,
    pub err_log: PathBuf,
}

impl fmt::Display for Status {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if !self.installed {
            return write!(
                f,
                "{} is not installed.\n  LaunchAgent: {} (missing)",
                self.label,
                self.plist.display()
            );
        }
        let headline = match (self.loaded, self.pid) {
            (false, _) => format!(
                "{} is installed but not loaded; `riggs launchd start` hands it to launchd.",
                self.label
            ),
            (true, Some(pid)) => format!("{} is running (pid {pid}).", self.label),
            (true, None) => format!("{} is loaded but not running right now.", self.label),
        };
        writeln!(f, "{headline}")?;
        writeln!(f, "  LaunchAgent: {}", self.plist.display())?;
        if let Some(code) = self.last_exit {
            writeln!(f, "  last exit:   {code}")?;
        }
        writeln!(f, "  out log:     {}", self.out_log.display())?;
        write!(f, "  err log:     {}", self.err_log.display())
    }
}

pub fn status(job: &Job, ctl: &dyn Control) -> Result<Status, LaunchdError> {
    let plist = job.path();
    let installed = plist.exists();
    let mut status = Status {
        label: job.label(),
        plist,
        installed,
        loaded: false,
        pid: None,
        last_exit: None,
        out_log: job.out_log(),
        err_log: job.err_log(),
    };
    if !installed {
        return Ok(status);
    }
    let target = job.target()?;
    let ran = ctl.run(&["print", &target]).map_err(LaunchdError::Spawn)?;
    if !ran.ok {
        return Ok(status);
    }
    status.loaded = true;
    status.pid = field(&ran.out, "pid").and_then(|value| value.parse().ok());
    status.last_exit = ["last exit code", "last exit status"]
        .iter()
        .find_map(|key| field(&ran.out, key))
        .and_then(|value| value.parse().ok());
    Ok(status)
}

/// `launchctl print` is a nested dump; take the first `key = value` at any depth.
fn field<'a>(out: &'a str, key: &str) -> Option<&'a str> {
    out.lines().find_map(|line| {
        let (name, value) = line.split_once('=')?;
        (name.trim() == key).then(|| value.trim())
    })
}

pub fn ensure_macos() -> Result<(), LaunchdError> {
    if cfg!(target_os = "macos") {
        Ok(())
    } else {
        Err(LaunchdError::NotMacos {
            os: std::env::consts::OS,
        })
    }
}

impl Plan {
    pub fn plist(&self) -> String {
        let label = self.job.label();
        let home = &self.job.home;
        let mut path = vec![home.join(".local/bin").display().to_string()];
        path.extend(PATH_DIRS.iter().map(|dir| (*dir).to_owned()));
        let args = [
            self.binary.display().to_string(),
            "--config".to_owned(),
            self.config.display().to_string(),
            "run".to_owned(),
        ]
        .iter()
        .map(|arg| format!("\t\t<string>{}</string>\n", escape(arg)))
        .collect::<String>();
        format!(
            r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{label}</string>
	<key>ProgramArguments</key>
	<array>
{args}	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>{home}</string>
	<key>StandardOutPath</key>
	<string>{out}</string>
	<key>StandardErrorPath</key>
	<string>{err}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{path}</string>
	</dict>
</dict>
</plist>
"#,
            label = escape(&label),
            home = escape(&home.display().to_string()),
            out = escape(&self.job.out_log().display().to_string()),
            err = escape(&self.job.err_log().display().to_string()),
            path = escape(&path.join(":")),
        )
    }
}

pub fn install(plan: &Plan, update_existing: bool) -> Result<Installed, LaunchdError> {
    let path = plan.job.path();
    let replaced = fs::symlink_metadata(&path).is_ok();
    if replaced && !update_existing {
        return Err(LaunchdError::Exists {
            path,
            label: plan.job.label(),
        });
    }
    for dir in [
        path.parent().map(Path::to_path_buf),
        Some(plan.job.log_dir()),
    ]
    .into_iter()
    .flatten()
    {
        fs::DirBuilder::new()
            .recursive(true)
            .mode(0o755)
            .create(&dir)
            .map_err(io_error(&dir))?;
    }
    let staged = path.with_extension("plist.tmp");
    write(&staged, plan.plist().as_bytes())?;
    fs::rename(&staged, &path).map_err(io_error(&path))?;
    lint(&path)?;
    Ok(Installed {
        path,
        label: plan.job.label(),
        replaced,
    })
}

pub fn uninstall(job: &Job, ctl: &dyn Control) -> Result<Outcome, LaunchdError> {
    let path = require_installed(job)?;
    let was_running = running(job, ctl)?;
    if was_running {
        bootout(job, ctl)?;
    }
    fs::remove_file(&path).map_err(io_error(&path))?;
    Ok(Outcome::Uninstalled { was_running })
}

pub fn start(job: &Job, ctl: &dyn Control) -> Result<Outcome, LaunchdError> {
    require_installed(job)?;
    if running(job, ctl)? {
        return Ok(Outcome::AlreadyRunning);
    }
    bootstrap(job, ctl)?;
    Ok(Outcome::Started)
}

/// `bootout` is the graceful one: launchd sends SIGTERM and waits out ExitTimeOut before killing.
pub fn stop(job: &Job, ctl: &dyn Control) -> Result<Outcome, LaunchdError> {
    require_installed(job)?;
    if !running(job, ctl)? {
        return Ok(Outcome::AlreadyStopped);
    }
    bootout(job, ctl)?;
    Ok(Outcome::Stopped)
}

pub fn restart(job: &Job, ctl: &dyn Control, force: bool) -> Result<Outcome, LaunchdError> {
    require_installed(job)?;
    if !running(job, ctl)? {
        bootstrap(job, ctl)?;
        return Ok(Outcome::Started);
    }
    if force {
        // kickstart -k kills the running process outright, so nothing gets to drain.
        let target = job.target()?;
        check(ctl.run(&["kickstart", "-k", &target]), "kickstart", job)?;
    } else {
        bootout(job, ctl)?;
        bootstrap(job, ctl)?;
    }
    Ok(Outcome::Restarted { forced: force })
}

fn require_installed(job: &Job) -> Result<PathBuf, LaunchdError> {
    let path = job.path();
    if path.exists() {
        Ok(path)
    } else {
        Err(LaunchdError::NotInstalled { path })
    }
}

fn running(job: &Job, ctl: &dyn Control) -> Result<bool, LaunchdError> {
    let target = job.target()?;
    Ok(ctl
        .run(&["print", &target])
        .map_err(LaunchdError::Spawn)?
        .ok)
}

/// bootstrap registers the job but does not reliably honour RunAtLoad, so kickstart forces the
/// first run.
fn bootstrap(job: &Job, ctl: &dyn Control) -> Result<(), LaunchdError> {
    let domain = job.domain()?;
    let plist = job.path().display().to_string();
    let target = job.target()?;
    check(ctl.run(&["bootstrap", &domain, &plist]), "bootstrap", job)?;
    check(ctl.run(&["kickstart", &target]), "kickstart", job)
}

fn bootout(job: &Job, ctl: &dyn Control) -> Result<(), LaunchdError> {
    let domain = job.domain()?;
    let plist = job.path().display().to_string();
    check(ctl.run(&["bootout", &domain, &plist]), "bootout", job)
}

fn check(ran: io::Result<Ran>, verb: &str, job: &Job) -> Result<(), LaunchdError> {
    let ran = ran.map_err(LaunchdError::Spawn)?;
    if ran.ok {
        return Ok(());
    }
    Err(LaunchdError::Launchctl {
        verb: verb.to_owned(),
        label: job.label(),
        detail: ran.detail,
    })
}

fn write(path: &Path, bytes: &[u8]) -> Result<(), LaunchdError> {
    let mut file = OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o644)
        .open(path)
        .map_err(io_error(path))?;
    file.write_all(bytes).map_err(io_error(path))?;
    fs::set_permissions(path, Permissions::from_mode(0o644)).map_err(io_error(path))
}

fn lint(path: &Path) -> Result<(), LaunchdError> {
    let output = match Command::new("plutil").arg("-lint").arg(path).output() {
        Ok(output) => output,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(source) => {
            return Err(LaunchdError::Io {
                path: path.to_path_buf(),
                source,
            });
        }
    };
    if output.status.success() {
        return Ok(());
    }
    Err(LaunchdError::Lint {
        path: path.to_path_buf(),
        output: String::from_utf8_lossy(&output.stdout).trim().to_owned(),
    })
}

fn io_error(path: &Path) -> impl FnOnce(io::Error) -> LaunchdError {
    let path = path.to_path_buf();
    move |source| LaunchdError::Io { path, source }
}

fn escape(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

    use std::cell::RefCell;

    use super::*;

    #[derive(Default)]
    struct Fake {
        running: bool,
        out: String,
        fails: Option<String>,
        calls: RefCell<Vec<String>>,
    }

    impl Fake {
        fn running() -> Self {
            Self {
                running: true,
                ..Self::default()
            }
        }

        /// A running job, with launchctl print dumping what the real one dumps.
        fn printing(out: &str) -> Self {
            Self {
                running: true,
                out: out.to_owned(),
                ..Self::default()
            }
        }

        /// launchctl verbs, with the uid-bearing domain reduced to the verb's shape.
        fn verbs(&self) -> Vec<String> {
            self.calls
                .borrow()
                .iter()
                .map(|call| {
                    call.split_whitespace()
                        .filter(|word| !word.starts_with("gui/") && !word.starts_with('/'))
                        .collect::<Vec<_>>()
                        .join(" ")
                })
                .collect()
        }
    }

    impl Control for Fake {
        fn run(&self, args: &[&str]) -> io::Result<Ran> {
            self.calls.borrow_mut().push(args.join(" "));
            if args[0] == "print" {
                return Ok(Ran {
                    ok: self.running,
                    out: self.out.clone(),
                    detail: String::new(),
                });
            }
            match &self.fails {
                Some(detail) if detail.starts_with(args[0]) => Ok(Ran {
                    ok: false,
                    out: String::new(),
                    detail: detail.clone(),
                }),
                _ => Ok(Ran {
                    ok: true,
                    out: String::new(),
                    detail: String::new(),
                }),
            }
        }
    }

    fn job(home: &Path) -> Job {
        Job::new("work", home.to_path_buf()).unwrap()
    }

    fn plan(home: &Path) -> Plan {
        Plan {
            job: job(home),
            binary: PathBuf::from("/opt/riggs/bin/riggs"),
            config: PathBuf::from("/Users/me/.config/riggs/work/riggs.toml"),
        }
    }

    /// Every verb but install needs a plist on disk to address.
    fn installed(home: &Path) -> Job {
        install(&plan(home), false).unwrap();
        job(home)
    }

    #[test]
    fn the_plist_runs_riggs_with_its_config_and_keeps_it_alive() {
        let home = tempfile::tempdir().unwrap();
        let plan = plan(home.path());
        let installed = install(&plan, false).unwrap();
        assert_eq!(installed.label, "riggs.work");
        assert_eq!(
            installed.path,
            home.path().join("Library/LaunchAgents/riggs.work.plist")
        );
        assert!(!installed.replaced);
        let mode = fs::metadata(&installed.path).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o644);
        let plist = fs::read_to_string(&installed.path).unwrap();
        assert!(plist.contains(
            "<string>/opt/riggs/bin/riggs</string>\n\t\t<string>--config</string>\n\t\t<string>/Users/me/.config/riggs/work/riggs.toml</string>\n\t\t<string>run</string>"
        ));
        assert!(plist.contains("<key>KeepAlive</key>\n\t<true/>"));
        assert!(plist.contains("<key>RunAtLoad</key>\n\t<true/>"));
        let logs = home.path().join("Library/Logs/riggs");
        assert!(plist.contains(&logs.join("riggs.work.err.log").display().to_string()));
        assert!(plist.contains(&format!(
            "{}:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
            home.path().join(".local/bin").display()
        )));
        assert!(logs.is_dir());
    }

    #[test]
    fn an_existing_plist_is_kept_unless_update_existing_is_passed() {
        let home = tempfile::tempdir().unwrap();
        let path = install(&plan(home.path()), false).unwrap().path;
        fs::write(&path, "hand edited").unwrap();

        let message = install(&plan(home.path()), false).unwrap_err().to_string();
        assert!(message.contains("--update-existing"), "{message}");
        assert_eq!(fs::read_to_string(&path).unwrap(), "hand edited");

        assert!(install(&plan(home.path()), true).unwrap().replaced);
        assert!(fs::read_to_string(&path).unwrap().contains("riggs.work"));
    }

    #[test]
    fn an_alias_that_would_break_the_label_is_refused() {
        let home = tempfile::tempdir().unwrap();
        for alias in ["", "a b", "a/b"] {
            let refused = Job::new(alias, home.path().to_path_buf());
            assert!(matches!(refused, Err(LaunchdError::Alias(_))), "{alias:?}");
        }
    }

    #[test]
    fn values_are_escaped_for_xml() {
        let home = tempfile::tempdir().unwrap();
        let mut plan = plan(home.path());
        plan.config = PathBuf::from("/tmp/a&b<c>.toml");
        assert!(plan.plist().contains("/tmp/a&amp;b&lt;c&gt;.toml"));
        install(&plan, false).unwrap();
    }

    #[test]
    fn start_bootstraps_a_job_that_is_not_running_then_forces_the_first_run() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::default();
        assert_eq!(
            start(&installed(home.path()), &ctl).unwrap(),
            Outcome::Started
        );
        assert_eq!(ctl.verbs(), ["print", "bootstrap", "kickstart"]);
    }

    #[test]
    fn start_leaves_a_running_job_alone() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::running();
        let outcome = start(&installed(home.path()), &ctl).unwrap();
        assert_eq!(outcome, Outcome::AlreadyRunning);
        assert_eq!(ctl.verbs(), ["print"]);
        assert_eq!(
            outcome.message("riggs.work"),
            "riggs.work is already running."
        );
    }

    #[test]
    fn stop_boots_the_job_out_rather_than_letting_keepalive_respawn_it() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::running();
        assert_eq!(
            stop(&installed(home.path()), &ctl).unwrap(),
            Outcome::Stopped
        );
        assert_eq!(ctl.verbs(), ["print", "bootout"]);
    }

    #[test]
    fn stop_on_a_job_that_is_not_running_says_so_instead_of_failing() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::default();
        let outcome = stop(&installed(home.path()), &ctl).unwrap();
        assert_eq!(outcome, Outcome::AlreadyStopped);
        assert_eq!(ctl.verbs(), ["print"]);
        assert_eq!(outcome.message("riggs.work"), "riggs.work is not running.");
    }

    #[test]
    fn a_restart_drains_the_daemon_and_never_kills_it() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::running();
        let outcome = restart(&installed(home.path()), &ctl, false).unwrap();
        assert_eq!(outcome, Outcome::Restarted { forced: false });
        assert_eq!(ctl.verbs(), ["print", "bootout", "bootstrap", "kickstart"]);
        assert!(
            !ctl.calls.borrow().iter().any(|call| call.contains("-k")),
            "{:?}",
            ctl.calls.borrow()
        );
    }

    #[test]
    fn a_forced_restart_kills_the_running_process() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::running();
        let outcome = restart(&installed(home.path()), &ctl, true).unwrap();
        assert_eq!(outcome, Outcome::Restarted { forced: true });
        assert_eq!(ctl.verbs(), ["print", "kickstart -k"]);
        assert_eq!(
            outcome.message("riggs.work"),
            "Restarted riggs.work, killing the running process."
        );
    }

    #[test]
    fn restarting_a_stopped_job_just_starts_it() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::default();
        assert_eq!(
            restart(&installed(home.path()), &ctl, true).unwrap(),
            Outcome::Started
        );
        assert_eq!(ctl.verbs(), ["print", "bootstrap", "kickstart"]);
    }

    #[test]
    fn uninstall_stops_the_job_before_removing_the_plist_it_points_at() {
        let home = tempfile::tempdir().unwrap();
        let job = installed(home.path());
        let ctl = Fake::running();
        let outcome = uninstall(&job, &ctl).unwrap();
        assert_eq!(outcome, Outcome::Uninstalled { was_running: true });
        assert_eq!(ctl.verbs(), ["print", "bootout"]);
        assert!(!job.path().exists());
    }

    #[test]
    fn uninstalling_a_stopped_job_only_removes_the_plist() {
        let home = tempfile::tempdir().unwrap();
        let job = installed(home.path());
        let ctl = Fake::default();
        let outcome = uninstall(&job, &ctl).unwrap();
        assert_eq!(outcome, Outcome::Uninstalled { was_running: false });
        assert!(!job.path().exists());
    }

    #[test]
    fn every_verb_refuses_a_job_that_was_never_installed() {
        let home = tempfile::tempdir().unwrap();
        let job = job(home.path());
        let ctl = Fake::default();
        let refusals = [
            start(&job, &ctl).unwrap_err(),
            stop(&job, &ctl).unwrap_err(),
            restart(&job, &ctl, false).unwrap_err(),
            uninstall(&job, &ctl).unwrap_err(),
        ];
        for refusal in refusals {
            assert!(
                matches!(refusal, LaunchdError::NotInstalled { .. }),
                "{refusal}"
            );
            assert!(refusal.to_string().contains("riggs launchd install"));
        }
        assert!(ctl.calls.borrow().is_empty());
    }

    #[test]
    fn a_launchctl_refusal_names_the_verb_the_label_and_what_launchctl_said() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake {
            fails: Some("bootout failed: 113: Could not find specified service".to_owned()),
            running: true,
            ..Fake::default()
        };
        let message = stop(&installed(home.path()), &ctl).unwrap_err().to_string();
        assert!(
            message.contains("launchctl bootout failed for riggs.work"),
            "{message}"
        );
        assert!(message.contains("113"), "{message}");
    }

    /// Trimmed from a real `launchctl print gui/501/riggs.work`.
    const PRINT: &str = "	gui/501/riggs.work = {
		active count = 1
		path = /Users/me/Library/LaunchAgents/riggs.work.plist
		state = running
		pid = 4812
		last exit code = 0
	}";

    #[test]
    fn status_reads_the_pid_and_last_exit_out_of_the_launchctl_dump() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::printing(PRINT);
        let status = status(&installed(home.path()), &ctl).unwrap();
        assert!(status.installed && status.loaded);
        assert_eq!(status.pid, Some(4812));
        assert_eq!(status.last_exit, Some(0));
        let report = status.to_string();
        assert!(
            report.starts_with("riggs.work is running (pid 4812)."),
            "{report}"
        );
        assert!(report.contains("last exit:   0"), "{report}");
        assert!(report.contains("riggs.work.err.log"), "{report}");
    }

    #[test]
    fn status_also_understands_the_last_exit_status_spelling() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::printing("	state = not running\n	last exit status = 78\n");
        let status = status(&installed(home.path()), &ctl).unwrap();
        assert_eq!(status.last_exit, Some(78));
        assert_eq!(status.pid, None);
        assert!(
            status
                .to_string()
                .starts_with("riggs.work is loaded but not running right now."),
            "{status}"
        );
    }

    #[test]
    fn status_tells_an_uninstalled_job_apart_from_an_unloaded_one() {
        let home = tempfile::tempdir().unwrap();
        let ctl = Fake::default();

        let missing = status(&job(home.path()), &ctl).unwrap();
        assert!(!missing.installed);
        assert!(
            missing.to_string().contains("is not installed."),
            "{missing}"
        );
        assert!(
            ctl.calls.borrow().is_empty(),
            "nothing to ask launchctl about"
        );

        let unloaded = status(&installed(home.path()), &ctl).unwrap();
        assert!(unloaded.installed && !unloaded.loaded);
        assert!(
            unloaded.to_string().contains("riggs launchd start"),
            "{unloaded}"
        );
    }

    #[test]
    fn the_job_is_addressed_in_the_gui_domain_of_whoever_owns_the_home() {
        let home = tempfile::tempdir().unwrap();
        let job = job(home.path());
        let uid = fs::metadata(home.path()).unwrap().uid();
        assert_eq!(job.domain().unwrap(), format!("gui/{uid}"));
        assert_eq!(job.target().unwrap(), format!("gui/{uid}/riggs.work"));
    }
}
