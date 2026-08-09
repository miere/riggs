// Package fetchreviews mirrors GitHub review requests into Slack — the
// every-1m job that replaces `pull_request/main.py review-queue`.
package fetchreviews

import (
	"context"
	"fmt"
	"io"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Engine is the reconciler seam, so the tool can be tested without a ledger.
type Engine interface {
	Run(ctx context.Context, target slack.Target, dryRun bool) (pullrequest.Report, error)
}

// Factory builds the engine (and whatever must be closed afterwards) for one
// invocation. It is a factory rather than a field because opening the ledger
// on every `riggs ping` would be waste — and would create the database as a
// side effect of an unrelated command.
type Factory func(ctx context.Context, login string) (Engine, io.Closer, error)

// Tool is `git.pr.fetch-reviews`.
type Tool struct {
	resolver *slack.Resolver
	newer    Factory
	// defaultLogin is admin.github-login, used when the verb flag carries no
	// value.
	defaultLogin string
}

// New builds the tool.
func New(resolver *slack.Resolver, defaultLogin string, newer Factory) *Tool {
	return &Tool{resolver: resolver, newer: newer, defaultLogin: defaultLogin}
}

// Name is the registry key. On the CLI this is spelled
// `riggs git pr --fetch-reviews [user]`.
func (t *Tool) Name() string { return "git.pr.fetch-reviews" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	return "Mirror open pull requests awaiting a user's review into Slack as self-updating cards."
}

// PrimaryArg binds the verb flag's value: `--fetch-reviews <user>`.
func (t *Tool) PrimaryArg() string { return "user" }

// InputSchema declares the parameters.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"user": {
				Type:        "string",
				Description: "GitHub login whose review requests are mirrored. Defaults to admin.github-login.",
			},
			"dry_run": {
				Type:        "boolean",
				Description: "Report what would change without sending anything or writing state.",
			},
			"slack_profile": {
				Type:        "string",
				Description: "Which configured Slack account to post through. Defaults to \"default\".",
			},
			"slack_channel": {
				Type:        "string",
				Description: "Target channel id. Omitted, cards are DMed to the configured admin.",
			},
		},
	}
}

// Invoke runs one reconcile pass.
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

	target, err := t.resolver.Resolve(profile, channel)
	if err != nil {
		return nil, err
	}

	engine, closer, err := t.newer(ctx, login)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	return engine.Run(ctx, target, dryRun)
}
