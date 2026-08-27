package pullrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// --- harness ---------------------------------------------------------------

// bulkRig assembles a digest engine over a temp ledger and a fake Slack, with a
// clock the test drives.
type bulkRig struct {
	bulk  *BulkEngine
	gh    *fakeGH
	slack *slacktest.Fake
	store *notify.Store
	// notifier and engine are kept so a test can rebuild the digest engine with
	// different options against the SAME ledger — the state lives there, not in
	// the engine, so a rebuilt engine picks up exactly where the last one left
	// off.
	notifier *notify.Notifier
	engine   *Engine
	clock    time.Time
}

var epoch = time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)

func newBulkRig(t *testing.T, gh *fakeGH, opts BulkOptions) *bulkRig {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	fake := slacktest.New()
	notifier := notify.New(store, fake)
	engine := NewEngine(gh, store, notifier, "miere", "U1")

	r := &bulkRig{gh: gh, slack: fake, store: store, notifier: notifier, engine: engine, clock: epoch}
	r.bulk = NewBulkEngine(engine, store, notifier, opts).WithClock(func() time.Time { return r.clock })
	return r
}

// reopen rebuilds the digest engine with new options over the same ledger.
func (r *bulkRig) reopen(opts BulkOptions) {
	r.bulk = NewBulkEngine(r.engine, r.store, r.notifier, opts).
		WithClock(func() time.Time { return r.clock })
}

