package tickets

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/ticket"
)

// Store is the ledger seam the importer writes through.
type Store interface {
	SaveCard(ctx context.Context, key string, e notify.Entry) error
	SetLatch(ctx context.Context, key, name string, t time.Time) error
	SaveSummary(ctx context.Context, key, text string, at time.Time) error
}

// StoreFactory opens the ledger for one invocation.
type StoreFactory func(ctx context.Context) (Store, func() error, error)

// ImportTool migrates the Python automation's state file into the ledger.
//
// Without it, cutover would re-advertise every live ticket and — because the
// nudge timestamps would be lost too — ping the admin on all of them at once.
type ImportTool struct {
	newer StoreFactory
	now   func() time.Time
}

// NewImport builds the importer.
func NewImport(f StoreFactory) *ImportTool { return &ImportTool{newer: f, now: time.Now} }

// Name is the registry key; on the CLI, `riggs jira tickets --import-state <path>`.
func (t *ImportTool) Name() string { return "jira.tickets.import-state" }

// Description is the one-line hint shown to MCP clients.
func (t *ImportTool) Description() string {
	return "Import the Python quick-coding-tasks state file into the ledger."
}

// PrimaryArg binds the verb flag's value.
func (t *ImportTool) PrimaryArg() string { return "from" }

// InputSchema declares the parameters.
func (t *ImportTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"from": {Type: "string", Description: "Path to tickets.json."},
			"slack_channel": {
				Type: "string",
				Description: "Channel the existing cards were posted in. The Python kept " +
					"this in its config rather than per card, so it must be supplied.",
			},
			"slack_profile": {Type: "string", Description: "Profile recorded against the imported cards."},
			"dry_run":       {Type: "boolean", Description: "Report what would be imported without writing."},
		},
		Required: []string{"from", "slack_channel"},
	}
}

// ImportResult reports what the import did.
type ImportResult struct {
	Source    string   `json:"source"`
	Total     int      `json:"total"`
	Imported  int      `json:"imported"`
	Live      int      `json:"live"`
	Summaries int      `json:"summaries"`
	Nudges    int      `json:"nudges"`
	Skipped   []string `json:"skipped,omitempty"`
	DryRun    bool     `json:"dry_run"`
}

// String renders the result for a human.
func (r ImportResult) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[dry run] ")
	}
	fmt.Fprintf(&b, "%s: %d entries, %d imported (%d still live), %d summaries, %d nudge timestamps",
		r.Source, r.Total, r.Imported, r.Live, r.Summaries, r.Nudges)
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "\nskipped %d without a Slack timestamp", len(r.Skipped))
	}
	if r.Live > 0 && !r.DryRun {
		fmt.Fprintf(&b, "\n\nThe %d live card(s) carry no fingerprint, so the next poll will "+
			"edit each once to adopt it.", r.Live)
	}
	return b.String()
}

// Invoke performs the import.
func (t *ImportTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["from"].(string)
	if path == "" {
		return nil, fmt.Errorf("from is required: the path to tickets.json")
	}
	channel, _ := args["slack_channel"].(string)
	if channel == "" {
		return nil, fmt.Errorf("slack_channel is required: the Python stored it in its config, " +
			"not per card, so the imported cards have no channel without it")
	}
	profile, _ := args["slack_profile"].(string)
	if profile == "" {
		profile = "default"
	}
	dryRun, _ := args["dry_run"].(bool)

	state, err := ticket.LoadLegacyState(path)
	if err != nil {
		return nil, err
	}
	result := ImportResult{Source: path, Total: len(state), DryRun: dryRun}

	var store Store
	if !dryRun {
		s, closer, err := t.newer(ctx)
		if err != nil {
			return nil, err
		}
		if closer != nil {
			defer closer()
		}
		store = s
	}

	now := t.now()
	for _, issueKey := range state.Keys() {
		entry := state[issueKey]
		// No timestamp means the card was never actually posted.
		if entry.TS == "" {
			result.Skipped = append(result.Skipped, issueKey)
			continue
		}
		key := ticket.Key(issueKey)
		st := entry.State()
		result.Imported++
		if st.Live() {
			result.Live++
		}
		if entry.GoalSummary != "" {
			result.Summaries++
		}
		nudgedAt, nudged := entry.NudgedAt()
		if nudged {
			result.Nudges++
		}
		if dryRun {
			continue
		}

		if err := store.SaveCard(ctx, key, notify.Entry{
			Profile: profile, Channel: channel, TS: entry.TS,
			// No fingerprint: the Python never recorded one. See the PR
			// importer for why inventing a value would be worse.
			Fingerprint: "", State: string(st), UpdatedAt: now,
		}); err != nil {
			return nil, err
		}
		if entry.GoalSummary != "" {
			if err := store.SaveSummary(ctx, key, entry.GoalSummary, now); err != nil {
				return nil, err
			}
		}
		// Carry the nudge clock across, or every stale card would be pinged
		// again on the first scheduled run.
		if nudged {
			if err := store.SetLatch(ctx, key, "nudge", nudgedAt); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
