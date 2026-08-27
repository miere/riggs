package pullrequest

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

// The bulk digest: one message carrying up to N pull requests, rather than one
// self-updating card per pull request.
//
// The card loop (reconcile.go) is untouched and still runs. This is a second,
// independent consumer of the same GitHub reads and the same ledger database.

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
		o.MaxItems = maxItemsFromEnv()
	}
	if o.Cooldown <= 0 {
		o.Cooldown = DefaultCooldown
	}
	return o
}

// maxItemsFromEnv reads the cap, falling back to the default. A value that is
// not a positive integer is ignored rather than fatal: a typo in a job's
// environment should not stop the queue being delivered.
func maxItemsFromEnv() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(MaxItemsEnv))); err == nil && n > 0 {
		return n
	}
	return DefaultMaxItems
}

// BulkEngine reconciles the digest.
type BulkEngine struct {
	engine   *Engine
	store    *notify.Store
	notifier *notify.Notifier
	now      func() time.Time
	opts     BulkOptions
}

// NewBulkEngine builds the digest reconciler over the card engine's GitHub
// reads and the shared ledger.
func NewBulkEngine(e *Engine, store *notify.Store, n *notify.Notifier, opts BulkOptions) *BulkEngine {
	return &BulkEngine{engine: e, store: store, notifier: n, now: time.Now, opts: opts.resolved()}
}

// WithClock overrides the clock; intended for tests.
func (b *BulkEngine) WithClock(now func() time.Time) *BulkEngine {
	b.now = now
	return b
}

// candidate is one pull request as the digest needs it.
type candidate struct {
	Ref       string
	Title     string
	Author    string
	URL       string
	CreatedAt time.Time
	Status    string
	// Done marks a row that is struck through — reviewed, merged, closed or
	// otherwise no longer actionable.
	Done bool
	// Dependabot gates the approve-and-merge option.
	Dependabot bool
}

// BulkReport is the result of one digest pass.
type BulkReport struct {
	Considered int      `json:"considered"`
	Posted     []string `json:"posted,omitempty"`
	Held       []string `json:"held,omitempty"`
	Purged     []string `json:"purged,omitempty"`
	Updated    []string `json:"updated_posts,omitempty"`
	Deleted    []string `json:"deleted_posts,omitempty"`
	DryRun     bool     `json:"dry_run"`
}

// String renders the report for a human.
func (r BulkReport) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[dry run] ")
	}
	fmt.Fprintf(&b, "considered %d pull request(s)\n", r.Considered)
	line := func(label string, refs []string) {
		if len(refs) > 0 {
			fmt.Fprintf(&b, "  %-9s %s\n", label, strings.Join(refs, ", "))
		}
	}
	line("posted", r.Posted)
	line("held", r.Held)
	line("purged", r.Purged)
	line("updated", r.Updated)
	line("deleted", r.Deleted)
	return strings.TrimRight(b.String(), "\n")
}

