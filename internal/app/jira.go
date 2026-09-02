package app

import (
	"context"
	"io"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/ticket"
	"github.com/miere/riggs-mcp/internal/tools/bulktickets"
)

// jiraClient builds the Jira REST client from the effective credentials.
func jiraClient(cfg *config.Config) *jira.Client {
	email, token := cfg.JiraCredentials()
	return jira.New(cfg.JiraBaseURL(), email, token)
}

// bulkEngineForTickets assembles the ticket digest reconciler for one
// invocation. It reads the same Jira and writes the same ledger as the card
// loop, in its own stream.
func bulkEngineForTickets(cfg *config.Config, jql string, opts ticket.BulkOptions) (*ticket.BulkEngine, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	opts.Actions = ticketRowActions(cfg)
	notifier := notify.New(store, slack.NewAPI())
	return ticket.NewBulkEngine(jiraClient(cfg), store, notifier, jql, opts), store, nil
}

// assistAskerFor assembles the SME assistance-request poster for one
// invocation.
//
// Its destination and wording come from `sme-assistance`, never from
// `review-request`: the two actions look identical and answer different
// questions, so they are configured apart (§10).
func assistAskerFor(cfg *config.Config) (*ticket.Asker, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	api := slack.NewAPI()
	asker := ticket.NewAsker(jiraClient(cfg), notify.New(store, api), api,
		cfg.SMEUser(), cfg.SMEAssistance.Channel, cfg.SMEPrompt()).WithResolver(api)
	return asker, store, nil
}

// ticketRowActions reads which optional verbs a ticket row may offer.
//
// Resolved here, in the composition root, rather than inside the digest: the
// domain renders what it is told to and the config is what decides. Both
// default to off, so an installation that answered neither question gets a row
// with a link on it and nothing that cannot work.
func ticketRowActions(cfg *config.Config) ticket.RowActions {
	return ticket.RowActions{
		AskAssist: cfg.SMEEnabled(),
		RunAssist: cfg.AIEnabled(),
	}
}

// ticketDigest builds the ticket digest command, when Jira is configured.
//
// Gated on what Riggs holds itself: with no token — or no tenant to point it at
// — every call would fail identically, so the command is simply absent and
// `riggs capabilities` says why.
//
// The tenant counts as configuration, not as a default. Defaulting it would
// mean a misconfigured machine quietly reading tickets on somebody else's Jira.
func ticketDigest(cfg *config.Config) *bulktickets.Tool {
	email, token := cfg.JiraCredentials()
	if email == "" || token == "" || cfg.JiraBaseURL() == "" {
		return nil
	}
	return bulktickets.New(slack.NewResolver(cfg),
		func(_ context.Context, jql string, opts ticket.BulkOptions) (bulktickets.Engine, io.Closer, error) {
			return bulkEngineForTickets(cfg, jql, opts)
		})
}
