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
	"time"

	"github.com/google/jsonschema-go/jsonschema"
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
	resolver     *slack.Resolver
	newer        Factory
	defaultLogin string
}

// New builds the tool.
func New(resolver *slack.Resolver, defaultLogin string, newer Factory) *Tool {
	return &Tool{resolver: resolver, newer: newer, defaultLogin: defaultLogin}
}

// Name is the registry key. On the CLI this is spelled `riggs git pr --bulk`.
func (t *Tool) Name() string { return "git.pr.bulk" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	return "Deliver pull requests awaiting a user's review as a single bulk digest message."
}

// PrimaryArg binds the verb flag's value: `--bulk <user>`.
func (t *Tool) PrimaryArg() string { return "user" }

// InputSchema declares the parameters.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"user": {
				Type:        "string",
				Description: "GitHub login whose review requests are delivered. Defaults to admin.github-login.",
			},
			"dry_run": {
				Type:        "boolean",
				Description: "Report what would change without sending anything or writing state.",
			},
			"max_items": {
				Type:        "integer",
				Description: "Cap on one digest. Defaults to $RIGGS_BULK_MAX_ITEMS, then 10.",
			},
			"cooldown": {
				Type:        "string",
				Description: "How long an item waits before it may move into a new digest, e.g. \"3h\". Defaults to 3h.",
			},
			"slack_profile": {
				Type:        "string",
				Description: "Which configured Slack account to post through. Should be the profile the daemon listens as, so clicks come back to Riggs.",
			},
			"slack_channel": {
				Type:        "string",
				Description: "Target channel id. Omitted, the digest is DMed to the configured admin.",
			},
		},
	}
}

// Invoke runs one digest pass.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	login, _ := args["user"].(string)
	if login == "" {
		login = t.defaultLogin
	}
	if login == "" {
		return nil, fmt.Errorf("no GitHub login: pass one, or set admin.github-login")
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
