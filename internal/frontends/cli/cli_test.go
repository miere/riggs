package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/tools"
)

// fakeTool records the args it was invoked with so the tests can assert on how
// the command line was translated.
type fakeTool struct {
	name    string
	schema  *jsonschema.Schema
	primary string
	got     map[string]any
}

func (f *fakeTool) Name() string                    { return f.name }
func (f *fakeTool) Description() string             { return "fake" }
func (f *fakeTool) InputSchema() *jsonschema.Schema { return f.schema }
func (f *fakeTool) PrimaryArg() string              { return f.primary }
func (f *fakeTool) Invoke(_ context.Context, a map[string]any) (any, error) {
	f.got = a
	return "done", nil
}

// props builds an object schema over the named string properties.
func props(names ...string) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
	for _, n := range names {
		s.Properties[n] = &jsonschema.Schema{Type: "string"}
	}
	return s
}

// registry wires the shapes the real tool surface will have: a flat tool, a
// two-part namespaced tool, and verb-flag tools under a two-part prefix.
func registry(t *testing.T) (*tools.Registry, map[string]*fakeTool) {
	t.Helper()
	fakes := map[string]*fakeTool{
		"ping":                 {name: "ping"},
		"jira.tickets":         {name: "jira.tickets", schema: props("query", "slack_profile", "slack_channel")},
		"git.pr.approve":       {name: "git.pr.approve", schema: props("pr", "slack_channel"), primary: "pr"},
		"git.pr.approve-merge": {name: "git.pr.approve-merge", schema: props("pr"), primary: "pr"},
		"git.pr.fetch-reviews": {name: "git.pr.fetch-reviews", schema: props("user", "slack_channel"), primary: "user"},
	}
	reg := tools.NewRegistry()
	for _, name := range []string{"ping", "jira.tickets", "git.pr.approve", "git.pr.approve-merge", "git.pr.fetch-reviews"} {
		reg.Register(fakes[name])
	}
	return reg, fakes
}

// run executes argv against a fresh frontend and returns stdout.
func run(t *testing.T, argv ...string) (string, map[string]*fakeTool, error) {
	t.Helper()
	reg, fakes := registry(t)
	var out, errOut bytes.Buffer
	err := New(reg).WithOutput(&out, &errOut).Run(context.Background(), argv)
	return strings.TrimSpace(out.String()), fakes, err
}

func TestFlatCommand(t *testing.T) {
	out, _, err := run(t, "ping")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "done" {
		t.Errorf("stdout = %q, want %q", out, "done")
	}
}

func TestDottedCommand(t *testing.T) {
	_, fakes, err := run(t, "jira", "tickets", "--query", "project = NYX", "--slack-profile", "nc")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fakes["jira.tickets"].got
	if got["query"] != "project = NYX" {
		t.Errorf("query = %v, want the JQL", got["query"])
	}
	// --kebab-case maps to the snake_case property the schema declares.
	if got["slack_profile"] != "nc" {
		t.Errorf("slack_profile = %v, want nc", got["slack_profile"])
	}
}

// The headline form: a verb flag completes the tool name and carries the
// operation's primary argument as its own value.
func TestVerbFlagBindsPrimaryArgument(t *testing.T) {
	_, fakes, err := run(t, "git", "pr", "--approve", "acme/monolith#20069")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fakes["git.pr.approve"].got
	if got["pr"] != "acme/monolith#20069" {
		t.Errorf("pr = %v, want the ref bound from the verb flag", got["pr"])
	}
	if fakes["git.pr.approve-merge"].got != nil {
		t.Error("--approve also invoked approve-merge; the verbs must not overlap")
	}
}

// A verb flag coexists with ordinary parameter flags, in any order.
func TestVerbFlagWithOtherFlags(t *testing.T) {
	_, fakes, err := run(t, "git", "pr", "--slack-channel", "C123", "--approve", "owner/repo#7")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fakes["git.pr.approve"].got
	if got["pr"] != "owner/repo#7" || got["slack_channel"] != "C123" {
		t.Errorf("args = %v, want both the verb's value and the channel flag", got)
	}
}

// The primary argument is optional: `--fetch-reviews` with no value must still
// resolve, so the tool can fall back to admin.github-login.
func TestVerbFlagWithoutValue(t *testing.T) {
	_, fakes, err := run(t, "git", "pr", "--fetch-reviews")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fakes["git.pr.fetch-reviews"].got
	if _, present := got["user"]; present {
		t.Errorf("user = %v, want it absent so the tool applies its own default", got["user"])
	}
}

// A following flag must not be mistaken for the verb's value.
func TestVerbFlagValueIsNotTheNextFlag(t *testing.T) {
	_, fakes, err := run(t, "git", "pr", "--fetch-reviews", "--slack-channel", "C9")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := fakes["git.pr.fetch-reviews"].got
	if _, present := got["user"]; present {
		t.Errorf("user = %v, want it absent — --slack-channel is a flag, not a value", got["user"])
	}
	if got["slack_channel"] != "C9" {
		t.Errorf("slack_channel = %v, want C9", got["slack_channel"])
	}
}

func TestVerbFlagEqualsForm(t *testing.T) {
	_, fakes, err := run(t, "git", "pr", "--approve-merge=owner/repo#3")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fakes["git.pr.approve-merge"].got; got["pr"] != "owner/repo#3" {
		t.Errorf("pr = %v, want the value after =", got["pr"])
	}
}

// An ordinary parameter flag must never be mistaken for a verb: verbs are
// matched against the registry, not against a list of known spellings.
func TestParameterFlagIsNotAVerb(t *testing.T) {
	_, _, err := run(t, "git", "pr", "--slack-channel", "C123")
	if err == nil {
		t.Fatal("Run = nil error, want `git pr` with no verb to be unknown")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q, want an unknown-command failure", err)
	}
}

func TestUnknownCommand(t *testing.T) {
	if _, _, err := run(t, "nope"); err == nil {
		t.Fatal("Run(nope) = nil error, want a failure")
	}
}

func TestNoArgsIsUsage(t *testing.T) {
	if _, _, err := run(t); err != ErrUsage {
		t.Fatalf("Run() = %v, want ErrUsage", err)
	}
}

// --json-output is a frontend flag: stripped anywhere on the line, never
// offered to the tool's schema.
func TestJSONOutput(t *testing.T) {
	out, _, err := run(t, "git", "pr", "--json-output", "--approve", "owner/repo#1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != `"done"` {
		t.Errorf("stdout = %q, want the JSON-quoted result", out)
	}
}

func TestUnknownFlagIsRejected(t *testing.T) {
	_, _, err := run(t, "jira", "tickets", "--nope", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("err = %v, want an unknown-flag failure", err)
	}
}
