use std::fs::Permissions;
use std::os::unix::fs::PermissionsExt;

use crate::harness::{Agent, Rig, SECRET};
use crate::support::TOKEN;

fn assert_no_dial(rig: &Rig) {
    assert!(
        rig.sim.handshakes().is_empty(),
        "riggs dialled: {:?}",
        rig.sim.handshakes()
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn a_config_without_a_gateway_fails_naming_the_field_and_never_dials() {
    let rig = Rig::new().await;
    rig.write_config(&rig.agent_config(Agent::Claude("basic")));
    rig.write_token(TOKEN, 0o600);
    let run = rig.with_config("run").await;
    assert_eq!(run.status.code(), Some(1), "{}", run.stderr);
    assert!(run.stderr.contains("gateway.url"), "{}", run.stderr);
    assert!(run.stdout.is_empty(), "the banner printed: {}", run.stdout);

    let missing = rig
        .exits(&["--config", "/nonexistent/riggs.toml", "run"])
        .await;
    assert_eq!(missing.status.code(), Some(1));
    assert!(
        missing.stderr.contains("/nonexistent/riggs.toml"),
        "{}",
        missing.stderr
    );

    let default = rig.exits(&["run"]).await;
    assert_eq!(default.status.code(), Some(1));
    assert!(
        default.stderr.contains(
            &rig.home()
                .join(".config/riggs/default/riggs.toml")
                .display()
                .to_string()
        ),
        "{}",
        default.stderr
    );
    assert_no_dial(&rig);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_placeholder_command_counts_as_unset() {
    let rig = Rig::new().await;
    rig.write_config(&format!(
        "[gateway]\nurl = \"{}\"\n[agent]\nkind = \"claude_code\"\ncommand = \"claude-replace-me\"\n",
        rig.sim.url()
    ));
    rig.write_token(TOKEN, 0o600);
    for command in ["run", "validate"] {
        let finished = rig.with_config(command).await;
        assert_eq!(finished.status.code(), Some(1));
        assert!(
            finished.stderr.contains("agent.command: is not set"),
            "{}",
            finished.stderr
        );
    }
    assert_no_dial(&rig);
}

#[tokio::test(flavor = "multi_thread")]
async fn plain_ws_is_only_accepted_for_loopback() {
    let rig = Rig::new().await;
    let agent = rig.agent_config(Agent::Claude("basic"));
    rig.write_token(TOKEN, 0o600);
    rig.write_config(&format!(
        "[gateway]\nurl = \"ws://gateway.example.com\"\n{agent}"
    ));
    let refused = rig.with_config("run").await;
    assert_eq!(refused.status.code(), Some(1));
    assert!(refused.stderr.contains("gateway.url"), "{}", refused.stderr);
    assert!(refused.stderr.contains("wss://"), "{}", refused.stderr);

    rig.write_config(&format!("[gateway]\nurl = \"ws://127.0.0.1:9\"\n{agent}"));
    let accepted = rig.with_config("validate").await;
    assert_eq!(accepted.status.code(), Some(0), "{}", accepted.stderr);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_token_file_others_can_read_is_refused_by_run_and_validate() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o644);
    for command in ["run", "validate"] {
        let finished = rig.with_config(command).await;
        assert_eq!(finished.status.code(), Some(1), "{command}");
        assert!(finished.stderr.contains("0644"), "{}", finished.stderr);
        assert!(!finished.stderr.contains(SECRET), "{}", finished.stderr);
    }
    assert_no_dial(&rig);
}

#[tokio::test(flavor = "multi_thread")]
async fn a_missing_or_empty_token_file_fails_with_the_mint_hint() {
    let rig = Rig::new().await;
    rig.configure(Agent::Claude("basic"), "");
    let missing = rig.with_config("run").await;
    assert_eq!(missing.status.code(), Some(1));
    assert!(
        missing.stderr.contains("node token mint"),
        "{}",
        missing.stderr
    );

    rig.write_token("", 0o600);
    let empty = rig.with_config("run").await;
    assert_eq!(empty.status.code(), Some(1));
    assert!(empty.stderr.contains("is empty"), "{}", empty.stderr);
    assert!(empty.stderr.contains("node token mint"), "{}", empty.stderr);

    rig.write_token("mrtg_node_nothex_secret", 0o600);
    let malformed = rig.with_config("run").await;
    assert_eq!(malformed.status.code(), Some(1));
    assert!(
        malformed.stderr.contains("malformed"),
        "{}",
        malformed.stderr
    );
    assert!(!malformed.stderr.contains("nothex"), "{}", malformed.stderr);
    assert_no_dial(&rig);
}

#[tokio::test(flavor = "multi_thread")]
async fn validate_reports_every_problem_together_and_passes_a_good_config_without_dialling() {
    let rig = Rig::new().await;
    rig.write_config("[agent]\ncommand = \"\"\n[log]\nlevel = \"loud\"\n");
    let bad = rig.with_config("validate").await;
    assert_eq!(bad.status.code(), Some(1));
    for field in [
        "gateway.url",
        "agent.kind",
        "agent.command",
        "log.level",
        "gateway.token_file",
    ] {
        assert!(
            bad.stderr.contains(field),
            "{field} missing from:\n{}",
            bad.stderr
        );
    }

    rig.configure(Agent::Claude("basic"), "");
    rig.write_token(TOKEN, 0o600);
    let good = rig.with_config("validate").await;
    assert_eq!(good.status.code(), Some(0), "{}", good.stderr);
    assert!(good.stdout.contains("is valid"), "{}", good.stdout);
    assert!(!good.stdout.contains(SECRET));
    assert_no_dial(&rig);
}

#[tokio::test(flavor = "multi_thread")]
async fn launchd_writes_a_plist_and_never_overwrites_one_by_accident() {
    let rig = Rig::new().await;
    let config = rig.config_path();
    let args = [
        "--config",
        config.to_str().unwrap(),
        "launchd",
        "--alias",
        "work",
        "--binary-path",
        "/opt/riggs/riggs",
    ];
    let first = rig.exits(&args).await;
    assert_eq!(first.status.code(), Some(0), "{}", first.stderr);
    let plist = rig.home().join("Library/LaunchAgents/riggs.work.plist");
    let written = std::fs::read(&plist).unwrap();
    let text = String::from_utf8_lossy(&written);
    assert!(text.contains(&format!(
        "<string>/opt/riggs/riggs</string>\n\t\t<string>--config</string>\n\t\t<string>{}</string>\n\t\t<string>run</string>",
        config.display()
    )), "{text}");
    assert!(text.contains("<key>KeepAlive</key>\n\t<true/>"));
    assert!(text.contains("/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"));
    assert!(
        text.contains(
            &rig.home()
                .join("Library/Logs/riggs/riggs.work.out.log")
                .display()
                .to_string()
        )
    );
    assert_eq!(
        std::fs::metadata(&plist).unwrap().permissions().mode() & 0o777,
        0o644
    );
    assert!(
        first.stdout.contains("launchctl bootstrap"),
        "{}",
        first.stdout
    );

    std::fs::set_permissions(&plist, Permissions::from_mode(0o644)).unwrap();
    let refused = rig.exits(&args).await;
    assert_eq!(refused.status.code(), Some(1));
    assert!(
        refused.stderr.contains("--update-existing"),
        "{}",
        refused.stderr
    );
    assert_eq!(std::fs::read(&plist).unwrap(), written);

    std::fs::write(&plist, "stale").unwrap();
    let mut replacing = args.to_vec();
    replacing.push("--update-existing");
    let replaced = rig.exits(&replacing).await;
    assert_eq!(replaced.status.code(), Some(0), "{}", replaced.stderr);
    assert_eq!(std::fs::read(&plist).unwrap(), written);
}

#[tokio::test(flavor = "multi_thread")]
async fn help_works_with_no_configuration_and_bare_riggs_is_a_usage_error() {
    let rig = Rig::new().await;
    let help = rig.exits(&["help"]).await;
    assert_eq!(help.status.code(), Some(0));
    assert!(help.stdout.contains("validate"), "{}", help.stdout);
    let bare = rig.exits(&[]).await;
    assert_eq!(bare.status.code(), Some(2));
    let version = rig.exits(&["version"]).await;
    assert_eq!(version.status.code(), Some(0));
    assert!(version.stdout.starts_with("riggs "), "{}", version.stdout);
    assert!(!rig.home().join(".config").exists());
}
