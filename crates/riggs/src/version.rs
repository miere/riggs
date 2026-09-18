use std::fmt;
use std::time::Duration;

use serde::Deserialize;

/// Release builds set `RIGGS_VERSION`; anything else reports the crate version.
pub const VERSION: &str = match option_env!("RIGGS_VERSION") {
    Some(version) => version,
    None => env!("CARGO_PKG_VERSION"),
};

const LATEST_RELEASE: &str = "https://api.github.com/repos/miere/riggs/releases/latest";
const TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Debug, thiserror::Error)]
pub enum VersionError {
    #[error("could not ask GitHub for the latest Riggs release: {0}")]
    Fetch(String),
}

#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct Release {
    #[serde(rename = "tag_name")]
    pub tag: String,
    #[serde(rename = "html_url")]
    pub url: String,
}

/// Injected so tests never touch the network.
pub trait Releases {
    fn latest(&self) -> Result<Release, VersionError>;
}

pub struct GitHub;

impl Releases for GitHub {
    fn latest(&self) -> Result<Release, VersionError> {
        let agent = ureq::Agent::config_builder()
            .timeout_global(Some(TIMEOUT))
            .build()
            .new_agent();
        let token = github_token();
        let mut request = agent
            .get(LATEST_RELEASE)
            .header("Accept", "application/vnd.github+json")
            .header("User-Agent", format!("riggs/{VERSION}"));
        if let Some(token) = &token {
            request = request.header("Authorization", format!("Bearer {token}"));
        }
        request
            .call()
            .map_err(|err| VersionError::Fetch(explain(err, token.is_some())))?
            .body_mut()
            .read_json::<Release>()
            .map_err(|err| VersionError::Fetch(err.to_string()))
    }
}

/// The repository is private, so an anonymous request only ever sees a 404.
fn github_token() -> Option<String> {
    ["GH_TOKEN", "GITHUB_TOKEN"]
        .iter()
        .filter_map(|name| std::env::var(name).ok())
        .map(|token| token.trim().to_owned())
        .find(|token| !token.is_empty())
}

fn explain(err: ureq::Error, authenticated: bool) -> String {
    match err {
        ureq::Error::StatusCode(404) if authenticated => {
            "GitHub has no Riggs release yet, or this token cannot read the repository".to_owned()
        }
        ureq::Error::StatusCode(404) => {
            "GitHub answered 404: the repository is private, so set GH_TOKEN, for example GH_TOKEN=$(gh auth token)".to_owned()
        }
        other => other.to_string(),
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Check {
    UpToDate {
        current: String,
        latest: String,
    },
    UpdateAvailable {
        current: String,
        latest: String,
        url: String,
    },
    /// A dev or otherwise unversioned build cannot be older than anything.
    Uncomparable {
        current: String,
        latest: String,
    },
}

impl fmt::Display for Check {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::UpToDate { current, latest } => {
                write!(
                    f,
                    "riggs {current} is up to date (latest release: {latest})"
                )
            }
            Self::UpdateAvailable {
                current,
                latest,
                url,
            } => write!(
                f,
                "riggs {current} is installed; {latest} is available.\nRelease notes: {url}"
            ),
            Self::Uncomparable { current, latest } => write!(
                f,
                "riggs {current} is not a release build, so it cannot tell whether an update applies (latest release: {latest})"
            ),
        }
    }
}

pub fn check(current: &str, releases: &dyn Releases) -> Result<Check, VersionError> {
    let release = releases.latest()?;
    let parse = |raw: &str| semver::Version::parse(raw.trim().trim_start_matches('v')).ok();
    let (current, latest) = (current.to_owned(), release.tag);
    Ok(match (parse(&current), parse(&latest)) {
        (Some(installed), Some(newest)) if newest > installed => Check::UpdateAvailable {
            current,
            latest,
            url: release.url,
        },
        (Some(_), Some(_)) => Check::UpToDate { current, latest },
        _ => Check::Uncomparable { current, latest },
    })
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

    use super::*;

    struct Fixed(Result<&'static str, &'static str>);

    impl Releases for Fixed {
        fn latest(&self) -> Result<Release, VersionError> {
            self.0
                .map(|tag| Release {
                    tag: tag.to_owned(),
                    url: format!("https://github.com/miere/riggs/releases/tag/{tag}"),
                })
                .map_err(|err| VersionError::Fetch(err.to_owned()))
        }
    }

    #[test]
    fn a_newer_release_is_reported_with_its_notes() {
        let checked = check("0.1.0", &Fixed(Ok("v0.2.0"))).unwrap();
        assert!(
            matches!(checked, Check::UpdateAvailable { ref url, .. } if url.ends_with("v0.2.0"))
        );
        assert!(checked.to_string().contains("v0.2.0 is available"));
    }

    #[test]
    fn the_same_or_an_older_release_is_up_to_date() {
        assert!(matches!(
            check("0.2.0", &Fixed(Ok("v0.2.0"))).unwrap(),
            Check::UpToDate { .. }
        ));
        assert!(matches!(
            check("0.3.0", &Fixed(Ok("v0.2.0"))).unwrap(),
            Check::UpToDate { .. }
        ));
    }

    #[test]
    fn dev_and_unversioned_builds_never_report_an_update() {
        for current in ["dev", "main", ""] {
            let checked = check(current, &Fixed(Ok("v9.9.9"))).unwrap();
            assert!(matches!(checked, Check::Uncomparable { .. }), "{current}");
        }
        assert!(matches!(
            check("0.1.0", &Fixed(Ok("nightly"))).unwrap(),
            Check::Uncomparable { .. }
        ));
    }

    #[test]
    fn a_network_failure_is_a_clean_error() {
        let message = check("0.1.0", &Fixed(Err("dns failure")))
            .unwrap_err()
            .to_string();
        assert!(message.contains("dns failure"), "{message}");
    }
}
