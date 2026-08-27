package app

import (
	"context"
	"io"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/ticket"
	"github.com/miere/riggs-mcp/internal/tools"
	"github.com/miere/riggs-mcp/internal/tools/bulktickets"
	"github.com/miere/riggs-mcp/internal/tools/tickets"
)

// jiraClient builds the Jira REST client from the effective credentials.
func jiraClient(cfg *config.Config) *jira.Client {
	email, token := cfg.JiraCredentials()
	return jira.New(cfg.JiraBaseURL(), email, token)
}

// engineForTickets assembles the ticket reconciler for one invocation.
func engineForTickets(cfg *config.Config) (*ticket.Engine, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	client := jiraClient(cfg)
	notifier := notify.New(store, slack.NewAPI())
	engine := ticket.NewEngine(client, store, notifier, ticket.Admin{
		SlackUserID: cfg.Admin.SlackUserID,
		JiraEmail:   cfg.Admin.JiraEmail,
	})
	return engine, store, nil
}

// bulkEngineForTickets assembles the ticket digest reconciler for one
// invocation. It reads the same Jira and writes the same ledger as the card
// loop, in its own stream.
func bulkEngineForTickets(cfg *config.Config, jql string, opts ticket.BulkOptions) (*ticket.BulkEngine, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	notifier := notify.New(store, slack.NewAPI())
	return ticket.NewBulkEngine(jiraClient(cfg), store, notifier, jql, opts), store, nil
}

// assistAskerFor assembles the assistance-request poster for one invocation.
//
// Its destination and wording come from `ai-assistance`, never from
// `review-request`: the two actions look identical and answer different
// questions, so they are configured apart (§10).
func assistAskerFor(cfg *config.Config) (*ticket.Asker, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	api := slack.NewAPI()
	asker := ticket.NewAsker(jiraClient(cfg), notify.New(store, api), api,
		cfg.AssistUser(), cfg.AIAssistance.Channel, cfg.AssistPrompt()).WithResolver(api)
	return asker, store, nil
}

// registerJiraTools wires the ticket verbs, when Jira is configured.
//
// Unlike the GitHub tools, these are gated on what Riggs holds itself: with no
// token — or no tenant to point it at — every call would fail identically, so
// the tools are simply absent and `riggs capabilities` says why.
//
// The tenant counts as configuration, not as a default. Registering these
// against a baked-in Atlassian instance would mean a misconfigured machine
// quietly reading and assigning tickets on somebody else's Jira.
func registerJiraTools(reg *tools.Registry, cfg *config.Config, resolver *slack.Resolver) {
	email, token := cfg.JiraCredentials()
	if email == "" || token == "" || cfg.JiraBaseURL() == "" {
		return
	}
	factory := func(context.Context) (tickets.Engine, io.Closer, error) {
		return engineForTickets(cfg)
	}
	reg.Register(tickets.NewPoll(resolver, factory))
	reg.Register(bulktickets.New(resolver,
		func(_ context.Context, jql string, opts ticket.BulkOptions) (bulktickets.Engine, io.Closer, error) {
			return bulkEngineForTickets(cfg, jql, opts)
		}))
	reg.Register(tickets.NewAssign(resolver, factory))
	reg.Register(tickets.NewDismiss(resolver, factory))
	reg.Register(tickets.NewImport(func(context.Context) (tickets.Store, func() error, error) {
		store, err := ledger(cfg)
		if err != nil {
			return nil, nil, err
		}
		return store, store.Close, nil
	}))
}
