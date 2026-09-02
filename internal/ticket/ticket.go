// Package ticket mirrors a Jira queue into Slack.
//
// It replaced the Python `quick_coding_tasks` automation, whose three
// operations — poll, nudge, act — became one mechanism here: a card in the
// ledger, upserted when its state changes and threaded when something needs
// saying.
//
// None of that survives. The nudge went in Phase 25, the per-ticket card loop
// and the assign/dismiss verbs in Phase 30, and with them the whole Engine.
// What is left is the digest (bulk.go) and the assistance request it can hand
// to a person (askassist.go); this file is the handful of identifiers both
// still share.
package ticket

import (
	"context"
	"strings"

	"github.com/miere/riggs-mcp/internal/jira"
)

// ReadyStatus is the status a ticket must be in to be up for grabs. A ticket
// that has moved out of it has been picked up by somebody, whatever the
// assignee field says.
const ReadyStatus = "Ready"

// iconURL is the Jira glyph, worn by the assistance-request card.
const iconURL = "https://avatars.slack-edge.com/2026-06-08/11307219713157_169ea7535e4444a83b8d_36.png"

// ActionsBlockID namespaces a ticket's actions row, so a click on one carries
// the key back.
func ActionsBlockID(issueKey string) string { return "jira_qct_" + issueKey }

// TicketKeyFromBlockID recovers the ticket key from a block id.
//
// It accepts a bare key as well as a namespaced one: a digest row's block_id is
// the key itself, while a card's actions row carries the prefix.
func TicketKeyFromBlockID(value string) string {
	return strings.TrimPrefix(value, "jira_qct_")
}

// Source is the Jira read surface this package needs. *jira.Client satisfies it.
//
// Three methods, down from six: FindUser, Assign and Transition went with
// Engine.Assign, which was the only caller of any of them. What is left is
// read-only, which is now true of everything this package does to Jira.
type Source interface {
	Search(ctx context.Context, jql string, max int) ([]jira.Issue, error)
	Get(ctx context.Context, key string) (jira.Issue, error)
	BrowseURL(key string) string
}
