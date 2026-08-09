// Package importstate migrates the Python mirror's state file into the ledger.
//
// This is the step that makes cutover safe. The file holds hundreds of cards
// that are already posted in #nc-code-reviews; without importing them Riggs
// would consider every one new and re-post the lot.
package importstate

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/pullrequest"
)

// Store is the ledger seam.
type Store interface {
	SaveCard(ctx context.Context, key string, e notify.Entry) error
	SetLatch(ctx context.Context, key, name string, t time.Time) error
	SaveSummary(ctx context.Context, key, text string, at time.Time) error
}

// Factory opens the ledger for one invocation.
type Factory func(ctx context.Context) (Store, func() error, error)

// Tool is `git.pr.import-state`.
type Tool struct {
	newer Factory
	now   func() time.Time
}

// New builds the tool.
func New(newer Factory) *Tool { return &Tool{newer: newer, now: time.Now} }

// Name is the registry key; on the CLI, `riggs git pr --import-state <path>`.
func (t *Tool) Name() string { return "git.pr.import-state" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	return "Import the Python review-queue state file into the ledger, so cutover does not re-post cards."
}

// PrimaryArg binds the verb flag's value: `--import-state <path>`.
func (t *Tool) PrimaryArg() string { return "from" }

// InputSchema declares the parameters.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"from": {
				Type:        "string",
				Description: "Path to github_review_queue.json.",
			},
			"slack_profile": {
				Type:        "string",
				Description: "Profile name recorded against the imported cards. Defaults to \"default\".",
			},
			"dry_run": {
				Type:        "boolean",
				Description: "Report what would be imported without writing anything.",
			},
		},
		Required: []string{"from"},
	}
}

// Result reports what the import did.
type Result struct {
	Source string `json:"source"`
	// Total is every entry in the file.
	Total int `json:"total"`
	// Imported is the entries written (those with a usable Slack ts).
	Imported int `json:"imported"`
	// Live is how many of those can still change, and so will be re-fetched.
	Live int `json:"live"`
	// Tagged and Summaries count the latches and cached summaries carried over.
	Tagged    int      `json:"tagged"`
	Summaries int      `json:"summaries"`
	Skipped   []string `json:"skipped,omitempty"`
	DryRun    bool     `json:"dry_run"`
}

// String renders the result for a human.
func (r Result) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[dry run] ")
	}
	fmt.Fprintf(&b, "%s: %d entries, %d imported (%d still live), %d tags, %d summaries",
		r.Source, r.Total, r.Imported, r.Live, r.Tagged, r.Summaries)
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "\nskipped %d without a Slack timestamp:", len(r.Skipped))
		for _, s := range r.Skipped {
			b.WriteString("\n  " + s)
		}
	}
	if r.Live > 0 && !r.DryRun {
		fmt.Fprintf(&b, "\n\nThe %d live card(s) carry no fingerprint, so the next run will "+
			"edit each once to adopt it. Terminal cards are never re-fetched.", r.Live)
	}
	return b.String()
}

// Invoke performs the import.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["from"].(string)
	if path == "" {
		return nil, fmt.Errorf("from is required: the path to github_review_queue.json")
	}
	profile, _ := args["slack_profile"].(string)
	if profile == "" {
		profile = "default"
	}
	dryRun, _ := args["dry_run"].(bool)

	state, err := pullrequest.LoadLegacyState(path)
	if err != nil {
		return nil, err
	}

	result := Result{Source: path, Total: len(state), DryRun: dryRun}

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
	for _, ref := range state.Refs() {
		entry := state[ref]
		// An entry with no timestamp was never actually posted (a failed
		// send). Importing it would claim a card that does not exist.
		if entry.TS == "" {
			result.Skipped = append(result.Skipped, ref)
			continue
		}
		key := pullrequest.Key(ref)
		result.Imported++
		if !entry.Terminal() {
			result.Live++
		}
		if entry.Tagged {
			result.Tagged++
		}
		if entry.Summary != nil && *entry.Summary != "" {
			result.Summaries++
		}
		if dryRun {
			continue
		}

		if err := store.SaveCard(ctx, key, notify.Entry{
			Profile: profile,
			Channel: entry.Channel,
			TS:      entry.TS,
			// No fingerprint: the Python never recorded one, and inventing a
			// value would make an unchanged card look unchanged by luck.
			// Leaving it empty means one honest edit per live card.
			Fingerprint: "",
			State:       entry.Token(),
			UpdatedAt:   now,
		}); err != nil {
			return nil, err
		}
		// Carry the latch across, or every already-tagged card would ping again.
		if entry.Tagged {
			if err := store.SetLatch(ctx, key, "tagged", now); err != nil {
				return nil, err
			}
		}
		if entry.Summary != nil && *entry.Summary != "" {
			if err := store.SaveSummary(ctx, key, *entry.Summary, now); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
