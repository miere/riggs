package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig lays out a config file and, optionally, a sibling .env.
func writeConfig(t *testing.T, body, env string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(dir, EnvFileName), []byte(env), 0o600); err != nil {
			t.Fatalf("write env: %v", err)
		}
	}
	return path
}

// This is the whole reason the feature exists: under launchd there is no shell
// environment, so a ${VAR} with nothing behind it silently yields a daemon
// connected to nothing.
func TestSiblingEnvFileResolvesTokens(t *testing.T) {
	path := writeConfig(t, `
slack:
  profiles:
    riggs:
      bot-token: ${TEST_RIGGS_BOT}
      app-token: ${TEST_RIGGS_APP}
`, "TEST_RIGGS_BOT=xoxb-from-file\nTEST_RIGGS_APP=xapp-from-file\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _, ok := cfg.Profile("riggs")
	if !ok {
		t.Fatal("profile riggs is missing")
	}
	if p.BotToken != "xoxb-from-file" || p.AppToken != "xapp-from-file" {
		t.Fatalf("tokens = %q / %q", p.BotToken, p.AppToken)
	}
}

// Standard dotenv precedence: an already-set variable wins, so an operator can
// override one token for a single run without editing a file.
func TestRealEnvironmentBeatsTheFile(t *testing.T) {
	t.Setenv("TEST_RIGGS_BOT", "xoxb-from-env")
	path := writeConfig(t, `
slack:
  profiles:
    riggs:
      bot-token: ${TEST_RIGGS_BOT}
`, "TEST_RIGGS_BOT=xoxb-from-file\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _, _ := cfg.Profile("riggs")
	if p.BotToken != "xoxb-from-env" {
		t.Fatalf("BotToken = %q, want the ambient value", p.BotToken)
	}
}

// Riggs is still invoked by Murtaugh with the variables already exported, and
// refusing to start there because there is no .env would be a regression.
func TestMissingSiblingEnvFileIsNotAnError(t *testing.T) {
	path := writeConfig(t, "admin:\n  slack-user-id: U1\n", "")
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// Asking for a specific file and silently getting none is never what the
// caller meant — the same rule --config-file follows.
func TestNamedEnvFileThatIsMissingIsAnError(t *testing.T) {
	path := writeConfig(t, "env-file: /nonexistent/riggs.env\n", "")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted a named env-file that does not exist")
	}
	if !strings.Contains(err.Error(), "env-file") {
		t.Fatalf("error does not name the setting: %v", err)
	}
}

// The point of naming one: pointing Riggs at Murtaugh's existing .env rather
// than duplicating the secrets.
func TestNamedEnvFileIsLoaded(t *testing.T) {
	other := filepath.Join(t.TempDir(), "murtaugh.env")
	if err := os.WriteFile(other, []byte("TEST_RIGGS_SHARED=xoxb-shared\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	path := writeConfig(t, `
env-file: `+other+`
slack:
  profiles:
    riggs:
      bot-token: ${TEST_RIGGS_SHARED}
`, "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _, _ := cfg.Profile("riggs")
	if p.BotToken != "xoxb-shared" {
		t.Fatalf("BotToken = %q", p.BotToken)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := expandHome("~/x/y"); got != filepath.Join(home, "x", "y") {
		t.Errorf("expandHome(~/x/y) = %q", got)
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q", got)
	}
	// A bare tilde inside a path is a legal filename character, not a home.
	for _, path := range []string{"/abs/~x", "~notme/x", "relative/path"} {
		if got := expandHome(path); got != path {
			t.Errorf("expandHome(%q) = %q, want it unchanged", path, got)
		}
	}
}
