// Package approve handles the card's Approve and Approve & Merge buttons.
//
// Murtaugh's gateway matches the click and shells this; Riggs never receives
// the interaction itself. The two operations are separate registered tools so
// each workflow rule names exactly one, and neither can be reached by
// accident from the other's payload.
package approve

import (
	"context"
	"fmt"
	"io"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Approver is the seam performing the approval.
type Approver interface {
	Run(ctx context.Context, ref string, merge bool, target slack.Target, threadTS string) (pullrequest.ApproveResult, error)
	DryRun(ctx context.Context, ref string, merge bool, target slack.Target, threadTS string) (pullrequest.ApproveResult, error)
}

// Factory builds the approver for one invocation.
type Factory func(ctx context.Context) (Approver, io.Closer, error)

// Tool is `git.pr.approve` or `git.pr.approve-merge`.
type Tool struct {
	resolver *slack.Resolver
	newer    Factory
	// merge distinguishes the two registrations. It is fixed at construction
	// rather than read from the arguments, so a payload can never turn an
	// approval into a merge.
	merge bool
}

// New builds the plain approve tool.
func New(resolver *slack.Resolver, newer Factory) *Tool {
	return &Tool{resolver: resolver, newer: newer}
}

// NewMerge builds the approve-and-rebase-merge tool.
func NewMerge(resolver *slack.Resolver, newer Factory) *Tool {
	return &Tool{resolver: resolver, newer: newer, merge: true}
}

// Name is the registry key.
func (t *Tool) Name() string {
	if t.merge {
		return "git.pr.approve-merge"
	}
	return "git.pr.approve"
}

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	if t.merge {
		return "Approve a pull request, verify it registered, then rebase-merge it."
	}
	return "Approve a pull request and verify the approval registered on GitHub."
}

// PrimaryArg binds the verb flag's value: `--approve <owner/repo#n>`.
func (t *Tool) PrimaryArg() string { return "pr" }

// InputSchema declares the parameters.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"pr": {
				Type:        "string",
				Description: "Pull request ref, as owner/repo#number.",
			},
			"thread": {
				Type: "string",
				Description: "Message ts to reply under. Omitted, the outcome is posted " +
					"on the tracked card's own thread.",
			},
			"slack_profile": {
				Type:        "string",
				Description: "Which configured Slack account to post through. Defaults to \"default\".",
			},
			"slack_channel": {
				Type:        "string",
				Description: "Channel to reply in. Omitted, the tracked card's channel is used.",
			},
			"dry_run": {
				Type:        "boolean",
				Description: "Report what would happen without approving, merging or posting.",
			},
		},
		Required: []string{"pr"},
	}
}

// Invoke performs the approval.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	ref, _ := args["pr"].(string)
	if ref == "" {
		return nil, fmt.Errorf("pr is required, as owner/repo#number")
	}
	// Fail before touching GitHub if the ref is malformed: approving the wrong
	// pull request is not recoverable from Slack.
	if _, _, err := pullrequest.SplitRef(ref); err != nil {
		return nil, err
	}
	thread, _ := args["thread"].(string)
	profile, _ := args["slack_profile"].(string)
	channel, _ := args["slack_channel"].(string)

	// A channel is only needed when we cannot fall back to the card, so an
	// unresolvable target is not fatal here — the approval still happens, it
	// just goes unannounced.
	target, err := t.resolver.Resolve(profile, channel)
	if err != nil && thread != "" {
		return nil, err
	}

	approver, closer, err := t.newer(ctx)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	if dry, _ := args["dry_run"].(bool); dry {
		return approver.DryRun(ctx, ref, t.merge, target, thread)
	}
	return approver.Run(ctx, ref, t.merge, target, thread)
}