// Run performs one digest pass.
//
// The rules, in the order they are applied to each pull request:
//
//   - untracked and actionable        -> a candidate to join the next digest
//   - untracked and not actionable    -> ignored; nothing is announced that was
//     never worth announcing (the same
//     dead-on-arrival rule as the card loop)
//   - tracked, within cooldown        -> stays where it is; its row is
//     refreshed in place
//   - tracked, cooled, still open     -> moves: removed from its old post and
//     included in the new one
//   - tracked, cooled, done           -> purged; a struck-through row does not
//     rotate into a fresh digest
//
// Anything past its cooldown that misses the cap stays exactly where it is and
// leads the queue next pass — it is never removed without somewhere to go.
//
// Every existing post is then rebuilt from the items that remain in it. That is
// idempotent by construction: the fingerprint gate means a rebuild that changes
// nothing makes no Slack call, so running a pass twice costs two GitHub reads
// and no writes.
func (b *BulkEngine) Run(ctx context.Context, target slack.Target, dryRun bool) (BulkReport, error) {
	now := b.now()

	tracked, err := b.store.ItemsInStream(ctx, BulkStream)
	if err != nil {
		return BulkReport{}, err
	}
	trackedByRef := make(map[string]notify.KeyedItem, len(tracked))
	for _, it := range tracked {
		trackedByRef[strings.TrimPrefix(it.Key, BulkItemPrefix)] = it
	}

	candidates, err := b.candidates(ctx, trackedByRef)
	if err != nil {
		return BulkReport{}, err
	}
	report := BulkReport{Considered: len(candidates), DryRun: dryRun}

	// Partition against the ledger.
	var joining []candidate
	staying := map[string]candidate{}
	purged := map[string]bool{}

	for _, c := range candidates {
		item, isTracked := trackedByRef[c.Ref]
		switch {
		case !isTracked:
			if !c.Done {
				joining = append(joining, c)
			}
		case !item.Cooled(now, b.opts.Cooldown):
			staying[c.Ref] = c
		case c.Done:
			purged[c.Ref] = true
			report.Purged = append(report.Purged, c.Ref)
		default:
			joining = append(joining, c)
		}
	}

	// A tracked item GitHub no longer reports at all cannot be refreshed. It is
	// held until its cooldown expires and then purged, so a deleted repository
	// or a lost permission cannot pin a row in place forever.
	for ref, item := range trackedByRef {
		if _, seen := staying[ref]; seen {
			continue
		}
		if containsRef(joining, ref) || purged[ref] {
			continue
		}
		if item.Cooled(now, b.opts.Cooldown) {
			purged[ref] = true
			report.Purged = append(report.Purged, ref)
		} else {
			staying[ref] = candidate{Ref: ref, Title: ref, Status: item.Status, Done: item.Done}
		}
	}

	// Oldest first: the queue is FIFO by pull request age, not by when Riggs
	// happened to notice it.
	sort.SliceStable(joining, func(i, j int) bool {
		return joining[i].CreatedAt.Before(joining[j].CreatedAt)
	})

	selected := joining
	if len(selected) > b.opts.MaxItems {
		// The ones that miss the cap keep their current home and lead the queue
		// next pass. They are deliberately NOT removed from it: a row taken out
		// with nowhere to go would simply vanish.
		for _, c := range selected[b.opts.MaxItems:] {
			if _, isTracked := trackedByRef[c.Ref]; isTracked {
				staying[c.Ref] = c
				report.Held = append(report.Held, c.Ref)
			}
		}
		selected = selected[:b.opts.MaxItems]
	}
	for _, c := range selected {
		report.Posted = append(report.Posted, c.Ref)
	}

	if dryRun {
		sortAll(&report)
		return report, nil
	}

	moving := map[string]bool{}
	for _, c := range selected {
		moving[c.Ref] = true
	}
	if err := b.rebuildPosts(ctx, target, tracked, staying, moving, purged, &report); err != nil {
		return report, err
	}
	if err := b.postDigest(ctx, target, selected, now); err != nil {
		return report, err
	}

	sortAll(&report)
	return report, nil
}

// rebuildPosts rewrites every existing digest from the items that remain in it,
// deleting the ones that empty out.
func (b *BulkEngine) rebuildPosts(ctx context.Context, target slack.Target,
	tracked []notify.KeyedItem, staying map[string]candidate,
	moving, purged map[string]bool, report *BulkReport) error {

	postKeys, byPost := notify.GroupByPost(tracked)

	for _, postKey := range postKeys {
		var rows []blockkit.Row
		var keep []notify.KeyedItem

		for _, it := range byPost[postKey] {
			ref := strings.TrimPrefix(it.Key, BulkItemPrefix)
			if moving[ref] || purged[ref] {
				continue
			}
			c, ok := staying[ref]
			if !ok {
				// Neither moving, purged nor refreshed: render it from the
				// ledger, which holds everything the row needs.
				c = itemCandidate(ref, it.Item)
			}
			rows = append(rows, c.row())
			keep = append(keep, it)
		}

		// Forget the rows that left this post before it is rewritten, so a
		// failure part-way cannot leave an item pointing at a message it is no
		// longer in.
		for _, it := range byPost[postKey] {
			ref := strings.TrimPrefix(it.Key, BulkItemPrefix)
			if purged[ref] {
				if err := b.store.DeleteItem(ctx, it.Key); err != nil {
					return err
				}
			}
		}

		if len(rows) == 0 {
			if err := b.notifier.DeleteDigest(ctx, postKey, target); err != nil {
				return err
			}
			report.Deleted = append(report.Deleted, postKey)
			continue
		}

		digest := blockkit.Digest{
			Title: bulkTitle, Subtitle: bulkSubtitle,
			IconURL: bulkIconURL, IconAlt: "GitHub", Rows: rows,
		}
		outcome, err := b.notifier.UpdateDigest(ctx, postKey, target, digest, bulkFallback(rows))
		if err != nil {
			return err
		}
		if outcome == notify.Updated {
			report.Updated = append(report.Updated, postKey)
		}
		// Re-record positions so a later rebuild preserves what the reader sees.
		for i, it := range keep {
			ref := strings.TrimPrefix(it.Key, BulkItemPrefix)
			updated := it.Item
			updated.Position = i
			if c, ok := staying[ref]; ok {
				updated.Status, updated.Done = c.Status, c.Done
				updated.Title, updated.Author, updated.URL = c.Title, c.Author, c.URL
			}
			updated.UpdatedAt = b.now()
			if err := b.store.SaveItem(ctx, it.Key, updated); err != nil {
				return err
			}
		}
	}
	return nil
}

