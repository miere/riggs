// Package tickets exposes the Jira ticket queue as four verbs:
// poll, nudge, assign and dismiss.
//
// They are separate registered tools for the same reason the approve pair is:
// each Murtaugh job or workflow rule names exactly one operation, and nothing
// in a payload can turn one into another.
package tickets

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/ticket"
)

// Defaults for the idle nudge, matching the Python's config.
const (
	DefaultNudgeAfterHours = 24
	// minGap stops a manual re-run landing on top of a scheduled one.
	minGap = time.Hour
)

// Engine is the seam performing the work.
type Engine interface {
	Poll(ctx context.Context, jql string, target slack.Target, dryRun bool) (ticket.Report, error)
	Nudge(ctx context.Context, after, minGap time.Duration, target slack.Target, dryRun bool) (ticket.Report, error)
	Assign(ctx context.Context, issueKey, actor string, target slack.Target) (ticket.ActionResult, error)
	Dismiss(ctx context.Context, issueKey, actor string, target slack.Target) (ticket.ActionResult, error)
}

// Factory builds the engine for one invocation.
type Factory func(ctx context.Context) (Engine, io.Closer, error)

// verb distinguishes the four registrations.
type verb int

const (
	verbPoll verb = iota
	verbNudge
	verbAssign
	verbDismiss
)

// Tool is one of the ticket verbs.
type Tool struct {
	resolver *slack.Resolver
	newer    Factory
	verb     verb
}

// NewPoll, NewNudge, NewAssign and NewDismiss build the four registrations.
func NewPoll(r *slack.Resolver, f Factory) *Tool { return &Tool{resolver: r, newer: f, verb: verbPoll} }
func NewNudge(r *slack.Resolver, f Factory) *Tool {
	return &Tool{resolver: r, newer: f, verb: verbNudge}
}
func NewAssign(r *slack.Resolver, f Factory) *Tool {
	return &Tool{resolver: r, newer: f, verb: verbAssign}
}
func NewDismiss(r *slack.Resolver, f Factory) *Tool {
	return &Tool{resolver: r, newer: f, verb: verbDismiss}
}

// Name is the registry key.
func (t *Tool) Name() string {
	switch t.verb {
	case verbNudge:
		return "jira.tickets.nudge"
	case verbAssign:
		return "jira.tickets.assign"
	case verbDismiss:
		return "jira.tickets.dismiss"
	default:
		return "jira.tickets.poll"
	}
}

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	switch t.verb {
	case verbNudge:
		return "Re-ping the admin on ticket cards that have sat unclaimed too long."
	case verbAssign:
		return "Claim a ticket: assign it in Jira, move it to In Progress, and collapse its card."
	case verbDismiss:
		return "Wave a ticket off: collapse its card without touching Jira."
	default:
		return "Advertise Jira tickets matching a JQL query as claimable Slack cards."
	}
}

// PrimaryArg binds the verb flag's value.
func (t *Tool) PrimaryArg() string {
	switch t.verb {
	case verbAssign, verbDismiss:
		return "ticket"
	case verbPoll:
		return "query"
	default:
		return ""
	}
}

// InputSchema declares the parameters for this verb.
func (t *Tool) InputSchema() *jsonschema.Schema {
	props := map[string]*jsonschema.Schema{
		"slack_profile": {
			Type:        "string",
			Description: "Which configured Slack account to post through. Defaults to \"default\".",
		},
		"slack_channel": {
			Type:        "string",
			Description: "Target channel id. Omitted, cards are DMed to the configured admin.",
		},
	}
	var required []string

	switch t.verb {
	case verbPoll:
		props["query"] = &jsonschema.Schema{
			Type:        "string",
			Description: "JQL selecting the tickets to advertise.",
		}
		props["dry_run"] = &jsonschema.Schema{
			Type:        "boolean",
			Description: "Report what would change without posting or writing.",
		}
		required = []string{"query"}
	case verbNudge:
		props["after_hours"] = &jsonschema.Schema{
			Type:        "number",
			Description: "Idle threshold before a card is nudged. Defaults to 24.",
		}
		props["dry_run"] = &jsonschema.Schema{
			Type:        "boolean",
			Description: "Report which cards would be nudged without posting.",
		}
	case verbAssign, verbDismiss:
		props["ticket"] = &jsonschema.Schema{
			Type:        "string",
			Description: "Ticket key, or the actions block_id (jira_qct_<KEY>) a click carries.",
		}
		props["user"] = &jsonschema.Schema{
			Type:        "string",
			Description: "Slack user id of whoever clicked. Must be the configured admin.",
		}
		required = []string{"ticket", "user"}
	}

	return &jsonschema.Schema{Type: "object", Properties: props, Required: required}
}

// Invoke dispatches to the engine.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	profile, _ := args["slack_profile"].(string)
	channel, _ := args["slack_channel"].(string)
	target, err := t.resolver.Resolve(profile, channel)
	if err != nil {
		return nil, err
	}

	// Validate before opening anything, so a bad argument costs nothing.
	var issueKey, actor, jql string
	switch t.verb {
	case verbPoll:
		if jql, _ = args["query"].(string); jql == "" {
			return nil, fmt.Errorf("query is required: the JQL selecting tickets to advertise")
		}
	case verbAssign, verbDismiss:
		raw, _ := args["ticket"].(string)
		// A click carries the actions block_id, not the bare key.
		if issueKey = ticket.TicketKeyFromBlockID(raw); issueKey == "" {
			return nil, fmt.Errorf("ticket is required, as a key or a jira_qct_<KEY> block id")
		}
		if actor, _ = args["user"].(string); actor == "" {
			return nil, fmt.Errorf("user is required: an unattributed click is not actionable")
		}
	}

	engine, closer, err := t.newer(ctx)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}

	dryRun, _ := args["dry_run"].(bool)
	switch t.verb {
	case verbPoll:
		return engine.Poll(ctx, jql, target, dryRun)
	case verbNudge:
		return engine.Nudge(ctx, nudgeAfter(args), minGap, target, dryRun)
	case verbAssign:
		return engine.Assign(ctx, issueKey, actor, target)
	default:
		return engine.Dismiss(ctx, issueKey, actor, target)
	}
}

// nudgeAfter reads the idle threshold, defaulting to the Python's 24 hours.
func nudgeAfter(args map[string]any) time.Duration {
	hours := float64(DefaultNudgeAfterHours)
	switch v := args["after_hours"].(type) {
	case float64:
		if v > 0 {
			hours = v
		}
	case int64:
		if v > 0 {
			hours = float64(v)
		}
	}
	return time.Duration(hours * float64(time.Hour))
}
