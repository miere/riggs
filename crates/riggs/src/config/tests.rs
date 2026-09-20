#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]

use super::*;

fn write(content: &str) -> (tempfile::TempDir, PathBuf) {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join(FILE_NAME);
    std::fs::write(&path, content).unwrap();
    (dir, path)
}

fn problems(content: &str) -> Vec<Problem> {
    let (_dir, path) = write(content);
    match load(&path, &Overrides::default()) {
        Err(ConfigError::Invalid { problems, .. }) => problems.0,
        other => panic!("expected problems, got {other:?}"),
    }
}

fn fields(content: &str) -> Vec<String> {
    problems(content)
        .into_iter()
        .map(|problem| problem.field)
        .collect()
}

const GOOD: &str = r#"
[gateway]
urls = ["wss://gateway.example.com"]

[agent]
kind = "claude_code"
command = "claude"
"#;

#[test]
fn a_minimal_config_resolves_defaults_against_its_directory() {
    let (dir, path) = write(GOOD);
    let config = load(&path, &Overrides::default()).unwrap();
    assert_eq!(config.gateways, ["wss://gateway.example.com/rax/v1/link"]);
    assert_eq!(config.token_file, dir.path().join("node-token"));
    let AgentConfig::ClaudeCode(agent) = &config.agent else {
        panic!("expected claude_code")
    };
    assert_eq!(agent.command, PathBuf::from("claude"));
    assert_eq!(agent.workdir, dir.path());
    assert!(matches!(
        &config.sessions,
        SessionsConfig::Durable { dir: store, retain } if *store == dir.path().join("sessions") && *retain == SESSION_RETENTION
    ));
    assert_eq!(config.log.level, tracing::Level::INFO);
    assert_eq!(config.log.format, LogFormat::Text);
}

#[test]
fn an_empty_file_names_every_required_field_together() {
    assert_eq!(fields(""), ["gateway.urls", "agent.kind", "agent.command"]);
}

#[test]
fn blank_and_placeholder_values_count_as_unset() {
    let found = problems(
        r#"
[gateway]
urls = ["  "]
[agent]
kind = "claude_code"
command = "claude-replace-me"
"#,
    );
    assert_eq!(found[0].field, "gateway.urls");
    assert!(found[0].message.contains("not set"));
    assert_eq!(found[1].field, "agent.command");
    assert!(found[1].message.contains("not set"));
}

#[test]
fn plain_ws_is_refused_unless_the_gateway_is_loopback() {
    let found = problems(&GOOD.replace("wss://gateway.example.com", "ws://gateway.example.com"));
    assert_eq!(found[0].field, "gateway.urls[0]");
    assert!(found[0].message.contains("wss://"), "{}", found[0].message);
    let (_dir, path) = write(&GOOD.replace("wss://gateway.example.com", "ws://127.0.0.1:9"));
    assert!(load(&path, &Overrides::default()).is_ok());
}

#[test]
fn fallback_gateways_keep_their_order_and_a_bad_one_is_named_by_position() {
    let (_dir, path) = write(&GOOD.replace(
        r#"["wss://gateway.example.com"]"#,
        r#"["wss://a.example.com", "wss://b.example.com"]"#,
    ));
    let config = load(&path, &Overrides::default()).unwrap();
    assert_eq!(
        config.gateways,
        [
            "wss://a.example.com/rax/v1/link",
            "wss://b.example.com/rax/v1/link"
        ]
    );
    let found = problems(&GOOD.replace(
        r#"["wss://gateway.example.com"]"#,
        r#"["wss://a.example.com", "http://b.example.com"]"#,
    ));
    assert_eq!(found[0].field, "gateway.urls[1]");
}

#[test]
fn unknown_keys_are_an_error_naming_the_key() {
    let (_dir, path) = write(&format!("{GOOD}\n[log]\nlevle = \"debug\"\n"));
    let message = load(&path, &Overrides::default()).unwrap_err().to_string();
    assert!(message.contains("levle"), "{message}");
}

#[test]
fn durations_must_parse_and_be_positive() {
    let found = problems(&format!(
        "{GOOD}hook_timeout = \"0s\"\ninterrupt_grace = \"soon\"\n[sessions]\nretain = \"-1d\"\n"
    ));
    let names: Vec<&str> = found.iter().map(|problem| problem.field.as_str()).collect();
    assert_eq!(
        names,
        [
            "agent.hook_timeout",
            "agent.interrupt_grace",
            "sessions.retain"
        ]
    );
    assert!(found[0].message.contains("longer than zero"));
}

#[test]
fn fields_of_the_other_backend_are_refused() {
    assert_eq!(
        fields(&format!("{GOOD}interruptible = false\n")),
        ["agent.interruptible"]
    );
    let acp = GOOD.replace("claude_code", "acp") + "model = \"opus\"\n";
    assert_eq!(fields(&acp), ["agent.model"]);
}

#[test]
fn insecure_skip_verify_is_refused_because_the_dialler_cannot_honour_it() {
    assert_eq!(
        fields(&GOOD.replace("[agent]", "insecure_skip_verify = true\n[agent]")),
        ["gateway.insecure_skip_verify"]
    );
}

