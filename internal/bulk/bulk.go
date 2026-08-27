// Package bulk is the rotation engine behind every bulk digest: one message
// carrying up to N items, rebuilt as its membership moves underneath it.
//
// It began inside internal/pullrequest, as the only digest there was. A second
// one — the Jira ticket queue — wanted the same five rules (cooldown, FIFO,
// cap-and-hold, purge, rebuild) and none of the same rendering, so the rules
// moved here and the rendering stayed where it was.
//
// That is the opposite call to the one blockkit made about its two card shapes
// (§7c), and for the opposite reason. There, two things that LOOKED alike had
// diverging lifecycles, so they were kept apart. Here, two things that look
// nothing alike — a pull request and a Jira ticket — have provably the same
// lifecycle: announce it, hold it for a cooldown, rotate it, strike it through,
// purge it. Duplicating that would mean two copies of the one piece of logic in
// Riggs where an off-by-one silently loses somebody's row.
//
// A domain supplies two things: what its items ARE (Source) and how they DRAW
// (Renderer). Everything between those is here.
package bulk

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
)

// DefaultMaxItems caps one digest when a domain names no other.
const DefaultMaxItems = 10

// DefaultCooldown is the fallback rolling window.
const DefaultCooldown = 3 * time.Hour

// Candidate is one item as a digest needs it: enough to draw a row, sort the
// queue and decide whether the row is still live.
type Candidate struct {
	// ID is the item's identity — a pull request reference, a ticket key. It is
	// the ledger key's suffix and the row's block_id, which is the only place a
	// per-row reference can travel back from a click.
	ID string
	// Title is the headline.
	Title string
	// Author is the second-line attribution: who wrote it, who asked for it.
	Author string
	// URL is where "Open on Browser" goes.
	URL string
	// CreatedAt orders the queue. FIFO is by the ITEM's age, not by when Riggs
	// noticed it, so a ticket that has been waiting a week leads one raised an
	// hour ago however long each has been tracked.
	CreatedAt time.Time
	// Status is the opaque domain label the ledger records and never reads.
	Status string
	// Done marks a row that is struck through and no longer rotates.
	Done bool
}

// Header heads a digest message.
type Header struct {
	Title    string
	Subtitle string
	IconURL  string
	IconAlt  string
}

// Renderer is what a domain supplies to DRAW its digest.
//
// It is separate from Source because half the callers have no upstream at all:
// completing a row after a click rebuilds the message from the ledger alone,
// and must not need a GitHub or Jira client to do it.
type Renderer interface {
	// Stream groups this domain's items in the ledger.
	Stream() string
	// PostPrefix namespaces its digest messages; ItemPrefix its membership rows.
	PostPrefix() string
	ItemPrefix() string
	// Header heads every message this domain posts.
	Header() Header
	// Noun names one item, for the report line a human reads.
	Noun() string
	// Row draws one item. The domain decides which options a live row carries
	// and which a done one keeps.
	Row(Candidate) blockkit.Row
	// Fallback is the notification text — what Slack shows in the sidebar,
	// where blocks are not rendered at all.
	Fallback([]blockkit.Row) string
}

// Source discovers the items one pass is responsible for.
//
// tracked is what the ledger already holds, keyed by ID, because the answer is
// always (upstream ∪ tracked): an item that has left the query still has a row
// to strike through.
type Source interface {
	Candidates(ctx context.Context, tracked map[string]notify.KeyedItem) ([]Candidate, error)
}

// Domain is both halves.
type Domain interface {
	Renderer
	Source
}

// Options are the two tunables. Zero means "apply the default", so a domain can
// pass a partially-filled struct straight through from its tool.
type Options struct {
	MaxItems int
	Cooldown time.Duration
}