// run performs one pass and fails the test on error.
func (r *bulkRig) run(t *testing.T) BulkReport {
	t.Helper()
	report, err := r.bulk.Run(context.Background(), target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

// advance moves the clock on.
func (r *bulkRig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

// bulkPR builds a green, review-requested pull request created at age before
// the epoch — so a larger age sorts earlier under FIFO.
func bulkPR(ref string, age time.Duration, author string) github.Detail {
	d := openPR(ref)
	d.Author = author
	created := epoch.Add(-age)
	d.CreatedAt = &created
	return d
}

// bulkGH scripts a GitHub where every listed ref is open, green and requested.
func bulkGH(details ...github.Detail) *fakeGH {
	f := &fakeGH{
		details: map[string]github.Detail{},
		checks:  map[string][]github.Check{},
	}
	for _, d := range details {
		ref := d.Ref()
		f.details[ref] = d
		f.checks[d.HeadSHA] = []github.Check{run("build", "COMPLETED", "SUCCESS")}
		f.search = append(f.search, github.PullRequest{Repo: d.Repo, Number: d.Number})
	}
	return f
}

// rows decodes the blocks of the nth Slack call into its section rows.
func rows(t *testing.T, call slacktest.Call) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(call.Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	// The first block is the card header.
	return blocks[1:]
}

// refsOf lists the block ids of a call's rows, in order.
func refsOf(t *testing.T, call slacktest.Call) []string {
	t.Helper()
	var out []string
	for _, row := range rows(t, call) {
		out = append(out, fmt.Sprint(row["block_id"]))
	}
	return out
}

// optionsOf lists the option values of one row.
func optionsOf(t *testing.T, row map[string]any) []string {
	t.Helper()
	acc, ok := row["accessory"].(map[string]any)
	if !ok {
		return nil
	}
	var out []string
	for _, o := range acc["options"].([]any) {
		out = append(out, fmt.Sprint(o.(map[string]any)["value"]))
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- the tick algorithm ----------------------------------------------------

// Oldest first: the queue is FIFO by pull request age, not by when Riggs
// noticed it or how the refs happen to sort.
func TestBulkPostsOldestFirst(t *testing.T) {
	r := newBulkRig(t, bulkGH(
		bulkPR("o/r#1", 1*time.Hour, "hjed"),
		bulkPR("o/r#2", 72*time.Hour, "hjed"),
		bulkPR("o/r#3", 24*time.Hour, "hjed"),
	), BulkOptions{})

	r.run(t)

	posts := r.slack.Posts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want one digest", len(posts))
	}
	got := refsOf(t, posts[0])
	want := []string{"o/r#2", "o/r#3", "o/r#1"}
	if !equal(got, want) {
		t.Fatalf("digest order = %v, want %v", got, want)
	}
}

func TestBulkCapsTheDigest(t *testing.T) {
	r := newBulkRig(t, bulkGH(
		bulkPR("o/r#1", 4*time.Hour, "hjed"),
		bulkPR("o/r#2", 3*time.Hour, "hjed"),
		bulkPR("o/r#3", 2*time.Hour, "hjed"),
	), BulkOptions{MaxItems: 2})

	report := r.run(t)

	if got := refsOf(t, r.slack.Posts()[0]); len(got) != 2 {
		t.Fatalf("digest carried %d rows, want 2", len(got))
	}
	if len(report.Posted) != 2 {
		t.Fatalf("report.Posted = %v, want two", report.Posted)
	}
}

// Rule (b)(i): inside the cooldown an item is not re-listed, and its row is
// refreshed where it already is.
func TestBulkRefreshesInPlaceWithinCooldown(t *testing.T) {
	gh := bulkGH(bulkPR("o/r#1", time.Hour, "hjed"))
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	// The checks go red: same item, new status, still inside its cooldown.
	gh.checks["sha-o/r#1"] = []github.Check{run("build", "COMPLETED", "FAILURE")}
	r.advance(time.Hour)
	report := r.run(t)

	if len(r.slack.Posts()) != 0 {
		t.Fatalf("posted a new digest inside the cooldown: %v", r.slack.Kinds())
	}
	if len(report.Posted) != 0 {
		t.Fatalf("report.Posted = %v, want empty", report.Posted)
	}
	if kinds := r.slack.Kinds(); len(kinds) != 1 || kinds[0] != "update" {
		t.Fatalf("calls = %v, want a single update", kinds)
	}
}

// Rule (b)(ii): past the cooldown a still-open item joins the new list and is
// removed from the message it was in.
func TestBulkRotatesACooledItemIntoANewPost(t *testing.T) {
	gh := bulkGH(
		bulkPR("o/r#1", 4*time.Hour, "hjed"),
		bulkPR("o/r#2", 3*time.Hour, "hjed"),
	)
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	r.advance(DefaultCooldown + time.Minute)
	report := r.run(t)

	posts := r.slack.Posts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want one new digest", len(posts))
	}
	if got := refsOf(t, posts[0]); !equal(got, []string{"o/r#1", "o/r#2"}) {
		t.Fatalf("new digest = %v", got)
	}
	// Everything left the first post, so it must be deleted rather than
	// updated to an empty shell.
	if len(r.slack.Deleted()) != 1 {
		t.Fatalf("calls = %v, want the emptied post deleted", r.slack.Kinds())
	}
	if len(report.Deleted) != 1 {
		t.Fatalf("report.Deleted = %v", report.Deleted)
	}
}

// A post that loses some but not all of its rows is updated, not deleted.
func TestBulkUpdatesAPartiallyEmptiedPost(t *testing.T) {
	old := bulkPR("o/r#1", 4*time.Hour, "hjed")
	fresh := bulkPR("o/r#2", 3*time.Hour, "hjed")
	gh := bulkGH(old, fresh)
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)

	// Only #1 has cooled: post it again, and leave #2 where it is.
	r.slack.Reset()
	r.advance(DefaultCooldown + time.Minute)

	// Re-anchor #2 by pretending it was posted more recently.
	item, _, err := r.store.Item(context.Background(), BulkItemPrefix+"o/r#2")
	if err != nil {
		t.Fatalf("Item: %v", err)
	}
	item.PostedAt = r.clock
	if err := r.store.SaveItem(context.Background(), BulkItemPrefix+"o/r#2", item); err != nil {
		t.Fatalf("SaveItem: %v", err)
	}

	r.run(t)

	if len(r.slack.Deleted()) != 0 {
		t.Fatalf("deleted a post that still had a row: %v", r.slack.Kinds())
	}
	updated := 0
	for _, k := range r.slack.Kinds() {
		if k == "update" {
			updated++
		}
	}
	if updated != 1 {
		t.Fatalf("calls = %v, want the old post updated in place", r.slack.Kinds())
	}
	if got := refsOf(t, r.slack.Posts()[0]); !equal(got, []string{"o/r#1"}) {
		t.Fatalf("new digest = %v, want only the cooled item", got)
	}
}

// A done row is struck through in place and loses everything but its link.
func TestBulkStrikesADoneRowInPlace(t *testing.T) {
	gh := bulkGH(bulkPR("o/r#1", time.Hour, "hjed"))
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	// Merged: not reviewable any more.
	d := gh.details["o/r#1"]
	d.Merged, d.State = true, "closed"
	merged := epoch.Add(30 * time.Minute)
	d.MergedAt = &merged
	gh.details["o/r#1"] = d

	r.advance(time.Hour)
	r.run(t)

	calls := r.slack.Calls
	if len(calls) != 1 || calls[0].Kind != "update" {
		t.Fatalf("calls = %v, want one in-place update", r.slack.Kinds())
	}
	row := rows(t, calls[0])[0]
	text := row["text"].(map[string]any)["text"].(string)
	if text[:2] != "~*" {
		t.Fatalf("done row is not struck through: %q", text)
	}
	if got := optionsOf(t, row); !equal(got, []string{IntentOpenBrowser}) {
		t.Fatalf("done row options = %v, want only the link", got)
	}
}

// A done row does not rotate into a fresh digest; once its cooldown expires it
// is purged, which is what eventually empties a post.
func TestBulkPurgesADoneRowAfterItsCooldown(t *testing.T) {
	gh := bulkGH(bulkPR("o/r#1", time.Hour, "hjed"))
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	d := gh.details["o/r#1"]
	d.Merged, d.State = true, "closed"
	gh.details["o/r#1"] = d

	r.advance(DefaultCooldown + time.Minute)
	report := r.run(t)

	if len(r.slack.Posts()) != 0 {
		t.Fatalf("a done row rotated into a new digest: %v", r.slack.Kinds())
	}
	if len(report.Purged) != 1 || report.Purged[0] != "o/r#1" {
		t.Fatalf("report.Purged = %v", report.Purged)
	}
	if len(r.slack.Deleted()) != 1 {
		t.Fatalf("emptied post was not deleted: %v", r.slack.Kinds())
	}
	if _, found, _ := r.store.Item(context.Background(), BulkItemPrefix+"o/r#1"); found {
		t.Fatal("purged item is still in the ledger")
	}
}

// Rule (b)(ii) with a cap: the ones that miss the cut stay exactly where they
// are rather than being removed with nowhere to go.
func TestBulkHoldsWhatMissesTheCap(t *testing.T) {
	gh := bulkGH(
		bulkPR("o/r#1", 5*time.Hour, "hjed"),
		bulkPR("o/r#2", 4*time.Hour, "hjed"),
	)
	r := newBulkRig(t, gh, BulkOptions{MaxItems: 2})
	r.run(t)
	r.slack.Reset()

	// Both have cooled, but only one may move.
	r.reopen(BulkOptions{MaxItems: 1})
	r.advance(DefaultCooldown + time.Minute)
	report := r.run(t)

	if !equal(report.Posted, []string{"o/r#1"}) {
		t.Fatalf("report.Posted = %v, want the oldest only", report.Posted)
	}
	if !equal(report.Held, []string{"o/r#2"}) {
		t.Fatalf("report.Held = %v", report.Held)
	}
	if len(r.slack.Deleted()) != 0 {
		t.Fatalf("held row's post was deleted: %v", r.slack.Kinds())
	}
	// The held row must still be somewhere a reader can see it.
	item, found, err := r.store.Item(context.Background(), BulkItemPrefix+"o/r#2")
	if err != nil || !found {
		t.Fatalf("held item lost from the ledger: %v", err)
	}
	if item.PostKey == "" {
		t.Fatal("held item has no post")
	}
}

// The cooldown anchor moves only on entry to a NEW post. A busy pull request
// whose checks flip all morning must still age out of the message it was first
// announced in.
func TestBulkRefreshDoesNotResetTheCooldown(t *testing.T) {
	gh := bulkGH(bulkPR("o/r#1", time.Hour, "hjed"))
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)

	key := BulkItemPrefix + "o/r#1"
	before, _, _ := r.store.Item(context.Background(), key)

	// Two in-place refreshes inside the cooldown.
	for i, conclusion := range []string{"FAILURE", "SUCCESS"} {
		gh.checks["sha-o/r#1"] = []github.Check{run("build", "COMPLETED", conclusion)}
		r.advance(time.Duration(i+1) * 30 * time.Minute)
		r.run(t)
	}

	after, _, _ := r.store.Item(context.Background(), key)
	if !after.PostedAt.Equal(before.PostedAt) {
		t.Fatalf("PostedAt moved on an in-place refresh: %v -> %v", before.PostedAt, after.PostedAt)
	}
}

// --- rows ------------------------------------------------------------------

func TestBulkOffersApproveAndMergeOnlyToDependabot(t *testing.T) {
	r := newBulkRig(t, bulkGH(
		bulkPR("o/r#1", 2*time.Hour, "dependabot[bot]"),
		bulkPR("o/r#2", 1*time.Hour, "hjed"),
	), BulkOptions{})
	r.run(t)

	got := rows(t, r.slack.Posts()[0])
	bot, human := optionsOf(t, got[0]), optionsOf(t, got[1])

	if !equal(bot, []string{IntentOpenBrowser, IntentAskReview, IntentApproveMerge}) {
		t.Fatalf("dependabot row options = %v", bot)
	}
	if !equal(human, []string{IntentOpenBrowser, IntentAskReview}) {
		t.Fatalf("human row options = %v", human)
	}
}

// Approve is specified but explicitly not implemented, and a button that
// silently does nothing is worse than one that is not there.
func TestBulkNeverOffersApprove(t *testing.T) {
	r := newBulkRig(t, bulkGH(bulkPR("o/r#1", time.Hour, "dependabot[bot]")), BulkOptions{})
	r.run(t)

	for _, v := range optionsOf(t, rows(t, r.slack.Posts()[0])[0]) {
		if v == "approve_only" || v == "approve" {
			t.Fatalf("digest offered an approve option: %v", v)
		}
	}
}

func TestIsDependabot(t *testing.T) {
	for _, login := range []string{"dependabot", "dependabot[bot]", "Dependabot[bot]", " dependabot "} {
		if !IsDependabot(login) {
			t.Errorf("IsDependabot(%q) = false", login)
		}
	}
	for _, login := range []string{"", "hjed", "notdependabot"} {
		if IsDependabot(login) {
			t.Errorf("IsDependabot(%q) = true", login)
		}
	}
}

// --- idempotence -----------------------------------------------------------

// Running a pass twice must cost GitHub reads and no Slack writes: the
// fingerprint gate is what makes the every-minute schedule affordable.
func TestBulkSecondPassWritesNothing(t *testing.T) {
	r := newBulkRig(t, bulkGH(bulkPR("o/r#1", time.Hour, "hjed")), BulkOptions{})
	r.run(t)
	r.slack.Reset()

	r.advance(time.Minute)
	r.run(t)

	if kinds := r.slack.Kinds(); len(kinds) != 0 {
		t.Fatalf("second pass made Slack calls: %v", kinds)
	}
}

// A dry run reports what would happen and touches neither Slack nor the ledger.
func TestBulkDryRunIsHarmless(t *testing.T) {
	r := newBulkRig(t, bulkGH(bulkPR("o/r#1", time.Hour, "hjed")), BulkOptions{})

	report, err := r.bulk.Run(context.Background(), target, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !equal(report.Posted, []string{"o/r#1"}) {
		t.Fatalf("report.Posted = %v", report.Posted)
	}
	if kinds := r.slack.Kinds(); len(kinds) != 0 {
		t.Fatalf("dry run made Slack calls: %v", kinds)
	}
	if _, found, _ := r.store.Item(context.Background(), BulkItemPrefix+"o/r#1"); found {
		t.Fatal("dry run wrote to the ledger")
	}
}

// A pull request that dies before it is ever worth showing is never announced.
func TestBulkIgnoresADeadOnArrivalPR(t *testing.T) {
	gh := bulkGH(bulkPR("o/r#1", time.Hour, "hjed"))
	gh.checks["sha-o/r#1"] = []github.Check{run("build", "COMPLETED", "FAILURE")}

	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)

	if kinds := r.slack.Kinds(); len(kinds) != 0 {
		t.Fatalf("announced a never-reviewable PR: %v", kinds)
	}
}

// --- configuration ---------------------------------------------------------

func TestMaxItemsComesFromTheEnvironment(t *testing.T) {
	t.Setenv(MaxItemsEnv, "3")
	if got := (BulkOptions{}).resolved().MaxItems; got != 3 {
		t.Fatalf("MaxItems = %d, want 3", got)
	}

	// A typo must not stop the queue being delivered.
	t.Setenv(MaxItemsEnv, "not-a-number")
	if got := (BulkOptions{}).resolved().MaxItems; got != DefaultMaxItems {
		t.Fatalf("MaxItems = %d, want the default", got)
	}

	// An explicit option beats the environment.
	t.Setenv(MaxItemsEnv, "3")
	if got := (BulkOptions{MaxItems: 7}).resolved().MaxItems; got != 7 {
		t.Fatalf("MaxItems = %d, want 7", got)
	}
}

func TestBulkDefaultsToAThreeHourCooldown(t *testing.T) {
	if got := (BulkOptions{}).resolved().Cooldown; got != 3*time.Hour {
		t.Fatalf("Cooldown = %v, want 3h", got)
	}
}

// The digest carries its own icon, not the legacy card's const — the two
// shapes have separate lifecycles, and sharing it would mean a change to one
// silently re-rendering every card of the other.
func TestDigestUsesItsOwnIcon(t *testing.T) {
	r := newBulkRig(t, bulkGH(bulkPR("o/r#1", time.Hour, "hjed")), BulkOptions{})
	r.run(t)

	raw, err := json.Marshal(r.slack.Posts()[0].Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	icon := blocks[0]["icon"].(map[string]any)
	if got := icon["image_url"]; got != bulkIconURL {
		t.Fatalf("digest icon = %v, want %q", got, bulkIconURL)
	}
	if bulkIconURL == iconURL {
		t.Error("the digest and the legacy card share an icon const")
	}
}
