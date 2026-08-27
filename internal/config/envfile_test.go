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

// Riggs' own dotenv wins over the ambient environment — a deliberate inversion
// of standard dotenv precedence.
//
// The scenario it exists for: Murtaugh's gateway exports its own
// SLACK_BOT_TOKEN into the environment every job it spawns inherits. Under
// standard precedence the *same* profile would resolve to Murtaugh's app when
// scheduled and to Riggs' own when started by launchd — one identity posting
// the digest and another listening for its clicks, failing silently.
func TestTheFileBeatsTheAmbientEnvironment(t *testing.T) {
	t.Setenv("TEST_RIGGS_BOT", "xoxb-murtaughs-app")
	path := writeConfig(t, `
slack:
  profiles:
    riggs:
      bot-token: ${TEST_RIGGS_BOT}
`, "TEST_RIGGS_BOT=xoxb-riggs-own-app\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, _, _ := cfg.Profile("riggs")
	if p.BotToken != "xoxb-riggs-own-app" {
		t.Fatalf("BotToken = %q, want Riggs' own .env to win", p.BotToken)
	}
}

// The inversion must hold for the process environment too, not just for the
// config's ${VAR} expansion — anything Riggs shells out to should see the same
// values the config resolved from.
func TestOverloadReachesTheProcessEnvironment(t *testing.T) {
	t.Setenv("TEST_RIGGS_OVERLOAD", "from-env")
	path := writeConfig(t, "admin:\n  slack-user-id: U1\n", "TEST_RIGGS_OVERLOAD=from-file\n")

	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := os.Getenv("TEST_RIGGS_OVERLOAD"); got != "from-file" {
		t.Fatalf("os.Getenv = %q, want the file value", got)
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

// The documented default, pinned. Every other test here uses a temp directory,
// so without this one a refactor could quietly move the file Riggs reads and
// nothing would notice until a launch agent came up with no tokens.
func TestDefaultEnvPathIsUnderTheRiggsConfigDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	want := filepath.Join(home, ".config", "riggs", EnvFileName)
	if got := DefaultEnvPath(); got != want {
		t.Fatalf("DefaultEnvPath = %q, want %q", got, want)
	}
	// And it must be the sibling of the default config, not a parallel rule
	// that happens to agree today.
	if got, want := filepath.Dir(DefaultEnvPath()), filepath.Dir(DefaultPath()); got != want {
		t.Fatalf("env dir %q is not the config dir %q", got, want)
	}
}

// With nothing configured at all — no config file on disk — Riggs must still
// look in the default location rather than giving up. This is the state a fresh
// machine is in, and the state `riggs capabilities` is run in to diagnose it.
func TestNoConfigFileStillLooksInTheDefaultDir(t *testing.T) {
	t.Setenv("RIGGS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	got, explicit := (&Config{}).envFilePath(candidatePaths()[0])
	if got != DefaultEnvPath() {
		t.Fatalf("envFilePath = %q, want %q", got, DefaultEnvPath())
	}
	if explicit {
		t.Error("the default location reported itself as explicitly named")
	}
}

// "Unless otherwise specified" — the three ways to move it, in precedence
// order. Each must win over the default.
func TestEnvFileLocationCanBeMoved(t *testing.T) {
	t.Run("env-file wins outright", func(t *testing.T) {
		cfg := &Config{EnvFile: "/opt/secrets/riggs.env"}
		got, explicit := cfg.envFilePath("/anywhere/config.yaml")
		if got != "/opt/secrets/riggs.env" || !explicit {
			t.Fatalf("envFilePath = %q (explicit=%v)", got, explicit)
		}
	})

	// The ledger already follows the config file (§10); the environment does
	// too, so moving a config moves the whole of its state.
	t.Run("otherwise it follows the config file", func(t *testing.T) {
		got, _ := (&Config{}).envFilePath("/opt/riggs/config.yaml")
		if got != filepath.Join("/opt/riggs", EnvFileName) {
			t.Fatalf("envFilePath = %q", got)
		}
	})

	t.Run("RIGGS_CONFIG moves it too", func(t *testing.T) {
		t.Setenv("RIGGS_CONFIG", "/srv/riggs/config.yaml")
		got, _ := (&Config{}).envFilePath(candidatePaths()[0])
		if got != filepath.Join("/srv/riggs", EnvFileName) {
			t.Fatalf("envFilePath = %q", got)
		}
	})
}

// The resolved path is reported whether or not the file was there, because
// "which file did you look in" is the question being asked when it was not.
func TestEnvPathIsReportedEvenWhenAbsent(t *testing.T) {
	path := writeConfig(t, "admin:\n  slack-user-id: U1\n", "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EnvLoaded() {
		t.Error("EnvLoaded is true with no .env on disk")
	}
	if cfg.EnvPath() != filepath.Join(filepath.Dir(path), EnvFileName) {
		t.Fatalf("EnvPath = %q", cfg.EnvPath())
	}

	cfg2, err := Load(writeConfig(t, "admin:\n  slack-user-id: U1\n", "X=1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg2.EnvLoaded() {
		t.Error("EnvLoaded is false after loading a .env")
	}
}