#[test]
fn an_acp_agent_maps_onto_its_backend_config() {
    let (dir, path) = write(&format!(
        "{}args = [\"--acp\"]\nworkdir = \"work\"\ninterruptible = false\nstartup_timeout = \"5s\"\nenv = {{ MODE = \"x\" }}\n[sessions]\ndurable = false\n",
        GOOD.replace("claude_code", "acp")
            .replace("\"claude\"", "\"./bin/agent\"")
    ));
    let config = load(&path, &Overrides::default()).unwrap();
    let AgentConfig::Acp(agent) = &config.agent else {
        panic!("expected acp")
    };
    assert_eq!(
        agent.command,
        dir.path().join("bin/agent").display().to_string()
    );
    assert_eq!(agent.args, ["--acp"]);
    assert_eq!(
        agent.workdir.as_deref(),
        Some(dir.path().join("work").as_path())
    );
    assert_eq!(agent.interruptible, Some(false));
    assert_eq!(agent.startup_timeout, Duration::from_secs(5));
    assert_eq!(agent.env["MODE"], Some("x".to_owned()));
    assert!(matches!(config.sessions, SessionsConfig::Ephemeral));
}

#[test]
fn the_env_file_reaches_the_agent_without_overriding_the_config() {
    let (dir, path) = write(&format!(
        "env_file = \"agent.env\"\n{GOOD}env = {{ RIGGS_TEST_BOTH = \"config\" }}\n"
    ));
    std::fs::write(
        dir.path().join("agent.env"),
        "RIGGS_TEST_ONLY_DOTENV=dotenv\nRIGGS_TEST_BOTH=dotenv\nPATH=/nowhere\n",
    )
    .unwrap();
    let config = load(&path, &Overrides::default()).unwrap();
    let AgentConfig::ClaudeCode(agent) = &config.agent else {
        panic!("expected claude_code")
    };
    assert_eq!(agent.env["RIGGS_TEST_ONLY_DOTENV"], "dotenv");
    assert_eq!(agent.env["RIGGS_TEST_BOTH"], "config");
    assert!(!agent.env.contains_key("PATH"));
}

#[test]
fn a_missing_env_file_is_named() {
    assert_eq!(
        fields(&format!("env_file = \"absent.env\"\n{GOOD}")),
        ["env_file"]
    );
}

#[test]
fn flags_replace_file_values() {
    let (_dir, path) = write(&GOOD.replace("[agent]", "token_file = \"t\"\n[agent]"));
    let overrides = Overrides {
        gateway: vec!["ws://localhost:1".to_owned()],
        token_file: Some(PathBuf::from("/tmp/elsewhere")),
        insecure_skip_verify: false,
    };
    let config = load(&path, &overrides).unwrap();
    assert_eq!(config.gateways, ["ws://localhost:1/rax/v1/link"]);
    assert_eq!(config.token_file, PathBuf::from("/tmp/elsewhere"));
}

#[test]
fn a_missing_file_is_named() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("absent.toml");
    let message = load(&path, &Overrides::default()).unwrap_err().to_string();
    assert!(message.contains("absent.toml"), "{message}");
}

#[test]
fn the_session_store_cannot_share_the_config_directory() {
    assert_eq!(
        fields(&format!("{GOOD}[sessions]\ndir = \".\"\n")),
        ["sessions.dir"]
    );
}

#[test]
fn a_seatbelt_box_resolves_its_paths_and_always_denies_the_node_credential() {
    let toml = format!(
        "{GOOD}\n[agent.sandbox]\nmode = \"seatbelt\"\nwrite = [\"scratch\"]\ndeny_read = [\"/secrets\"]\n"
    );
    let (dir, path) = write(&toml);
    let config = load(&path, &Overrides::default()).unwrap();
    let AgentConfig::ClaudeCode(agent) = config.agent else {
        panic!("expected a claude_code agent");
    };
    if cfg!(target_os = "macos") {
        assert_eq!(agent.sandbox.mode, SandboxMode::Seatbelt);
    }
    assert_eq!(agent.sandbox.write, [dir.path().join("scratch")]);
    assert_eq!(
        agent.sandbox.deny_read.as_deref(),
        Some(&[PathBuf::from("/secrets")][..])
    );
    assert_eq!(
        agent.sandbox.node_token.as_deref(),
        Some(config.token_file.as_path())
    );
}

#[test]
fn a_box_defaults_to_off_and_an_unknown_one_is_named() {
    let (_dir, path) = write(GOOD);
    let config = load(&path, &Overrides::default()).unwrap();
    let AgentConfig::ClaudeCode(agent) = config.agent else {
        panic!("expected a claude_code agent");
    };
    assert_eq!(agent.sandbox.mode, SandboxMode::Off);
    assert!(
        agent.sandbox.deny_read.is_none(),
        "the credential stores stay blinded"
    );
    assert_eq!(
        fields(&format!("{GOOD}\n[agent.sandbox]\nmode = \"jail\"\n")),
        ["agent.sandbox.mode"]
    );
}
