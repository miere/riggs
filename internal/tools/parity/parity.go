// Package parity checks the Go port against the Python it replaces.
//
// This is the phase-2 gate. The Python's state file records, per pull request,
// the state it last derived. For every entry that can still change, this
// re-derives the state from live GitHub and compares. A disagreement is a
// porting bug — found here, deliberately, rather than in production when a
// card silently stops appearing.
//
// It reads only: no Slack, no ledger writes.
package parity

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/pullrequest"
)

// Resolver derives a pull request's state from GitHub.
type Resolver interface {
	Resolve(ctx context.Context, repo string, number int) (pullrequest.Resolved, error)
}

// Factory builds the resolver for one invocation.
type Factory func(ctx context.Context, login string) (Resolver, io.Closer, error)

// Tool is `git.pr.check-parity`.
type Tool struct {
	newer Factory
}

// New builds the tool.
func New(newer Factory) *Tool {
	return &Tool{newer: newer}
}

// Name is the registry key; on the CLI, `riggs git pr --check-parity <path>`.
func (t *Tool) Name() string { return "git.pr.check-parity" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	return "Compare Riggs' derived pull-request states against the Python mirror's recorded state."
}

// PrimaryArg binds the verb flag's value: `--check-parity <path>`.
func (t *Tool) PrimaryArg() string { return "against" }

// InputSchema declares the parameters.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"against": {
				Type:        "string",
				Description: "Path to the Python mirror's github_review_queue.json.",
			},
			"user": {
				Type:        "string",
				Description: "GitHub login to derive against. Required.",
			},
			"limit": {
				Type:        "integer",
				Description: "Check at most this many entries. 0 checks every live one.",
			},
		},
		Required: []string{"against"},
	}
}

// Comparison is one pull request's verdict.
type Comparison struct {
	Ref string `json:"ref"`
	// Python is the state the Python mirror last recorded.
	Python string `json:"python"`
	// Riggs is the state this build derives from live GitHub.
	Riggs string `json:"riggs"`
	Match bool   `json:"match"`
	Error string `json:"error,omitempty"`
}

// Result is the whole comparison.
type Result struct {
	Source string `json:"source"`
	// Live is how many entries could still change, and so were checked.
	Live       int          `json:"live"`
	Checked    int          `json:"checked"`
	Matched    int          `json:"matched"`
	Mismatched int          `json:"mismatched"`
	Errors     int          `json:"errors"`
	Details    []Comparison `json:"details"`
}

// Passed reports whether every checked entry agreed.
func (r Result) Passed() bool { return r.Mismatched == 0 && r.Errors == 0 && r.Checked > 0 }

// String renders the result for a human, mismatches first — they are the
// reason to run this.
func (r Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "parity against %s\n", r.Source)
	fmt.Fprintf(&b, "  %d live entr(ies), %d checked: %d matched, %d mismatched, %d error(s)\n",
		r.Live, r.Checked, r.Matched, r.Mismatched, r.Errors)

	sorted := append([]Comparison(nil), r.Details...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return rank(sorted[i]) < rank(sorted[j])
	})
	for _, c := range sorted {
		switch {
		case c.Error != "":
			fmt.Fprintf(&b, "  [err ] %-40s %s\n", c.Ref, c.Error)
		case !c.Match:
			fmt.Fprintf(&b, "  [DIFF] %-40s python=%-18s riggs=%s\n", c.Ref, c.Python, c.Riggs)
		default:
			fmt.Fprintf(&b, "  [ ok ] %-40s %s\n", c.Ref, c.Riggs)
		}
	}
	if r.Passed() {
		b.WriteString("\nPASS — the port derives what the Python derived.")
	} else {
		b.WriteString("\nFAIL — investigate the differences above before cutting over.")
	}
	return b.String()
}

// rank orders errors first, then mismatches, then agreements.
func rank(c Comparison) int {
	switch {
	case c.Error != "":
		return 0
	case !c.Match:
		return 1
	default:
		return 2
	}
}

// Invoke runs the comparison.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	path, _ := args["against"].(string)
	if path == "" {
		return nil, fmt.Errorf("against is required: the path to github_review_queue.json")
	}
	login, _ := args["user"].(string)
	if login == "" {
		return nil, fmt.Errorf("no GitHub login: pass one, e.g. `riggs git pr --check-parity <user>`")
	}
	limit := 0
	switch v := args["limit"].(type) {
	case int64:
		limit = int(v)
	case float64:
		limit = int(v)
	}

	state, err := pullrequest.LoadLegacyState(path)
	if err != nil {
		return nil, err
	}
	live := state.Live()

	resolver, closer, err := t.newer(ctx, login)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}

	result := Result{Source: path, Live: len(live)}
	for _, ref := range live {
		if limit > 0 && result.Checked >= limit {
			break
		}
		result.Checked++
		c := Comparison{Ref: ref, Python: state[ref].Token()}

		repo, number, err := pullrequest.SplitRef(ref)
		if err != nil {
			c.Error = err.Error()
			result.Errors++
			result.Details = append(result.Details, c)
			continue
		}
		resolved, err := resolver.Resolve(ctx, repo, number)
		if err != nil {
			c.Error = err.Error()
			result.Errors++
			result.Details = append(result.Details, c)
			continue
		}
		c.Riggs = resolved.State.Token()
		c.Match = c.Riggs == c.Python
		if c.Match {
			result.Matched++
		} else {
			result.Mismatched++
		}
		result.Details = append(result.Details, c)
	}
	return result, nil
}
