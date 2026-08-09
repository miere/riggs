package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/tools"
)

// A machine with nothing provisioned must still boot: the notifying tools
// report their own missing credentials, they do not stop the binary.
func TestNewWithNoConfig(t *testing.T) {
	t.Setenv("RIGGS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	a, err := New(ModeCLI, []string{"ping"}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := a.registry.Get("ping"); !ok {
		t.Error("ping is not registered on an unprovisioned machine")
	}
	if _, ok := a.registry.Get("capabilities"); !ok {
		t.Error("capabilities is not registered on an unprovisioned machine")
	}
	if err := a.Run(context.Background()); err != nil {
		t.Errorf("Run: %v", err)
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

type fakeTool struct{ name string }

func (f *fakeTool) Name() string                    { return f.name }
func (f *fakeTool) Description() string             { return "fake" }
func (f *fakeTool) InputSchema() *jsonschema.Schema { return nil }
func (f *fakeTool) Invoke(context.Context, map[string]any) (any, error) {
	return nil, nil
}

// The usage line spells each tool the way it is typed: flat, dotted, or — for
// three-part names — as a verb flag under its namespace.
func TestUsageLineSpellsVerbFlags(t *testing.T) {
	reg := tools.NewRegistry()
	for _, n := range []string{"ping", "jira.tickets", "git.pr.approve", "git.pr.fetch-reviews"} {
		reg.Register(&fakeTool{name: n})
	}
	a := &Application{registry: reg}

	got := a.UsageLine()
	for _, want := range []string{"ping", "jira <tickets>", "git pr <--approve|--fetch-reviews>", "mcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage line %q is missing %q", got, want)
		}
	}
}

// A Slack-backed tool is registered only when there is an account to post
// through; `capabilities` explains the absence.
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
	if _, ok := a.registry.Get("slack.send-msg"); !ok {
		t.Error("slack.send-msg absent despite a configured profile")
	}

	b, err := New(ModeCLI, nil, bare)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := b.registry.Get("slack.send-msg"); ok {
		t.Error("slack.send-msg registered with no Slack profile configured")
	}
	if _, ok := b.registry.Get("capabilities"); !ok {
		t.Error("capabilities must remain available to explain the absence")
	}
}
