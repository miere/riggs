//! The sign-in workflows this node can drive. A profile answers four questions about a flow: what
//! to run, whether it ends with a code pasted back, how to spot the link in the command's output,
//! and which CLI release its output was checked against.
//!
//! The link patterns are kept per profile rather than shared, because a loose match is worse than
//! none: `gcloud` prints documentation links beside its consent URL, and handing someone the SDK
//! docs to sign in with is its own kind of silence.

use std::path::PathBuf;

use crate::config::ClaudeCodeConfig;

pub(crate) const CLAUDE_CODE: &str = "claude-code";
pub(crate) const GCLOUD: &str = "gcloud";
pub(crate) const GCLOUD_ADC: &str = "gcloud-adc";
pub(crate) const CUSTOM: &str = "custom";
/// Every profile a node can be asked for, in the order they are offered.
pub(crate) const NAMES: [&str; 4] = [CLAUDE_CODE, GCLOUD, GCLOUD_ADC, CUSTOM];

const CLAUDE_VERIFIED: &str = "2.1.271";
const CONSENT_PATH: &str = "oauth/authorize";

/// Where in a line a sign-in link is allowed to appear.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct Link {
    /// The link must be on this host, or any host when empty.
    pub host: &'static str,
    /// The link must carry this path segment, followed by something, or anything when empty.
    pub path: &'static str,
}

impl Link {
    fn matches(self, candidate: &str) -> bool {
        let host_ok = self.host.is_empty()
            || candidate.starts_with(&format!("https://{}/", self.host))
            || candidate == format!("https://{}", self.host);
        let path_ok = self.path.is_empty()
            || candidate
                .find(self.path)
                .is_some_and(|at| candidate.len() > at + self.path.len());
        host_ok && path_ok
    }
}

/// One sign-in workflow.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Profile {
    pub name: String,
    pub command: PathBuf,
    pub args: Vec<String>,
    /// The flow ends with a verification code pasted back, rather than in the browser alone.
    pub needs_code: bool,
    /// Run behind stub launchers, for a flow with no switch that keeps it off this machine's
    /// browser.
    pub suppress_browser: bool,
    pub link: Link,
    /// Tried only when `link` misses: it rescues a moved page without widening every match.
    pub fallback: Option<Link>,
    /// The CLI release this flow's output was read against, so drift is named in a failure.
    pub verified_version: Option<&'static str>,
}

impl Profile {
    /// The named built-in, ready to run. `custom` is not here: it carries a caller's command.
    pub(crate) fn builtin(name: &str, config: &ClaudeCodeConfig) -> Option<Profile> {
        let args = |args: &[&str]| args.iter().map(|arg| (*arg).to_owned()).collect();
        match name.trim() {
            // `claude auth login`, not `setup-token`: the latter mints a year-long inference-only
            // token, which is a different credential from the one every turn runs on.
            // `--claudeai` skips a menu a pipe cannot navigate.
            CLAUDE_CODE => Some(Profile {
                name: CLAUDE_CODE.to_owned(),
                command: config.command.clone(),
                args: args(&["auth", "login", "--claudeai"]),
                needs_code: true,
                suppress_browser: true,
                link: Link {
                    host: "claude.com",
                    path: CONSENT_PATH,
                },
                fallback: Some(Link {
                    host: "",
                    path: CONSENT_PATH,
                }),
                verified_version: Some(CLAUDE_VERIFIED),
            }),
            GCLOUD => Some(Profile {
                name: GCLOUD.to_owned(),
                command: PathBuf::from("gcloud"),
                args: args(&["auth", "login", "--no-launch-browser"]),
                needs_code: true,
                suppress_browser: false,
                link: Link {
                    host: "accounts.google.com",
                    path: "",
                },
                fallback: None,
                verified_version: None,
            }),
            // `--quiet` matters on a second run: with GOOGLE_APPLICATION_CREDENTIALS already set,
            // gcloud asks to confirm, cannot be answered without a terminal, and exits without
            // ever printing a link.
            GCLOUD_ADC => Some(Profile {
                name: GCLOUD_ADC.to_owned(),
                command: PathBuf::from("gcloud"),
                args: args(&[
                    "auth",
                    "application-default",
                    "login",
                    "--no-launch-browser",
                    "--quiet",
                ]),
                needs_code: true,
                suppress_browser: false,
                link: Link {
                    host: "accounts.google.com",
                    path: "",
                },
                fallback: None,
                verified_version: None,
            }),
            _ => None,
        }
    }

    /// A caller's own command line. Its output shape is unknown here, so any https link counts,
    /// and the browser is kept off this machine.
    pub(crate) fn custom(command: PathBuf, args: Vec<String>, needs_code: bool) -> Profile {
        Profile {
            name: CUSTOM.to_owned(),
            command,
            args,
            needs_code,
            suppress_browser: true,
            link: Link { host: "", path: "" },
            fallback: None,
            verified_version: None,
        }
    }

