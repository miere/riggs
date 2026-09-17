use std::fs::{self, OpenOptions, Permissions};
use std::io::{self, Write};
use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt, PermissionsExt};
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
    #[error("{path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
    #[error("plutil rejected {path}: {output}")]
    Lint { path: PathBuf, output: String },
}

#[derive(Debug, Clone)]
pub struct Plan {
    pub alias: String,
    pub binary: PathBuf,
    pub config: PathBuf,
    pub home: PathBuf,
}

#[derive(Debug)]
pub struct Installed {
    pub path: PathBuf,
    pub label: String,
    pub replaced: bool,
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

    pub fn plist(&self) -> String {
        let label = self.label();
        let logs = self.log_dir();
        let mut path = vec![self.home.join(".local/bin").display().to_string()];
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
            home = escape(&self.home.display().to_string()),
            out = escape(&logs.join(format!("{label}.out.log")).display().to_string()),
            err = escape(&logs.join(format!("{label}.err.log")).display().to_string()),
            path = escape(&path.join(":")),
        )
    }
}

pub fn install(plan: &Plan, update_existing: bool) -> Result<Installed, LaunchdError> {
    let valid = !plan.alias.is_empty()
        && plan
            .alias
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'));
    if !valid {
        return Err(LaunchdError::Alias(plan.alias.clone()));
    }
    let path = plan.path();
    let replaced = fs::symlink_metadata(&path).is_ok();
    if replaced && !update_existing {
        return Err(LaunchdError::Exists {
            path,
            label: plan.label(),
        });
    }
    for dir in [path.parent().map(Path::to_path_buf), Some(plan.log_dir())]
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
        label: plan.label(),
        replaced,
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

    use super::*;

    fn plan(home: &Path) -> Plan {
        Plan {
            alias: "work".to_owned(),
            binary: PathBuf::from("/opt/riggs/bin/riggs"),
            config: PathBuf::from("/Users/me/.config/riggs/work/riggs.toml"),
            home: home.to_path_buf(),
        }
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
            let mut plan = plan(home.path());
            plan.alias = alias.to_owned();
            assert!(matches!(install(&plan, false), Err(LaunchdError::Alias(_))));
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
}