// MaxItemsFromEnv reads a cap from the named variable, falling back to def.
//
// A value that is not a positive integer is ignored rather than fatal: a typo
// in a job's environment should not stop the queue being delivered. The
// variable's NAME is the domain's, not this package's — the pull-request and
// ticket digests are configured independently, so one being raised must not
// silently raise the other.
func MaxItemsFromEnv(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

// Report is the result of one pass.
type Report struct {
	Considered int      `json:"considered"`
	Posted     []string `json:"posted,omitempty"`
	Held       []string `json:"held,omitempty"`
	Purged     []string `json:"purged,omitempty"`
	Updated    []string `json:"updated_posts,omitempty"`
	Deleted    []string `json:"deleted_posts,omitempty"`
	DryRun     bool     `json:"dry_run"`

	// Noun names what was counted, so the human line reads "considered 4
	// ticket(s)" rather than something generic. It is not part of the JSON
	// shape: a caller parsing the report knows which tool it invoked.
	Noun string `json:"-"`
}

// String renders the report for a human. It is a Stringer with no argument
// because that is what the CLI's renderer dispatches on — a String(noun) would
// silently fall through to %v.
func (r Report) String() string {
	noun := r.Noun
	if noun == "" {
		noun = "item"
	}
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[dry run] ")
	}
	fmt.Fprintf(&b, "considered %d %s(s)\n", r.Considered, noun)
	line := func(label string, ids []string) {
		if len(ids) > 0 {
			fmt.Fprintf(&b, "  %-9s %s\n", label, strings.Join(ids, ", "))
		}
	}
	line("posted", r.Posted)
	line("held", r.Held)
	line("purged", r.Purged)
	line("updated", r.Updated)
	line("deleted", r.Deleted)
	return strings.TrimRight(b.String(), "\n")
}

// sortAll makes the report deterministic, so a test (and a human diffing two
// runs) sees a stable order.
func (r *Report) sortAll() {
	for _, s := range [][]string{r.Posted, r.Held, r.Purged, r.Updated, r.Deleted} {
		sort.Strings(s)
	}
}

// Rebuilder redraws a domain's existing digests from the ledger.
//
// It is the half of Engine that needs no upstream read, split out because the
// click path uses exactly that: an approval strikes its row through and rewrites
// the message it sits in, with no GitHub client involved.
type Rebuilder struct {
	renderer Renderer
	store    *notify.Store
	notifier *notify.Notifier
	now      func() time.Time
}

// NewRebuilder builds the redraw half.
func NewRebuilder(r Renderer, store *notify.Store, n *notify.Notifier) *Rebuilder {
	return &Rebuilder{renderer: r, store: store, notifier: n, now: time.Now}
}

// WithClock overrides the clock; intended for tests.
func (b *Rebuilder) WithClock(now func() time.Time) *Rebuilder {
	b.now = now
	return b
}

// Digest assembles a message from rows.
func (b *Rebuilder) digest(rows []blockkit.Row) blockkit.Digest {
	h := b.renderer.Header()
	return blockkit.Digest{
		Title: h.Title, Subtitle: h.Subtitle,
		IconURL: h.IconURL, IconAlt: h.IconAlt, Rows: rows,
	}
}

// ID recovers an item's identity from its ledger key.
func (b *Rebuilder) ID(key string) string { return strings.TrimPrefix(key, b.renderer.ItemPrefix()) }

// Key is the ledger key for an item.
func (b *Rebuilder) Key(id string) string { return b.renderer.ItemPrefix() + id }

// Rebuild rewrites one digest from the items the ledger says are in it.
//
// Every row renders from stored data, so striking one through cannot disturb the
// others — which is exactly what used to happen before the row data was
// recorded, when an unrefreshed row collapsed to its bare reference with a dead
// link.
//
// An emptied post is deleted, not blanked: a header with nothing under it reads
// as "nothing needs you" while occupying the space of when something did.
func (b *Rebuilder) Rebuild(ctx context.Context, postKey string, target slack.Target) error {
	all, err := b.store.ItemsInStream(ctx, b.renderer.Stream())
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
		return b.notifier.DeleteDigest(ctx, postKey, target)
	}

	rows := make([]blockkit.Row, 0, len(members))
	for _, it := range members {
		rows = append(rows, b.renderer.Row(FromItem(b.ID(it.Key), it.Item)))
	}
	_, err = b.notifier.UpdateDigest(ctx, postKey, target, b.digest(rows), b.renderer.Fallback(rows))
	return err
}

// Engine reconciles a domain's digests: it decides which items belong in which
// message, and keeps Slack in step.
type Engine struct {
	*Rebuilder
	source Source
	opts   Options
}

// New builds the reconciler. Zero options take the package defaults.
func New(d Domain, store *notify.Store, n *notify.Notifier, opts Options) *Engine {
	if opts.MaxItems <= 0 {
		opts.MaxItems = DefaultMaxItems
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = DefaultCooldown
	}
	return &Engine{Rebuilder: NewRebuilder(d, store, n), source: d, opts: opts}
}

