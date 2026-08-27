package ticket

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/bulk"
	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
)

// The bulk digest: one message carrying up to N tickets, rather than one
// self-updating card per ticket.
//
// It is the ticket queue's version of what the pull-request queue does, on the
// same rotation engine (internal/bulk) and with none of the same rendering. The
// card loop (engine.go) is untouched and still runs: this is a second,
// independent consumer of the same Jira reads and the same ledger, in its own
// stream, so one can be scheduled without disturbing the other.

const (
	// BulkStream groups this domain's digest items in the ledger.
	BulkStream = "jira.tickets.bulk"
	// BulkPostPrefix namespaces the digest messages themselves.
	BulkPostPrefix = "jira.tickets.bulk:post:"
	// BulkItemPrefix namespaces the per-ticket membership rows. It is distinct
	// from KeyPrefix so the digest and the per-ticket cards can track the same
	// ticket without colliding.
	BulkItemPrefix = "jira.tickets.bulk:item:"

	// BulkActionID names every row's overflow menu.
	BulkActionID = "jira_bulk_overflow"

	// Intents. Bare tokens, identical on every row, so the daemon's router can
	// match them exactly (§7b).
	IntentOpenBrowser = "open_browser"
	IntentAskAssist   = "ask_assist"
)

// DefaultCooldown is how long a ticket must sit before it may move into a new
// digest — and how long a struck-through row lingers before it is purged.
//
// Three hours, the same as the pull-request digest. The specification asked for
// "no more than once per period, in 6h blocks"; this is a rolling window rather
// than calendar blocks, for the reason §9b already documents — a block boundary
// makes the delay between two announcements anything from a minute to a whole
// block depending on where in it the ticket appeared, and nobody reading the
// channel can tell which they got.
//
// It is still a separate constant from the pull-request one, and deliberately
// so: the two queues are tuned independently and happen to agree today.
const DefaultCooldown = 3 * time.Hour

// DefaultMaxItems caps one digest.
const DefaultMaxItems = 10

// MaxItemsEnv overrides that cap.
//
// Its own variable, not the pull-request digest's: the two queues are
// configured independently, and a channel that wants twenty tickets a pass has
// said nothing about how many pull requests it wants.
const MaxItemsEnv = "RIGGS_JIRA_BULK_MAX_ITEMS"

// searchLimit caps the discovery query. One call per pass, never fanned out.
const searchLimit = 100

// bulkTitle and bulkSubtitle head every digest.
const (
	bulkTitle = "Jira - Actionable"
	// The mock-up carried the pull-request subtitle verbatim ("juicy code
	// reviews"), which is a copy-paste rather than a decision: these are tickets
	// nobody has picked up, and none of them is a code review.
	bulkSubtitle = "These tickets are ready and nobody has picked them up."
	// bulkIconURL is the digest's own glyph, deliberately not the legacy card's
	// const: the two shapes have separate lifecycles (§7c), and sharing it would
	// mean a change to one silently re-renders every card of the other.
	bulkIconURL = "https://avatars.slack-edge.com/2026-06-08/11307219713157_169ea7535e4444a83b8d_36.png"
)

// Statuses a row carries. The ledger records them and never reads them.
const (
	// StatusReady is a ticket still up for grabs.
	StatusReady = "ready"
	// StatusTaken is one that has been assigned or moved out of Ready — by
	// anybody, anywhere.
	StatusTaken = "taken"
)

// BulkOptions configures one digest family.
type BulkOptions struct {
	// MaxItems caps a single digest. Zero takes the environment, then the
	// default.
	MaxItems int
	// Cooldown is the rolling window. Zero takes the default.
	Cooldown time.Duration
}

// resolved fills in the blanks from the environment and the defaults.
func (o BulkOptions) resolved() BulkOptions {
	if o.MaxItems <= 0 {
		o.MaxItems = bulk.MaxItemsFromEnv(MaxItemsEnv, DefaultMaxItems)
	}
	if o.Cooldown <= 0 {
		o.Cooldown = DefaultCooldown
	}
	return o
}

// BulkReport is the result of one digest pass.
type BulkReport = bulk.Report

// BulkEngine reconciles the ticket digest.
type BulkEngine struct {
	*bulk.Engine
}

// NewBulkEngine builds the digest reconciler over a Jira source and the shared
// ledger.
//
// jql is the query that decides what is up for grabs — a parameter, not a
// constant, so one tool serves any queue rather than only the `ai-able` board
// (§8b).
func NewBulkEngine(src Source, store *notify.Store, n *notify.Notifier,
	jql string, opts BulkOptions) *BulkEngine {

	opts = opts.resolved()
	return &BulkEngine{Engine: bulk.New(
		bulkDomain{jira: src, jql: jql}, store, n,
		bulk.Options{MaxItems: opts.MaxItems, Cooldown: opts.Cooldown},
	)}
}

// WithClock overrides the clock; intended for tests.
func (b *BulkEngine) WithClock(now func() time.Time) *BulkEngine {
	b.Engine.WithClock(now)
	return b
}

