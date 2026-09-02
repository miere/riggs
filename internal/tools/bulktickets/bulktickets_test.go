package bulktickets

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/ticket"
)

// fakeEngine records the pass it was asked for.
type fakeEngine struct {
	target slack.Target
	dryRun bool
	ran    bool
	err    error
}

func (f *fakeEngine) Run(_ context.Context, target slack.Target, dryRun bool) (ticket.BulkReport, error) {
	f.ran, f.target, f.dryRun = true, target, dryRun
	return ticket.BulkReport{Considered: 1, Noun: "ticket"}, f.err
}

// newTool wires the tool over a fake engine, recording what the factory was
// given.
func newTool(t *testing.T, engine *fakeEngine) (*Tool, *struct {
	jql  string
	opts ticket.BulkOptions
}) {
	t.Helper()
	seen := &struct {
		jql  string
		opts ticket.BulkOptions
	}{}
	cfg := &config.Config{
		Admin: config.Admin{SlackUserID: "UADMIN01"},
		Slack: config.Slack{Profiles: map[string]config.Profile{
			"default": {BotToken: "xoxb-test"},
		}},
	}
	tool := New(slack.NewResolver(cfg), func(_ context.Context, jql string, opts ticket.BulkOptions) (Engine, io.Closer, error) {
		seen.jql, seen.opts = jql, opts
		return engine, nil, nil
	})
	return tool, seen
}

// A missing query is refused before the ledger is opened, so a bad argument
// costs nothing.
func TestInvokeRefusesAnEmptyQuery(t *testing.T) {
	engine := &fakeEngine{}
	tool, _ := newTool(t, engine)

	if _, err := tool.Invoke(context.Background(), map[string]any{"query": "   "}); err == nil {
		t.Fatal("Invoke succeeded with no query")
	}
	if engine.ran {
		t.Fatal("the engine was built for an invalid call")
	}
}

func TestInvokePassesTheTunablesThrough(t *testing.T) {
	engine := &fakeEngine{}
	tool, seen := newTool(t, engine)

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"query":     "project = NYX",
		"max_items": float64(4), // JSON numbers arrive as float64 over MCP.
		"cooldown":  "2h",
		"dry_run":   true,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	if seen.jql != "project = NYX" {
		t.Fatalf("jql = %q", seen.jql)
	}
	if seen.opts.MaxItems != 4 || seen.opts.Cooldown != 2*time.Hour {
		t.Fatalf("opts = %+v", seen.opts)
	}
	if !engine.dryRun {
		t.Fatal("dry_run was not passed through")
	}
}

// Absent tunables stay zero, so the engine applies the environment and then the
// defaults rather than the tool second-guessing them.
func TestInvokeLeavesAbsentTunablesZero(t *testing.T) {
	tool, seen := newTool(t, &fakeEngine{})

	if _, err := tool.Invoke(context.Background(), map[string]any{"query": "project = NYX"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if seen.opts.MaxItems != 0 || seen.opts.Cooldown != 0 {
		t.Fatalf("opts = %+v, want zero", seen.opts)
	}
}

func TestInvokeRejectsAnUnparseableCooldown(t *testing.T) {
	tool, _ := newTool(t, &fakeEngine{})

	_, err := tool.Invoke(context.Background(), map[string]any{
		"query": "project = NYX", "cooldown": "soon",
	})
	if err == nil {
		t.Fatal("Invoke accepted a nonsense cooldown")
	}
}

func TestInvokeSurfacesTheEngineError(t *testing.T) {
	engine := &fakeEngine{err: errors.New("jira is down")}
	tool, _ := newTool(t, engine)

	if _, err := tool.Invoke(context.Background(), map[string]any{"query": "project = NYX"}); err == nil {
		t.Fatal("Invoke swallowed the engine error")
	}
}