// WithClock overrides the clock; intended for tests.
func (e *Engine) WithClock(now func() time.Time) *Engine {
	e.Rebuilder.WithClock(now)
	return e
}

// Run performs one digest pass.
//
// The rules, in the order they are applied to each item:
//
//   - untracked and actionable        -> a candidate to join the next digest
//   - untracked and not actionable    -> ignored; nothing is announced that was
//     never worth announcing
//   - tracked, within cooldown        -> stays where it is; its row is
//     refreshed in place
//   - tracked, cooled, still live     -> moves: removed from its old post and
//     included in the new one
//   - tracked, cooled, done           -> purged; a struck-through row does not
//     rotate into a fresh digest
//
// Anything past its cooldown that misses the cap stays exactly where it is and
// leads the queue next pass — it is never removed without somewhere to go.
//
// Every existing post is then rebuilt from the items that remain in it. That is
// idempotent by construction: the fingerprint gate means a rebuild that changes
// nothing makes no Slack call, so running a pass twice costs two upstream reads
// and no writes.
func (e *Engine) Run(ctx context.Context, target slack.Target, dryRun bool) (Report, error) {
	now := e.now()

	tracked, err := e.store.ItemsInStream(ctx, e.renderer.Stream())
	if err != nil {
		return Report{}, err
	}
	trackedByID := make(map[string]notify.KeyedItem, len(tracked))
	for _, it := range tracked {
		trackedByID[e.ID(it.Key)] = it
	}

	candidates, err := e.source.Candidates(ctx, trackedByID)
	if err != nil {
		return Report{}, err
	}
	report := Report{Considered: len(candidates), DryRun: dryRun, Noun: e.renderer.Noun()}

	// Partition against the ledger.
	var joining []Candidate
	staying := map[string]Candidate{}
	purged := map[string]bool{}

	for _, c := range candidates {
		item, isTracked := trackedByID[c.ID]
		switch {
		case !isTracked:
			if !c.Done {
				joining = append(joining, c)
			}
		case !item.Cooled(now, e.opts.Cooldown):
			staying[c.ID] = c
		case c.Done:
			purged[c.ID] = true
			report.Purged = append(report.Purged, c.ID)
		default:
			joining = append(joining, c)
		}
	}

	// A tracked item the source no longer reports at all cannot be refreshed. It
	// is held until its cooldown expires and then purged, so a deleted repository
	// or a lost permission cannot pin a row in place forever.
	//
	// While it is held it renders from the LEDGER, not from a stub. Rebuilding it
	// as a bare id was the one path that still reintroduced the failure §9b
	// exists to prevent: a transient read failure would redraw the row without
	// its title or its link, and then write that back — losing the real values
	// permanently, for a pull request that was only briefly unreadable.
	for id, item := range trackedByID {
		if _, seen := staying[id]; seen {
			continue
		}
		if containsID(joining, id) || purged[id] {
			continue
		}
		if item.Cooled(now, e.opts.Cooldown) {
			purged[id] = true
			report.Purged = append(report.Purged, id)
		} else {
			staying[id] = FromItem(id, item.Item)
		}
	}

	// Oldest first: the queue is FIFO by the item's own age, not by when Riggs
	// happened to notice it.
	sort.SliceStable(joining, func(i, j int) bool {
		return joining[i].CreatedAt.Before(joining[j].CreatedAt)
	})

	selected := joining
	if len(selected) > e.opts.MaxItems {
		// The ones that miss the cap keep their current home and lead the queue
		// next pass. They are deliberately NOT removed from it: a row taken out
		// with nowhere to go would simply vanish.
		for _, c := range selected[e.opts.MaxItems:] {
			if _, isTracked := trackedByID[c.ID]; isTracked {
				staying[c.ID] = c
				report.Held = append(report.Held, c.ID)
			}
		}
		selected = selected[:e.opts.MaxItems]
	}
	for _, c := range selected {
		report.Posted = append(report.Posted, c.ID)
	}

	if dryRun {
		report.sortAll()
		return report, nil
	}

	moving := map[string]bool{}
	for _, c := range selected {
		moving[c.ID] = true
	}
	if err := e.rebuildPosts(ctx, target, tracked, staying, moving, purged, &report); err != nil {
		return report, err
	}
	if err := e.postDigest(ctx, target, selected, now); err != nil {
		return report, err
	}

	report.sortAll()
	return report, nil
}