// bulkDomain is the ticket half of a digest: what the items are, and how a row
// draws.
type bulkDomain struct {
	jira Source
	jql  string
}

// The ledger identity of this digest family.
func (bulkDomain) Stream() string     { return BulkStream }
func (bulkDomain) PostPrefix() string { return BulkPostPrefix }
func (bulkDomain) ItemPrefix() string { return BulkItemPrefix }
func (bulkDomain) Noun() string       { return "ticket" }

// Header heads every digest this domain posts.
func (bulkDomain) Header() bulk.Header {
	return bulk.Header{
		Title: bulkTitle, Subtitle: bulkSubtitle,
		IconURL: bulkIconURL, IconAlt: "Jira",
	}
}

// Fallback is the notification text: what Slack shows in the sidebar, where
// blocks are not rendered at all.
func (bulkDomain) Fallback(rows []blockkit.Row) string { return bulkFallback(rows) }

// Row renders one ticket as a digest row.
//
// A done row keeps only the link. There is nothing left to ask about a ticket
// somebody has already taken, and an option that can only fail is worse than no
// option.
//
// "Assign to Me" is deliberately absent. It is specified but explicitly not for
// this pass, and the pull-request digest set the precedent with Approve: a
// control that silently does nothing is worse than one that is not there. The
// verb behind it (jira.tickets.assign) already exists, so adding the option is
// two lines and one route the day it is wanted.
func (bulkDomain) Row(c bulk.Candidate) blockkit.Row {
	row := blockkit.Row{
		BlockID:  c.ID,
		Title:    c.Title,
		Meta:     rowMeta(c.ID, c.Author),
		Done:     c.Done,
		ActionID: BulkActionID,
		Options: []blockkit.MenuOption{
			{Text: blockkit.MarkerOpen + "  Open on Browser", Value: IntentOpenBrowser, URL: c.URL},
		},
	}
	if c.Done {
		return row
	}
	row.Options = append(row.Options,
		blockkit.MenuOption{Text: blockkit.MarkerAsk + "  Ask for AI Assistance", Value: IntentAskAssist})
	return row
}

// rowMeta is the row's second line: the key, and who raised it.
func rowMeta(key, reporter string) string {
	if reporter == "" {
		return "_" + key + "_"
	}
	return fmt.Sprintf("_%s_ - reported by `%s`", key, reporter)
}

// Candidates resolves every ticket this pass is responsible for:
//
//	(matches the query) ∪ (already in the digest)
//
// The query is the source of truth for "still up for grabs" (§8b). A tracked
// ticket that has fallen out of it has been handled by somebody, so its row is
// struck through rather than quietly dropped — the reader saw it advertised and
// is owed the outcome.
func (d bulkDomain) Candidates(ctx context.Context, tracked map[string]notify.KeyedItem) ([]bulk.Candidate, error) {
	issues, err := d.jira.Search(ctx, d.jql, searchLimit)
	if err != nil {
		return nil, fmt.Errorf("searching for tickets: %w", err)
	}

	out := make([]bulk.Candidate, 0, len(issues)+len(tracked))
	matched := map[string]bool{}
	for _, issue := range issues {
		matched[issue.Key] = true
		out = append(out, d.candidate(issue, issue.Claimed(ReadyStatus)))
	}

	// Tracked but no longer matching. Each is re-read to find out what became of
	// it, because the row says so: "reported by X", struck through.
	keys := make([]string, 0, len(tracked))
	for key := range tracked {
		if !matched[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		issue, err := d.jira.Get(ctx, key)
		if err != nil {
			// A ticket that cannot be READ is left exactly as it is. Striking it
			// through on a transient failure would claim it was handled when it
			// may not have been — the same rule the card loop follows (§8b).
			// Omitting it here leaves the ledger row untouched and the rotation
			// engine renders it from what was last stored.
			continue
		}
		out = append(out, d.candidate(issue, true))
	}
	return out, nil
}

// candidate maps one issue onto a digest row's worth of data.
func (d bulkDomain) candidate(issue jira.Issue, done bool) bulk.Candidate {
	status := StatusReady
	if done {
		status = StatusTaken
	}
	return bulk.Candidate{
		ID:        issue.Key,
		Title:     issue.Summary,
		Author:    issue.Reporter,
		URL:       d.jira.BrowseURL(issue.Key),
		CreatedAt: issue.Created,
		Status:    status,
		Done:      done,
	}
}

// bulkFallback is the notification text: what Slack shows in the sidebar, where
// blocks are not rendered at all.
func bulkFallback(rows []blockkit.Row) string {
	open := 0
	for _, r := range rows {
		if !r.Done {
			open++
		}
	}
	if open == 1 {
		return "1 ticket is ready to be picked up."
	}
	return fmt.Sprintf("%d tickets are ready to be picked up.", open)
}

// Guard: the domain must satisfy both halves of the contract.
var _ bulk.Domain = bulkDomain{}
