//! Which gateway tool groups a session can publish without shadowing a tool server this machine's
//! Claude Code already has. Claude Code names every tool `mcp__<server>__<name>`, so a group named
//! like one of the owner's servers would hand the agent two tools under one name.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use rax::Unhandled;
use rax::open::{Subject, UnhandledReason};
use rax::tool::ToolGroup;
use serde_json::Value;

use crate::config::ClaudeCodeConfig;
use crate::wire::MCP_SERVER;

/// The servers configured for this node's working directory, read fresh for every session so an
/// edit to the owner's config needs no restart: user scope and local scope from `.claude.json`,
/// project scope from the working directory's `.mcp.json`. A file that is missing or unreadable
/// adds nothing.
pub(crate) async fn configured(config: &ClaudeCodeConfig) -> BTreeSet<String> {
    let mut names = BTreeSet::new();
    if let Some(user) = user_config(config)
        && let Some(json) = read(&user).await
    {
        names.extend(server_names(&json));
        let workdir = config.workdir.display().to_string();
        if let Some(project) = json
            .get("projects")
            .and_then(|projects| projects.get(&workdir))
        {
            names.extend(server_names(project));
        }
    }
    if let Some(json) = read(&config.workdir.join(".mcp.json")).await {
        names.extend(server_names(&json));
    }
    names
}

/// Splits the session's groups into those it can publish and those it cannot, each refused with a
/// reason the gateway can pass on to the person.
pub(crate) fn publishable(
    groups: &[ToolGroup],
    taken: &BTreeSet<String>,
) -> (Vec<ToolGroup>, Vec<Unhandled>) {
    let mut kept = Vec::with_capacity(groups.len());
    let mut refused = Vec::new();
    for group in groups {
        let message = if group.namespace == MCP_SERVER {
            format!("`{MCP_SERVER}` is this node's own tool server")
        } else if taken.contains(&group.namespace) {
            format!(
                "this machine's Claude Code already has a tool server named `{}`",
                group.namespace
            )
        } else {
            kept.push(group.clone());
            continue;
        };
        refused.push(Unhandled {
            subject: Subject::ToolGroup {
                namespace: group.namespace.clone(),
            },
            reason: UnhandledReason::Other,
            message: Some(message),
        });
    }
    (kept, refused)
}

/// `CLAUDE_CONFIG_DIR`, from the agent's own environment first, moves `.claude.json` with it.
fn user_config(config: &ClaudeCodeConfig) -> Option<PathBuf> {
    let dir = config
        .env
        .get("CLAUDE_CONFIG_DIR")
        .map(PathBuf::from)
        .or_else(|| std::env::var_os("CLAUDE_CONFIG_DIR").map(PathBuf::from));
    match dir {
        Some(dir) => Some(dir.join(".claude.json")),
        None => std::env::var_os("HOME").map(|home| PathBuf::from(home).join(".claude.json")),
    }
}

async fn read(path: &Path) -> Option<Value> {
    let bytes = tokio::fs::read(path).await.ok()?;
    match serde_json::from_slice(&bytes) {
        Ok(json) => Some(json),
        Err(err) => {
            tracing::debug!(path = %path.display(), error = %err, "could not read a Claude config for its tool servers");
            None
        }
    }
}

/// Each name as it appears in a tool name: Claude Code replaces anything outside
/// `[A-Za-z0-9_-]` with `_`. Lowercased, so a server differing from a group only in case is still
/// treated as the same: an agent told about both could not tell them apart.
fn server_names(scope: &Value) -> impl Iterator<Item = String> + '_ {
    scope
        .get("mcpServers")
        .and_then(Value::as_object)
        .into_iter()
        .flat_map(|servers| servers.keys())
        .map(|name| {
            name.chars()
                .map(|ch| match ch {
                    'a'..='z' | 'A'..='Z' | '0'..='9' | '_' | '-' => ch.to_ascii_lowercase(),
                    _ => '_',
                })
                .collect()
        })
}

#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used, clippy::expect_used)]

    use serde_json::json;

    use super::*;

    fn group(namespace: &str) -> ToolGroup {
        ToolGroup {
            namespace: namespace.to_owned(),
            tools: vec![],
        }
    }

    #[test]
    fn a_group_named_like_a_configured_server_or_riggs_is_not_published() {
        let taken = BTreeSet::from(["github".to_owned()]);
        let groups = [group("slack"), group("github"), group("riggs")];
        let (kept, refused) = publishable(&groups, &taken);
        assert_eq!(kept, vec![group("slack")]);
        let refused: Vec<_> = refused.into_iter().map(|u| u.subject).collect();
        assert_eq!(
            refused,
            vec![
                Subject::ToolGroup {
                    namespace: "github".into()
                },
                Subject::ToolGroup {
                    namespace: "riggs".into()
                },
            ]
        );
    }

    #[tokio::test]
    async fn every_scope_of_the_claude_config_is_read() {
        let home = tempfile::tempdir().unwrap();
        let workdir = tempfile::tempdir().unwrap();
        let workdir_key = workdir.path().display().to_string();
        let user = json!({
            "mcpServers": {"GitHub": {}, "my server": {}},
            "projects": {
                workdir_key: {"mcpServers": {"postgres": {}}},
                "/somewhere/else": {"mcpServers": {"elsewhere": {}}},
            },
        });
        std::fs::write(home.path().join(".claude.json"), user.to_string()).unwrap();
        let project = json!({"mcpServers": {"linear": {}}});
        std::fs::write(workdir.path().join(".mcp.json"), project.to_string()).unwrap();
        let mut config = ClaudeCodeConfig::new(workdir.path());
        config.env.insert(
            "CLAUDE_CONFIG_DIR".to_owned(),
            home.path().display().to_string(),
        );
        let names = configured(&config).await;
        let expected = ["github", "linear", "my_server", "postgres"].map(str::to_owned);
        assert_eq!(names, BTreeSet::from(expected));
    }

    #[tokio::test]
    async fn a_missing_or_broken_config_takes_nothing() {
        let home = tempfile::tempdir().unwrap();
        std::fs::write(home.path().join(".claude.json"), "{not json").unwrap();
        let mut config = ClaudeCodeConfig::new(home.path().join("absent"));
        config.env.insert(
            "CLAUDE_CONFIG_DIR".to_owned(),
            home.path().display().to_string(),
        );
        assert!(configured(&config).await.is_empty());
    }
}
