// Package bulkreviews delivers the review queue as a bulk digest: one message
// carrying up to N pull requests, rather than one card each.
//
// It is a sibling of fetchreviews, not a replacement. Both read the same
// GitHub and write the same ledger, in different streams, so one can be
// scheduled without disturbing the other.
package bulkreviews

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Engine is the reconciler seam, so the tool can be tested without a ledger.
type Engine interface {
	Run(ctx context.Context, target slack.Target, dryRun bool) (pullrequest.BulkReport, error)
}

// Factory builds the engine (and whatever must be closed afterwards) for one
// invocation. A factory rather than a field, for the same reason as
// fetchreviews: opening the ledger on every `riggs ping` would create the
// database as a side effect of an unrelated command.
type Factory func(ctx context.Context, login string, opts pullrequest.BulkOptions) (Engine, io.Closer, error)

// Tool is `git.pr.bulk`.
type Tool struct {
	resolver *slack.Resolver
	newer    Factory
}

// New builds the tool.
func New(resolver *slack.Resolver, newer Factory) *Tool {
	return &Tool{resolver: resolver, newer: newer}
}

// Invoke runs one digest pass.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	login, _ := args["user"].(string)
	if strings.TrimSpace(login) == "" {
		// Deliberately no fallback. It used to read admin.github-login, which
		// meant a config edit could repoint the queue at a different person
		// while the job that fetches it looked unchanged.
		return nil, fmt.Errorf("no GitHub login: pass one, e.g. `riggs git pr %s <user>`", "--bulk")
	}
	dryRun, _ := args["dry_run"].(bool)
	profile, _ := args["slack_profile"].(string)
	channel, _ := args["slack_channel"].(string)

	opts, err := bulkOptions(args)
	if err != nil {
		return nil, err
	}

	target, err := t.resolver.Resolve(profile, channel)
	if err != nil {
		return nil, err
	}

	engine, closer, err := t.newer(ctx, login, opts)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	return engine.Run(ctx, target, dryRun)
}

// bulkOptions reads the two tunables. Both are left zero when absent, so the
// engine applies the environment and then the defaults.
func bulkOptions(args map[string]any) (pullrequest.BulkOptions, error) {
	var opts pullrequest.BulkOptions

	switch n := args["max_items"].(type) {
	case float64: // JSON numbers arrive as float64 over MCP.
		opts.MaxItems = int(n)
	case int:
		opts.MaxItems = n
	}

	if raw, _ := args["cooldown"].(string); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return opts, fmt.Errorf("cooldown %q is not a duration (try \"3h\"): %w", raw, err)
		}
		if d <= 0 {
			return opts, fmt.Errorf("cooldown must be positive, got %s", d)
		}
		opts.Cooldown = d
	}
	return opts, nil
}
