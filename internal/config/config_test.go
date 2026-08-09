package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write drops content at dir/name and returns the full path.
func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

const sample = `
version: 1
admin:
  slack-user-id: U0B20G0ET9T
  jira-email: miere@nurturecloud.com
  github-login: miere
slack:
  profiles:
    default:
      bot-token: ${RIGGS_TEST_BOT}
      user-token: xoxp-literal
    nc:
      bot-token: xoxb-nc
`

func TestLoadExplicitPath(t *testing.T) {
	t.Setenv("RIGGS_TEST_BOT", "xoxb-from-env")
	path := write(t, t.TempDir(), "config.yaml", sample)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q, want %q", cfg.Path, path)
	}
	if cfg.Admin.GitHubLogin != "miere" {
		t.Errorf("github-login = %q, want miere", cfg.Admin.GitHubLogin)
	}
	p, name, ok := cfg.Profile("")
	if !ok || name != DefaultProfile {
		t.Fatalf("Profile(\"\") = %q, ok=%v; want the default profile", name, ok)
	}
	if p.BotToken != "xoxb-from-env" {
		t.Errorf("bot-token = %q, want the ${ENV} reference expanded", p.BotToken)
	}
	if p.UserToken != "xoxp-literal" {
		t.Errorf("user-token = %q, want the literal preserved", p.UserToken)
	}
}

// A missing explicit path is an error: asking for a specific file and silently
// getting none is never what the caller meant.
func TestLoadExplicitMissingIsError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load(missing explicit path) = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "--config-file") {
		t.Errorf("error %q does not name the flag that caused it", err)
	}
}

// No file at any conventional location is NOT an error — that is what keeps
// `riggs ping` and `riggs mcp` working on an unprovisioned machine.
func TestLoadMissingConventionalIsEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RIGGS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Path != NoFilePath {
		t.Errorf("Path = %q, want %q", cfg.Path, NoFilePath)
	}
	if len(cfg.Slack.Profiles) != 0 {
		t.Errorf("Profiles = %v, want none", cfg.Slack.Profiles)
	}
	// The ledger still needs a home, derived from where the file would live.
	if want := filepath.Join(dir, "riggs", "config.db"); cfg.DBPath() != want {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath(), want)
	}
}

func TestPrecedenceEnvBeatsXDG(t *testing.T) {
	envDir, xdgDir := t.TempDir(), t.TempDir()
	envPath := write(t, envDir, "chosen.yaml", "version: 1\n")
	if err := os.MkdirAll(filepath.Join(xdgDir, "riggs"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(xdgDir, "riggs"), "config.yaml", "version: 2\n")

	t.Setenv("RIGGS_CONFIG", envPath)
	t.Setenv("XDG_CONFIG_HOME", xdgDir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Path != envPath {
		t.Errorf("Path = %q, want $RIGGS_CONFIG to win (%q)", cfg.Path, envPath)
	}
}

// The ledger lives beside the config file, under the same base name, so moving
// the config with --config-file moves the state with it.
func TestDBPathMirrorsConfigName(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "riggs-test.yaml", "version: 1\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "riggs-test.db"); cfg.DBPath() != want {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath(), want)
	}
}

// A mistyped key is a silent behaviour change, so it is refused.
func TestUnknownFieldRejected(t *testing.T) {
	path := write(t, t.TempDir(), "config.yaml", "admin:\n  slack_user_id: U1\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(unknown key) = nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "slack_user_id") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestProfileLookupMiss(t *testing.T) {
	path := write(t, t.TempDir(), "config.yaml", "slack:\n  profiles:\n    nc:\n      bot-token: x\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, _, ok := cfg.Profile(""); ok {
		t.Error("Profile(\"\") succeeded with no default profile configured; want a miss")
	}
	if _, name, ok := cfg.Profile("nc"); !ok || name != "nc" {
		t.Errorf("Profile(\"nc\") = %q, ok=%v; want the nc profile", name, ok)
	}
}
