package pullrequest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
)

// What happens to the digest when an approval lands.
//
// The reconcile pass would get there on its own: an approved pull request stops
// being reviewable and the next tick strikes it through. But "on its own" is up
// to three minutes away, and a button that visibly does nothing for three
// minutes reads as a button that did not work. So the row is struck through
// immediately, and a failure is said out loud in the digest's own thread rather
// than left to a log.

// Completer marks a digest row done, or reports why it is not.
//
// It works from the LEDGER alone — no GitHub, no summariser. Everything a row
// renders is recorded (§9b), which is what makes acting on one item safe: the
// others in the same message re-render exactly as they were.
type Completer struct {
	store    *notify.Store
	notifier *notify.Notifier
	poster   slack.Poster
	// gh re-reads the pull request when an ask card has to be redrawn. Only
	// then: the digest renders from the ledger alone, and this stays nil in the
	// tests that exercise it.
	gh  Detailer
	now func() time.Time
}

// NewCompleter builds the completer.
func NewCompleter(store *notify.Store, n *notify.Notifier, poster slack.Poster) *Completer {
	return &Completer{store: store, notifier: n, poster: poster, now: time.Now}
}

// WithDetailer supplies the read used to redraw an ask card.
func (c *Completer) WithDetailer(gh Detailer) *Completer {
	c.gh = gh
	return c
}

// WithClock overrides the clock; intended for tests.
func (c *Completer) WithClock(now func() time.Time) *Completer {
	c.now = now
	return c
}

// Complete marks ref done and rewrites the digest it lives in.
//
// A reference the digest is not tracking is a no-op, not an error: the approval
// still happened, and an ask-review card can perfectly well be approved for a
// pull request that never made it into a digest.
//
// status is the label the row carries afterwards, e.g. ReasonApproved.
func (c *Completer) Complete(ctx context.Context, ref, status string, target slack.Target) (bool, error) {
	key := BulkItemPrefix + ref
	item, found, err := c.store.Item(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if item.Done {
		return false, nil // already struck through; nothing to rewrite
	}

	item.Done = true
	item.Status = status
	item.UpdatedAt = c.now()
	if err := c.store.SaveItem(ctx, key, item); err != nil {
		return false, err
	}
	if err := c.rebuild(ctx, item.PostKey, target); err != nil {
		return true, err
	}
	return true, nil
}

// Settle collapses the review-request card for ref, if one was ever posted.
//
// The Approve button goes and the container closes. A card still offering
// Approve for a pull request that is already merged is worse than no card: it
// invites a click that can only fail.
//
// Independent of Complete, because the two are independent facts. A pull
// request can be in a digest, or have an ask card, or both, or neither — the
// same approval settles whichever exist.
func (c *Completer) Settle(ctx context.Context, ref, label string, target slack.Target) (bool, error) {
	key := AskKey(ref)
	entry, found, err := c.notifier.Card(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if entry.State == AskStateDone {
		return false, nil // already collapsed
	}
	if c.gh == nil {
		return false, fmt.Errorf("cannot redraw the review request for %s: nothing is wired to read it", ref)
	}

	repo, number, err := SplitRef(ref)
	if err != nil {
		return false, err
	}
	detail, err := c.gh.PullRequestDetail(ctx, repo, number)
	if err != nil {
		return false, fmt.Errorf("re-reading %s to collapse its review request: %w", ref, err)
	}

	dest := target
	dest.Channel = entry.Channel
	card := AskSettledCard(detail, Body(detail), label)
	if _, err := c.notifier.Upsert(ctx, key, dest, card,
		AskFallbackText(detail), AskStateDone); err != nil {
		return false, err
	}
	return true, nil
}

// Fail reports a failure in the thread of the digest ref belongs to.
//
// The digest's thread rather than the clicked message's: the digest is where
// the row still sits waiting, and it is where somebody looking for the outcome
// will look. An untracked reference has no such thread, which is reported so
// the caller can say it somewhere else instead.
func (c *Completer) Fail(ctx context.Context, ref, message string, target slack.Target) (bool, error) {
	item, found, err := c.store.Item(ctx, BulkItemPrefix+ref)
	if err != nil || !found {
		return false, err
	}
	entry, found, err := c.notifier.Card(ctx, item.PostKey)
	if err != nil || !found || entry.TS == "" {
		return false, err
	}

	dest := target
	dest.Channel = entry.Channel
	if _, err := c.poster.Post(ctx, dest, slack.Message{
		Text:     message,
		Blocks:   blockkit.ContextBlocks(message),
		ThreadTS: entry.TS,
	}); err != nil {
		return false, fmt.Errorf("reporting the failure for %s: %w", ref, err)
	}
	return true, nil
}

// rebuild rewrites one digest from the items the ledger says are in it.
//
// Every row is rendered from stored data, so striking one through cannot
// disturb the others — which is exactly what would have happened before the row
// data was recorded, when an unrefreshed row collapsed to its bare reference.
func (c *Completer) rebuild(ctx context.Context, postKey string, target slack.Target) error {
	all, err := c.store.ItemsInStream(ctx, BulkStream)
	if err != nil {
		return err
	}

	var members []notify.KeyedItem
	for _, it := range all {
		if it.PostKey == postKey {
			members = append(members, it)
		}
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].Position < members[j].Position })

	if len(members) == 0 {
		// Nothing left to show. An empty digest is deleted, not blanked (§7c).
		return c.notifier.DeleteDigest(ctx, postKey, target)
	}

	rows := make([]blockkit.Row, 0, len(members))
	for _, it := range members {
		ref := strings.TrimPrefix(it.Key, BulkItemPrefix)
		rows = append(rows, itemCandidate(ref, it.Item).row())
	}

	digest := blockkit.Digest{
		Title: bulkTitle, Subtitle: bulkSubtitle,
		IconURL: bulkIconURL, IconAlt: "GitHub", Rows: rows,
	}
	_, err = c.notifier.UpdateDigest(ctx, postKey, target, digest, bulkFallback(rows))
	return err
}
