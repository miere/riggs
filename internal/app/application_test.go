package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A machine with nothing provisioned must still boot: the notifying tools
// report their own missing credentials, they do not stop the binary.
func TestNewWithNoConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RIGGS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Isolate HOME as well, or this exercises whatever config the developer
	// happens to have rather than an unprovisioned machine.
	t.Setenv("HOME", dir)

	a, err := New(ModeCLI, nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Neither digest is built: both need a Slack account to post through, and
	// an unprovisioned machine has none. That is a capability gap, not a boot
	// failure.
	if a.reviews != nil || a.tickets != nil {
		t.Error("a digest was built on an unprovisioned machine")
	}
}

// A broken config IS fatal — unlike a missing credential, it means the operator
// wrote something they believe is in effect.
func TestNewWithBrokenConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("admin:\n  nope: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ModeCLI, nil, path); err == nil {
		t.Fatal("New(broken config) = nil error, want a failure")
	}
}

// The usage line names both digests in the spelling stored jobs actually use.
// It used to be reconstructed from registry names by splitting on dots; the
// spelling is a contract with the scheduler, so it is asserted rather than
// derived.
func TestUsageLineNamesTheTwoDigests(t *testing.T) {
	usage := (&Application{}).UsageLine()
	for _, want := range []string{"git pr --bulk", "jira tickets --bulk", "capabilities", "daemon"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage = %q, want it to mention %q", usage, want)
		}
	}
	if strings.Contains(usage, "mcp") {
		t.Errorf("usage still offers mcp: %q", usage)
	}
}

func TestSlackToolsGatedOnProfiles(t *testing.T) {
	dir := t.TempDir()
	withProfile := filepath.Join(dir, "with.yaml")
	if err := os.WriteFile(withProfile, []byte(
		"slack:\n  profiles:\n    default:\n      bot-token: xoxb-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "bare.yaml")
	if err := os.WriteFile(bare, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := New(ModeCLI, nil, withProfile)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.reviews == nil {
		t.Error("the pull-request digest is absent despite a configured profile")
	}

	b, err := New(ModeCLI, nil, bare)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.reviews != nil {
		t.Error("the digest was built with no Slack profile configured")
	}
}
