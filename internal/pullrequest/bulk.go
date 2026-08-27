package pullrequest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/bulk"
	"github.com/miere/riggs-mcp/internal/notify"
)

// The bulk digest: one message carrying up to N pull requests, rather than one
// self-updating card per pull request.
//
// The rotation itself — cooldown, FIFO, cap-and-hold, purge, rebuild — lives in
// internal/bulk, because the ticket queue needs precisely the same five rules
// and none of the same rendering. What stays here is what is actually about
// pull requests: which ones a pass is responsible for, and what a row says.
//
// The card loop (reconcile.go) is untouched and still runs. This is a second,
// independent consumer of the same GitHub reads and the same ledger.

const (
	// BulkStream groups this domain's digest items in the ledger.
	BulkStream = "git.pr.bulk"
	// BulkPostPrefix namespaces the digest messages themselves.
	BulkPostPrefix = "git.pr.bulk:post:"
	// BulkItemPrefix namespaces the per-pull-request membership rows. It is
	// distinct from KeyPrefix so the digest and the per-PR cards can track the
	// same pull request without colliding.
	BulkItemPrefix = "git.pr.bulk:item:"

	// BulkActionID names every row's overflow menu.
	BulkActionID = "pr_bulk_overflow"

	// Intents. Bare tokens, identical on every row, so the daemon's router can
	// match them exactly (§7b).
	IntentOpenBrowser  = "open_browser"
	IntentAskReview    = "ask_review"
	IntentApproveMerge = "approve_merge"
)

// DefaultCooldown is how long an item must sit before it may move into a new
// digest — and how long a struck-through row lingers before it is purged.
const DefaultCooldown = 3 * time.Hour

// DefaultMaxItems caps one digest.
const DefaultMaxItems = 10

// MaxItemsEnv overrides that cap.
//
// It is the pull-request digest's own variable. The ticket digest has a
// separate one: the two queues are configured independently, so raising one
// cap must not silently raise the other.
const MaxItemsEnv = "RIGGS_BULK_MAX_ITEMS"

// bulkTitle and bulkSubtitle head every digest.
const (
	bulkTitle    = "GitHub - Pull Requests"
	bulkSubtitle = "You have been assigned some juicy code reviews."
	// bulkIconURL is the digest's own glyph, deliberately not the legacy card's
	// const: the two shapes have separate lifecycles (§7c), and sharing it would
	// mean a change to one silently re-renders every card of the other.
	bulkIconURL = "https://avatars.slack-edge.com/2020-11-25/1527503386626_319578f21381f9641cd8_36.png"
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

// BulkEngine reconciles the digest.
type BulkEngine struct {
	*bulk.Engine
}

// NewBulkEngine builds the digest reconciler over the card engine's GitHub
// reads and the shared ledger.
func NewBulkEngine(e *Engine, store *notify.Store, n *notify.Notifier, opts BulkOptions) *BulkEngine {
	opts = opts.resolved()
	return &BulkEngine{Engine: bulk.New(
		bulkDomain{engine: e}, store, n,
		bulk.Options{MaxItems: opts.MaxItems, Cooldown: opts.Cooldown},
	)}
}

// WithClock overrides the clock; intended for tests.
func (b *BulkEngine) WithClock(now func() time.Time) *BulkEngine {
	b.Engine.WithClock(now)
	return b
}

// bulkDomain is the pull-request half of a digest: what the items are, and how
// a row draws.
type bulkDomain struct{ engine *Engine }

// The ledger identity of this digest family.
func (bulkDomain) Stream() string     { return BulkStream }
func (bulkDomain) PostPrefix() string { return BulkPostPrefix }
func (bulkDomain) ItemPrefix() string { return BulkItemPrefix }
func (bulkDomain) Noun() string       { return "pull request" }

// Header heads every digest this domain posts.
func (bulkDomain) Header() bulk.Header {
	return bulk.Header{
		Title: bulkTitle, Subtitle: bulkSubtitle,
		IconURL: bulkIconURL, IconAlt: "GitHub",
	}
}

// Fallback is the notification text: what Slack shows in the sidebar, where
// blocks are not rendered at all.
func (bulkDomain) Fallback(rows []blockkit.Row) string { return bulkFallback(rows) }

// Row renders one pull request as a digest row.
//
// A done row keeps only the link: there is nothing left to approve or ask about
// on a pull request that has been reviewed, merged or closed.
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
		blockkit.MenuOption{Text: blockkit.MarkerAsk + "  Ask for Code Review", Value: IntentAskReview})
	// Approve is deliberately absent: it is specified but not implemented, and
	// a button that silently does nothing is worse than one that is not there.
	//
	// Approve and Merge is offered only on a Dependabot pull request, and is
	// derived from the author rather than stored: the ledger records what the
	// row says, not what it is allowed to offer.
	if IsDependabot(c.Author) {
		row.Options = append(row.Options,
			blockkit.MenuOption{Text: blockkit.MarkerDone + "  Approve and Merge", Value: IntentApproveMerge})
	}
	return row
}

// rowMeta is the row's second line: the reference, and who wrote it.
func rowMeta(ref, author string) string {
	if author == "" {
		return "_" + ref + "_"
	}
	return fmt.Sprintf("_%s_ by `@%s`", ref, author)
}

// Candidates resolves every pull request this pass is responsible for:
//
//	(we are a direct requested reviewer) ∪ (already in the digest)
func (d bulkDomain) Candidates(ctx context.Context, tracked map[string]notify.KeyedItem) ([]bulk.Candidate, error) {
	e := d.engine
	seen := map[string]bool{}

	prs, err := e.gh.ReviewRequested(ctx, e.login, searchLimit)
	if err != nil {
		return nil, fmt.Errorf("discovering review requests: %w", err)
	}
	for _, p := range prs {
		if p.Repo != "" {
			seen[p.Ref()] = true
		}
	}
	for ref := range tracked {
		seen[ref] = true
	}

	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)

	out := make([]bulk.Candidate, 0, len(refs))
	for _, ref := range refs {
		repo, number, err := SplitRef(ref)
		if err != nil {
			continue
		}
		detail, err := e.gh.PullRequestDetail(ctx, repo, number)
		if err != nil {
			// A read that failed this pass must not look like a pull request
			// that disappeared, so it is simply skipped and retried next time.
			continue
		}
		if detail.Draft {
			continue
		}
		_, isTracked := tracked[ref]
		if !detail.Requested(e.login) && !isTracked {
			// A team-only request nobody adopted.
			continue
		}
		r, err := e.resolveFrom(ctx, detail, isTracked)
		if err != nil {
			continue
		}

		c := bulk.Candidate{
			ID: ref, Title: detail.Title, Author: detail.Author, URL: detail.URL,
			Status: r.State.Token(), Done: !r.State.Reviewable,
		}
		if detail.CreatedAt != nil {
			c.CreatedAt = *detail.CreatedAt
		}
		out = append(out, c)
	}
	return out, nil
}

// IsDependabot reports whether a login is Dependabot's. GitHub reports the app
// as "dependabot[bot]" on most endpoints and plain "dependabot" on a few, so
// the prefix is matched rather than the whole string.
func IsDependabot(login string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(login)), "dependabot")
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
		return "You have 1 pull request waiting for review."
	}
	return fmt.Sprintf("You have %d pull requests waiting for review.", open)
}

// bulkRenderer is the draw-only half, for callers with no upstream client —
// the completer, which rebuilds a message from the ledger after a click.
func bulkRenderer() bulk.Renderer { return bulkDomain{} }

// Guard: the domain must satisfy both halves of the contract.
var _ bulk.Domain = bulkDomain{}