// rebuildPosts rewrites every existing digest from the items that remain in it,
// deleting the ones that empty out.
func (e *Engine) rebuildPosts(ctx context.Context, target slack.Target,
	tracked []notify.KeyedItem, staying map[string]Candidate,
	moving, purged map[string]bool, report *Report) error {

	postKeys, byPost := notify.GroupByPost(tracked)

	for _, postKey := range postKeys {
		var rows []blockkit.Row
		var keep []notify.KeyedItem

		for _, it := range byPost[postKey] {
			id := e.ID(it.Key)
			if moving[id] || purged[id] {
				continue
			}
			c, ok := staying[id]
			if !ok {
				// Neither moving, purged nor refreshed: render it from the
				// ledger, which holds everything the row needs.
				c = FromItem(id, it.Item)
			}
			rows = append(rows, e.renderer.Row(c))
			keep = append(keep, it)
		}

		// Forget the rows that left this post before it is rewritten, so a
		// failure part-way cannot leave an item pointing at a message it is no
		// longer in.
		for _, it := range byPost[postKey] {
			if purged[e.ID(it.Key)] {
				if err := e.store.DeleteItem(ctx, it.Key); err != nil {
					return err
				}
			}
		}

		if len(rows) == 0 {
			if err := e.notifier.DeleteDigest(ctx, postKey, target); err != nil {
				return err
			}
			report.Deleted = append(report.Deleted, postKey)
			continue
		}

		outcome, err := e.notifier.UpdateDigest(ctx, postKey, target,
			e.digest(rows), e.renderer.Fallback(rows))
		if err != nil {
			return err
		}
		if outcome == notify.Updated {
			report.Updated = append(report.Updated, postKey)
		}
		// Re-record positions so a later rebuild preserves what the reader sees.
		for i, it := range keep {
			updated := it.Item
			updated.Position = i
			if c, ok := staying[e.ID(it.Key)]; ok {
				updated.Status, updated.Done = c.Status, c.Done
				updated.Title, updated.Author, updated.URL = c.Title, c.Author, c.URL
			}
			updated.UpdatedAt = e.now()
			if err := e.store.SaveItem(ctx, it.Key, updated); err != nil {
				return err
			}
		}
	}
	return nil
}

// postDigest posts the new digest and records its membership.
func (e *Engine) postDigest(ctx context.Context, target slack.Target, selected []Candidate, now time.Time) error {
	if len(selected) == 0 {
		return nil
	}
	rows := make([]blockkit.Row, 0, len(selected))
	for _, c := range selected {
		rows = append(rows, e.renderer.Row(c))
	}

	postKey, err := e.store.NextPostKey(ctx, e.renderer.PostPrefix())
	if err != nil {
		return err
	}
	if _, err := e.notifier.PostDigest(ctx, postKey, target,
		e.digest(rows), e.renderer.Fallback(rows)); err != nil {
		return err
	}

	for i, c := range selected {
		if err := e.store.SaveItem(ctx, e.Key(c.ID), notify.Item{
			Stream:   e.renderer.Stream(),
			PostKey:  postKey,
			Position: i,
			Title:    c.Title,
			Author:   c.Author,
			URL:      c.URL,
			Status:   c.Status,
			Done:     c.Done,
			// The cooldown anchor moves only here — on entry to a NEW post.
			PostedAt:  now,
			UpdatedAt: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// FromItem rebuilds a candidate from what the ledger recorded, for a pass that
// has no fresh upstream read of it.
//
// An item stored before the row columns existed has no title; the id is a poor
// one but it is never a wrong one.
func FromItem(id string, it notify.Item) Candidate {
	c := Candidate{
		ID: id, Title: it.Title, Author: it.Author, URL: it.URL,
		Status: it.Status, Done: it.Done,
	}
	if c.Title == "" {
		c.Title = id
	}
	return c
}

// containsID reports whether id is among the candidates.
func containsID(cs []Candidate, id string) bool {
	for _, c := range cs {
		if c.ID == id {
			return true
		}
	}
	return false
}