// postDigest posts the new digest and records its membership.
func (b *BulkEngine) postDigest(ctx context.Context, target slack.Target, selected []candidate, now time.Time) error {
	if len(selected) == 0 {
		return nil
	}
	rows := make([]blockkit.Row, 0, len(selected))
	for _, c := range selected {
		rows = append(rows, c.row())
	}

	postKey, err := b.store.NextPostKey(ctx, BulkPostPrefix)
	if err != nil {
		return err
	}
	digest := blockkit.Digest{
		Title: bulkTitle, Subtitle: bulkSubtitle,
		IconURL: bulkIconURL, IconAlt: "GitHub", Rows: rows,
	}
	if _, err := b.notifier.PostDigest(ctx, postKey, target, digest, bulkFallback(rows)); err != nil {
		return err
	}

	for i, c := range selected {
		if err := b.store.SaveItem(ctx, BulkItemPrefix+c.Ref, notify.Item{
			Stream:   BulkStream,
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

// candidates resolves every pull request this pass is responsible for:
//
//	(we are a direct requested reviewer) ∪ (already in the digest)
func (b *BulkEngine) candidates(ctx context.Context, tracked map[string]notify.KeyedItem) ([]candidate, error) {
	seen := map[string]bool{}

	prs, err := b.engine.gh.ReviewRequested(ctx, b.engine.login, searchLimit)
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

	out := make([]candidate, 0, len(refs))
	for _, ref := range refs {
		repo, number, err := SplitRef(ref)
		if err != nil {
			continue
		}
		d, err := b.engine.gh.PullRequestDetail(ctx, repo, number)
		if err != nil {
			// A read that failed this pass must not look like a pull request
			// that disappeared, so it is simply skipped and retried next time.
			continue
		}
		if d.Draft {
			continue
		}
		_, isTracked := tracked[ref]
		if !d.Requested(b.engine.login) && !isTracked {
			// A team-only request nobody adopted.
			continue
		}
		r, err := b.engine.resolveFrom(ctx, d, isTracked)
		if err != nil {
			continue
		}

		c := candidate{
			Ref: ref, Title: d.Title, Author: d.Author, URL: d.URL,
			Status: r.State.Token(), Done: !r.State.Reviewable,
			Dependabot: IsDependabot(d.Author),
		}
		if d.CreatedAt != nil {
			c.CreatedAt = *d.CreatedAt
		}
		out = append(out, c)
	}
	return out, nil
}

// itemCandidate rebuilds a row from what the ledger recorded, for a pass that
// has no fresh upstream read of it.
func itemCandidate(ref string, it notify.Item) candidate {
	c := candidate{
		Ref: ref, Title: it.Title, Author: it.Author, URL: it.URL,
		Status: it.Status, Done: it.Done, Dependabot: IsDependabot(it.Author),
	}
	if c.Title == "" {
		// Pre-dates the stored columns. The reference is a poor title but it is
		// never a wrong one.
		c.Title = ref
	}
	return c
}

// row renders one candidate as a digest row.
//
// A done row keeps only the link: there is nothing left to approve or ask about
// on a pull request that has been reviewed, merged or closed.
func (c candidate) row() blockkit.Row {
	row := blockkit.Row{
		BlockID:  c.Ref,
		Title:    c.Title,
		Meta:     c.meta(),
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
	if c.Dependabot {
		row.Options = append(row.Options,
			blockkit.MenuOption{Text: blockkit.MarkerDone + "  Approve and Merge", Value: IntentApproveMerge})
	}
	return row
}

// meta is the row's second line: the reference, and who wrote it.
func (c candidate) meta() string {
	if c.Author == "" {
		return "_" + c.Ref + "_"
	}
	return fmt.Sprintf("_%s_ by `@%s`", c.Ref, c.Author)
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

// containsRef reports whether ref is among the candidates.
func containsRef(cs []candidate, ref string) bool {
	for _, c := range cs {
		if c.Ref == ref {
			return true
		}
	}
	return false
}

// sortAll makes the report deterministic, so a test (and a human diffing two
// runs) sees a stable order.
func sortAll(r *BulkReport) {
	for _, s := range [][]string{r.Posted, r.Held, r.Purged, r.Updated, r.Deleted} {
		sort.Strings(s)
	}
}