    /// The sign-in link in one line of the command's output, if it holds one.
    pub(crate) fn link_in(&self, line: &str) -> Option<String> {
        let candidates: Vec<&str> = line
            .match_indices("https://")
            .filter_map(|(at, _)| line.get(at..))
            .map(|rest| rest.split(char::is_whitespace).next().unwrap_or_default())
            .map(|candidate| {
                candidate.trim_end_matches(['.', ',', ';', ':', '\'', '"', ')', ']', '>'])
            })
            .collect();
        [Some(self.link), self.fallback]
            .into_iter()
            .flatten()
            .find_map(|link| {
                candidates
                    .iter()
                    .copied()
                    .find(|candidate| link.matches(candidate))
            })
            .map(str::to_owned)
    }

    /// What the owner is told is about to run on their machine.
    pub(crate) fn command_line(&self) -> String {
        let mut line = self.command.display().to_string();
        for arg in &self.args {
            line.push(' ');
            line.push_str(arg);
        }
        line
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::panic)]

    use super::*;

    const REAL_CLAUDE_LINE: &str = "If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Ainference&state=y_pucGha";

    fn claude() -> Profile {
        Profile::builtin(CLAUDE_CODE, &ClaudeCodeConfig::new("/tmp")).unwrap()
    }

    #[test]
    fn the_consent_link_is_taken_from_the_real_claude_login_line() {
        let url = REAL_CLAUDE_LINE.split_once("visit: ").map(|(_, url)| url);
        assert_eq!(claude().link_in(REAL_CLAUDE_LINE).as_deref(), url);
    }

    #[test]
    fn a_moved_claude_consent_page_still_matches_and_punctuation_is_trimmed() {
        assert_eq!(
            claude()
                .link_in("visit: https://auth.anthropic.example/cai/oauth/authorize?code=true.")
                .as_deref(),
            Some("https://auth.anthropic.example/cai/oauth/authorize?code=true")
        );
        assert_eq!(
            claude()
                .link_in(
                    "(https://auth.example/oauth/authorize?x=1) then https://claude.com/cai/oauth/authorize?y=2"
                )
                .as_deref(),
            Some("https://claude.com/cai/oauth/authorize?y=2")
        );
    }

    #[test]
    fn links_that_are_not_a_claude_consent_page_never_match() {
        for line in [
            "redirect_uri is https://platform.claude.com/oauth/code/callback for this flow",
            "See https://code.claude.com/docs/en/overview for help",
            "https://accounts.google.com/o/oauth2/auth?client_id=x",
            "bare https://claude.com/cai/oauth/authorize",
            "Paste code here if prompted > ",
        ] {
            assert_eq!(claude().link_in(line), None, "{line}");
        }
    }

    #[test]
    fn gcloud_takes_googles_consent_url_and_leaves_its_other_links_alone() {
        let profile = Profile::builtin(GCLOUD, &ClaudeCodeConfig::new("/tmp")).unwrap();
        assert_eq!(
            profile
                .link_in("Go to the following link: https://accounts.google.com/o/oauth2/auth?x=1")
                .as_deref(),
            Some("https://accounts.google.com/o/oauth2/auth?x=1")
        );
        for line in [
            "See https://cloud.google.com/sdk/auth_success for help",
            "error reference https://accounts.google.example/o/oauth2/auth",
        ] {
            assert_eq!(profile.link_in(line), None, "{line}");
        }
        assert!(profile.needs_code);
        assert_eq!(
            Profile::builtin(GCLOUD_ADC, &ClaudeCodeConfig::new("/tmp"))
                .unwrap()
                .command_line(),
            "gcloud auth application-default login --no-launch-browser --quiet"
        );
    }

    #[test]
    fn a_custom_flow_takes_any_https_link_but_never_a_plain_one() {
        let profile = Profile::custom(PathBuf::from("/bin/login"), vec!["--now".into()], false);
        assert_eq!(
            profile
                .link_in("open https://id.example.com/device?code=ABCD")
                .as_deref(),
            Some("https://id.example.com/device?code=ABCD")
        );
        assert_eq!(profile.link_in("open http://id.example.com/device"), None);
        assert_eq!(profile.command_line(), "/bin/login --now");
        assert!(profile.suppress_browser);
    }

    #[test]
    fn an_unknown_profile_is_not_a_builtin() {
        assert!(Profile::builtin("aws", &ClaudeCodeConfig::new("/tmp")).is_none());
        assert!(Profile::builtin(CUSTOM, &ClaudeCodeConfig::new("/tmp")).is_none());
    }
}
