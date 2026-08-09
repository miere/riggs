// Package ticket mirrors a Jira queue into Slack as claimable cards.
//
// It replaces the Python `quick_coding_tasks` automation. The three operations
// it had — poll, nudge, act — are one mechanism here: a card in the ledger,
// upserted when its state changes and threaded when something needs saying.
package ticket

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/jira"
)

// KeyPrefix namespaces this domain's cards in the ledger.
const KeyPrefix = "jira.ticket:"

// nudgeLatch rate-limits the idle ping.
const nudgeLatch = "nudge"

// ReadyStatus is the status a ticket must be in to be up for grabs. A ticket
// that leaves it has been handled, whoever did it.
const ReadyStatus = "Ready"

// iconURL is the Jira glyph, carried over so migrated cards keep their look.
const iconURL = "https://avatars.slack-edge.com/2026-06-08/11307219713157_169ea7535e4444a83b8d_36.png"

// State is a card's condition.
type State string

const (
	// Pending means the ticket is advertised and unclaimed.
	Pending State = "pending"
	// Assigned means someone took it.
	Assigned State = "assigned"
	// Dismissed means it was waved off from Slack.
	Dismissed State = "dismissed"
	// Resolved means it was handled outside Slack — assigned in Jira, or
	// moved out of Ready.
	Resolved State = "resolved"
)

// Live reports whether the card still shows buttons.
func (s State) Live() bool { return s == Pending }

// Key is the ledger key for a ticket's card.
func Key(issueKey string) string { return KeyPrefix + issueKey }

// ActionsBlockID is the block_id carrying the ticket key back from a click.
// The Python's spelling is preserved so the existing workflow rules match.
func ActionsBlockID(issueKey string) string { return "jira_qct_" + issueKey }

// TicketKeyFromBlockID recovers the ticket key from an actions block_id,
// tolerating a caller that passes the bare key instead.
func TicketKeyFromBlockID(value string) string {
	return strings.TrimPrefix(value, "jira_qct_")
}

// Card renders a ticket.
//
// A live card carries the summary and the buttons; a resolved one collapses to
// a single status line with no description at all — which is how the Python
// renders it, and the reason blockkit treats an empty body as "no section".
func Card(issue jira.Issue, summary string, state State) blockkit.Card {
	card := blockkit.Card{
		Title:    issue.Summary,
		Subtitle: issue.Key,
		IconURL:  iconURL,
		IconAlt:  "Jira",
	}
	if state.Live() {
		card.Body = summary
		card.BodyBlockID = "ticket_description"
		card.ActionsBlockID = ActionsBlockID(issue.Key)
		card.Actions = []blockkit.Element{
			blockkit.Button{ActionID: "qct_assign", Text: "Assign", Value: issue.Key, Primary: true},
			blockkit.Button{ActionID: "qct_dismiss", Text: "Dismiss", Value: issue.Key},
		}
		return card
	}
	card.Collapsed = true
	card.Context = statusLine(issue, state)
	return card
}

// statusLine is the single context line on a collapsed card.
func statusLine(issue jira.Issue, state State) string {
	var parts []string
	switch {
	case state == Assigned && issue.Assignee != "":
		parts = append(parts, "Assigned to: "+issue.Assignee)
	case state == Resolved && issue.Assignee != "":
		parts = append(parts, "Assigned to: "+issue.Assignee)
	case state == Dismissed:
		parts = append(parts, "Dismissed")
	case state == Resolved:
		parts = append(parts, "Closed")
	default:
		parts = append(parts, string(state))
	}
	if updated := jira.FormatUpdated(issue.Updated); updated != "" {
		parts = append(parts, "Last updated: "+updated)
	}
	return strings.Join(parts, " - ")
}

// AvailableText is the notification text for a freshly advertised ticket.
func AvailableText(browseURL string) string {
	return "A quick task is available for implementation: " + browseURL
}

// UnavailableText is the notification text once a ticket is claimed.
func UnavailableText(browseURL string) string {
	return "Ticket " + browseURL + " is not available for implementation anymore"
}

// NudgeText is the threaded idle ping.
func NudgeText(slackUserID, issueKey string, idle time.Duration) string {
	span := "a while"
	if days := int(idle.Hours() / 24); days >= 1 {
		span = fmt.Sprintf("%d day%s", days, plural(days))
	}
	return fmt.Sprintf("<@%s> :hourglass_flowing_sand: *%s* has been sitting in *%s* "+
		"unclaimed for %s - still up for grabs if you (or anyone) can pick it up.",
		slackUserID, issueKey, ReadyStatus, span)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// AssignedText and DismissedText are the threaded confirmations.
func AssignedText(slackUserID string) string {
	return fmt.Sprintf("<@%s> The task is ready for you!", slackUserID)
}

func DismissedText(slackUserID string) string {
	return fmt.Sprintf("<@%s> dismissed the task.", slackUserID)
}

// Source is the Jira seam.
type Source interface {
	Search(ctx context.Context, jql string, max int) ([]jira.Issue, error)
	Get(ctx context.Context, key string) (jira.Issue, error)
	FindUser(ctx context.Context, email string) (jira.User, error)
	Assign(ctx context.Context, key, accountID string) error
	Transition(ctx context.Context, key, name string) error
	BrowseURL(key string) string
}
