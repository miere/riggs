package ticket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// --- harness ---------------------------------------------------------------

// bulkRig assembles a ticket digest over a temp ledger and a fake Slack, with a
// clock the test drives.
type bulkRig struct {
	bulk  *BulkEngine
	jira  *fakeJira
	slack *slacktest.Fake
	store *notify.Store
	// notifier is kept so a test can rebuild the engine with different options
	// against the SAME ledger — the state lives there, not in the engine.
	notifier *notify.Notifier
	jql      string
	clock    time.Time
}

var bulkEpoch = time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)

const testJQL = `project = NYX AND labels = "ai-able" AND assignee IS EMPTY AND status = "Ready"`

func newBulkRig(t *testing.T, fj *fakeJira, opts BulkOptions) *bulkRig {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	fake := slacktest.New()
	notifier := notify.New(store, fake)

	r := &bulkRig{jira: fj, slack: fake, store: store, notifier: notifier, jql: testJQL, clock: bulkEpoch}
	r.bulk = NewBulkEngine(fj, store, notifier, testJQL, opts).
		WithClock(func() time.Time { return r.clock })
	return r
}

// reopen rebuilds the digest engine with new options over the same ledger.
func (r *bulkRig) reopen(opts BulkOptions) {
	r.bulk = NewBulkEngine(r.jira, r.store, r.notifier, r.jql, opts).
		WithClock(func() time.Time { return r.clock })
}

func (r *bulkRig) run(t *testing.T) BulkReport {
	t.Helper()
	report, err := r.bulk.Run(context.Background(), target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

func (r *bulkRig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

// ticketAged builds an unclaimed Ready ticket created age before the epoch — so
// a larger age sorts earlier under FIFO.
func ticketAged(key string, age time.Duration) jira.Issue {
	return jira.Issue{
		Key:      key,
		Summary:  key + " needs doing",
		Status:   ReadyStatus,
		Reporter: "Miere Teixeira",
		Created:  bulkEpoch.Add(-age),
		Updated:  bulkEpoch.Add(-age),
	}
}

// bulkJira scripts a Jira where every listed ticket matches the query and can
// also be re-read by key.
func bulkJira(issues ...jira.Issue) *fakeJira {
	f := &fakeJira{issues: map[string]jira.Issue{}}
	for _, i := range issues {
		f.search = append(f.search, i)
		f.issues[i.Key] = i
	}
	return f
}

// blocksOf decodes a call's blocks; the first is the card header.
func blocksOf(t *testing.T, call slacktest.Call) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(call.Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal blocks: %v", err)
	}
	return blocks
}

func bulkRows(t *testing.T, call slacktest.Call) []map[string]any {
	t.Helper()
	return blocksOf(t, call)[1:]
}

func keysOf(t *testing.T, call slacktest.Call) []string {
	t.Helper()
	var out []string
	for _, row := range bulkRows(t, call) {
		out = append(out, fmt.Sprint(row["block_id"]))
	}
	return out
}

func bulkOptionsOf(t *testing.T, row map[string]any) []string {
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

func sameStrings(a, b []string) bool {
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

// Oldest first: the queue is FIFO by how long the ticket has been waiting, not
// by when Riggs noticed it or how the keys happen to sort.
func TestTicketBulkPostsOldestFirst(t *testing.T) {
	r := newBulkRig(t, bulkJira(
		ticketAged("NYX-1", 1*time.Hour),
		ticketAged("NYX-2", 72*time.Hour),
		ticketAged("NYX-3", 24*time.Hour),
	), BulkOptions{})

	r.run(t)

	posts := r.slack.Posts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want one digest", len(posts))
	}
	got := keysOf(t, posts[0])
	want := []string{"NYX-2", "NYX-3", "NYX-1"}
	if !sameStrings(got, want) {
		t.Fatalf("digest order = %v, want %v", got, want)
	}
}

func TestTicketBulkCapsTheDigest(t *testing.T) {
	r := newBulkRig(t, bulkJira(
		ticketAged("NYX-1", 4*time.Hour),
		ticketAged("NYX-2", 3*time.Hour),
		ticketAged("NYX-3", 2*time.Hour),
	), BulkOptions{MaxItems: 2})

	report := r.run(t)

	if got := keysOf(t, r.slack.Posts()[0]); len(got) != 2 {
		t.Fatalf("digest carried %d rows, want 2", len(got))
	}
	if len(report.Posted) != 2 {
		t.Fatalf("report.Posted = %v, want two", report.Posted)
	}
}

// Inside the cooldown a ticket is not re-listed: the whole point of the digest
// is that the same thing does not arrive twice in a period.
func TestTicketBulkDoesNotRepostWithinCooldown(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)), BulkOptions{})
	r.run(t)
	r.slack.Reset()

	r.advance(DefaultCooldown - time.Minute)
	report := r.run(t)

	if len(r.slack.Posts()) != 0 {
		t.Fatalf("posted a new digest inside the cooldown: %v", r.slack.Kinds())
	}
	if len(report.Posted) != 0 {
		t.Fatalf("report.Posted = %v, want empty", report.Posted)
	}
}

// Past the cooldown a still-unclaimed ticket joins a new digest and leaves the
// one it was in. That rolling re-post is what replaces the old idle nudge.
func TestTicketBulkRotatesACooledTicketIntoANewPost(t *testing.T) {
	r := newBulkRig(t, bulkJira(
		ticketAged("NYX-1", 4*time.Hour),
		ticketAged("NYX-2", 3*time.Hour),
	), BulkOptions{})
	r.run(t)
	r.slack.Reset()

	r.advance(DefaultCooldown + time.Minute)
	report := r.run(t)

	posts := r.slack.Posts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want one new digest", len(posts))
	}
	if got := keysOf(t, posts[0]); !sameStrings(got, []string{"NYX-1", "NYX-2"}) {
		t.Fatalf("new digest = %v", got)
	}
	// Everything left the first post, so it is deleted rather than left as an
	// empty shell.
	if len(r.slack.Deleted()) != 1 {
		t.Fatalf("calls = %v, want the emptied post deleted", r.slack.Kinds())
	}
	if len(report.Deleted) != 1 {
		t.Fatalf("report.Deleted = %v", report.Deleted)
	}
}

// A ticket claimed in Jira falls out of the query. The reader was shown it, so
// they are shown the outcome: struck through, in place.
func TestTicketBulkStrikesAClaimedTicketInPlace(t *testing.T) {
	live := ticketAged("NYX-1", time.Hour)
	fj := bulkJira(live)
	r := newBulkRig(t, fj, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	// Somebody took it: it no longer matches, but it is still readable.
	claimed := live
	claimed.Assignee = "Someone Else"
	claimed.Status = "In Progress"
	fj.search = nil
	fj.issues["NYX-1"] = claimed

	r.advance(time.Hour)
	r.run(t)

	updates := r.slack.Updates()
	if len(updates) != 1 {
		t.Fatalf("calls = %v, want a single update", r.slack.Kinds())
	}
	row := bulkRows(t, updates[0])[0]
	text := fmt.Sprint(row["text"].(map[string]any)["text"])
	if !strings.Contains(text, "~*") {
		t.Fatalf("row text = %q, want the title struck through", text)
	}
	// A struck-through row has nothing left to ask about.
	if got := bulkOptionsOf(t, row); !sameStrings(got, []string{IntentOpenBrowser}) {
		t.Fatalf("options = %v, want the link alone", got)
	}
}

// A ticket that cannot be READ is left exactly as it is. Striking it through on
// a transient Jira failure would claim it was handled when it may not have been.
func TestTicketBulkLeavesAnUnreadableTicketAlone(t *testing.T) {
	live := ticketAged("NYX-1", time.Hour)
	fj := bulkJira(live)
	r := newBulkRig(t, fj, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	fj.search = nil
	fj.getErr = errors.New("502 bad gateway")

	r.advance(time.Hour)
	r.run(t)

	// The row is still live, and still says what it said.
	item, found, err := r.store.Item(context.Background(), BulkItemPrefix+"NYX-1")
	if err != nil || !found {
		t.Fatalf("Item: %v found=%v", err, found)
	}
	if item.Done {
		t.Fatal("an unreadable ticket was struck through")
	}
	if item.Title != live.Summary {
		t.Fatalf("item.Title = %q, want the stored title kept", item.Title)
	}
	if item.URL == "" {
		t.Fatal("item.URL was lost, so the row's link would be dead")
	}
}

// Past its cooldown, a struck-through row is purged rather than rotated: a done
// ticket does not lead the next digest.
func TestTicketBulkPurgesADoneRowAfterItsCooldown(t *testing.T) {
	live := ticketAged("NYX-1", time.Hour)
	fj := bulkJira(live)
	r := newBulkRig(t, fj, BulkOptions{})
	r.run(t)

	claimed := live
	claimed.Assignee = "Someone Else"
	fj.search = nil
	fj.issues["NYX-1"] = claimed
	r.advance(time.Hour)
	r.run(t)
	r.slack.Reset()

	r.advance(DefaultCooldown)
	report := r.run(t)

	if !sameStrings(report.Purged, []string{"NYX-1"}) {
		t.Fatalf("report.Purged = %v", report.Purged)
	}
	if _, found, _ := r.store.Item(context.Background(), BulkItemPrefix+"NYX-1"); found {
		t.Fatal("the purged item is still tracked")
	}
	// It was the only row, so its post goes with it.
	if len(r.slack.Deleted()) != 1 {
		t.Fatalf("calls = %v, want the emptied post deleted", r.slack.Kinds())
	}
}

// Anything past its cooldown that misses the cap keeps its current home and
// leads the queue next pass. A row taken out with nowhere to go would vanish.
func TestTicketBulkHoldsWhatMissesTheCap(t *testing.T) {
	r := newBulkRig(t, bulkJira(
		ticketAged("NYX-1", 5*time.Hour),
		ticketAged("NYX-2", 4*time.Hour),
	), BulkOptions{MaxItems: 2})
	r.run(t)
	r.slack.Reset()

	r.reopen(BulkOptions{MaxItems: 1})
	r.advance(DefaultCooldown + time.Minute)
	report := r.run(t)

	if !sameStrings(report.Posted, []string{"NYX-1"}) {
		t.Fatalf("report.Posted = %v, want the oldest only", report.Posted)
	}
	if !sameStrings(report.Held, []string{"NYX-2"}) {
		t.Fatalf("report.Held = %v", report.Held)
	}
	if len(r.slack.Deleted()) != 0 {
		t.Fatalf("held row's post was deleted: %v", r.slack.Kinds())
	}
	item, found, err := r.store.Item(context.Background(), BulkItemPrefix+"NYX-2")
	if err != nil || !found {
		t.Fatalf("the held row stopped being tracked: %v found=%v", err, found)
	}
	if item.PostKey == "" {
		t.Fatal("the held row has no post to be seen in")
	}
}

// A refresh in place must not move the cooldown anchor. Otherwise a ticket
// whose description is edited every hour could never age out of the message it
// was first announced in.
func TestTicketBulkRefreshDoesNotResetTheCooldown(t *testing.T) {
	live := ticketAged("NYX-1", time.Hour)
	fj := bulkJira(live)
	r := newBulkRig(t, fj, BulkOptions{})
	r.run(t)
	postedAt := itemPostedAt(t, r, "NYX-1")

	edited := live
	edited.Summary = "NYX-1 needs doing, urgently"
	fj.search = []jira.Issue{edited}
	fj.issues["NYX-1"] = edited
	r.advance(time.Hour)
	r.run(t)

	if got := itemPostedAt(t, r, "NYX-1"); !got.Equal(postedAt) {
		t.Fatalf("posted_at moved from %s to %s on an in-place refresh", postedAt, got)
	}
}

func itemPostedAt(t *testing.T, r *bulkRig, key string) time.Time {
	t.Helper()
	item, found, err := r.store.Item(context.Background(), BulkItemPrefix+key)
	if err != nil || !found {
		t.Fatalf("Item(%s): %v found=%v", key, err, found)
	}
	return item.PostedAt
}

// --- the row ---------------------------------------------------------------

// A live row offers the link and the ask, and nothing else. "Assign to Me" is
// specified but explicitly not for this pass, and a control that silently does
// nothing is worse than one that is not there.
func TestTicketBulkRowOffersTheLinkAndTheAsk(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)),
		BulkOptions{Actions: RowActions{AskAssist: true}})
	r.run(t)

	row := bulkRows(t, r.slack.Posts()[0])[0]
	got := bulkOptionsOf(t, row)
	want := []string{IntentOpenBrowser, IntentAskAssist}
	if !sameStrings(got, want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
	if strings.Contains(fmt.Sprint(got), "assign") {
		t.Fatalf("options = %v, want no assign option", got)
	}
}

// The row's second line names the reporter, which is who you go to about scope.
func TestTicketBulkRowNamesTheReporter(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)), BulkOptions{})
	r.run(t)

	row := bulkRows(t, r.slack.Posts()[0])[0]
	text := fmt.Sprint(row["text"].(map[string]any)["text"])
	if !strings.Contains(text, "reported by `Miere Teixeira`") {
		t.Fatalf("row text = %q, want the reporter named", text)
	}
}

// The digest header is the ticket queue's, not a copy of the pull-request one.
func TestTicketDigestUsesItsOwnHeader(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)), BulkOptions{})
	r.run(t)

	header := blocksOf(t, r.slack.Posts()[0])[0]
	title := fmt.Sprint(header["title"].(map[string]any)["text"])
	if title != bulkTitle {
		t.Fatalf("header title = %q, want %q", title, bulkTitle)
	}
	subtitle := fmt.Sprint(header["subtitle"].(map[string]any)["text"])
	if strings.Contains(subtitle, "code review") {
		t.Fatalf("subtitle = %q, which is the pull-request queue's", subtitle)
	}
}

// --- idempotence and safety ------------------------------------------------

// Running a pass twice must change nothing the second time: the fingerprint
// gate is what keeps a scheduled job affordable.
func TestTicketBulkSecondPassWritesNothing(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)), BulkOptions{})
	r.run(t)
	r.slack.Reset()

	r.run(t)

	if kinds := r.slack.Kinds(); len(kinds) != 0 {
		t.Fatalf("second pass made calls: %v", kinds)
	}
}

func TestTicketBulkDryRunIsHarmless(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)), BulkOptions{})

	report, err := r.bulk.Run(context.Background(), target, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.DryRun || !sameStrings(report.Posted, []string{"NYX-1"}) {
		t.Fatalf("report = %+v", report)
	}
	if kinds := r.slack.Kinds(); len(kinds) != 0 {
		t.Fatalf("dry run made calls: %v", kinds)
	}
	if _, found, _ := r.store.Item(context.Background(), BulkItemPrefix+"NYX-1"); found {
		t.Fatal("dry run wrote to the ledger")
	}
}

// A ticket that is already claimed when it is first seen is never announced:
// there is nothing to offer and nobody to offer it to.
func TestTicketBulkIgnoresAnAlreadyClaimedTicket(t *testing.T) {
	claimed := ticketAged("NYX-1", time.Hour)
	claimed.Assignee = "Someone Else"
	r := newBulkRig(t, bulkJira(claimed), BulkOptions{})

	report := r.run(t)

	if len(report.Posted) != 0 || len(r.slack.Kinds()) != 0 {
		t.Fatalf("announced a claimed ticket: %+v %v", report, r.slack.Kinds())
	}
}

// --- configuration ---------------------------------------------------------

func TestTicketMaxItemsComesFromItsOwnEnvironment(t *testing.T) {
	t.Setenv(MaxItemsEnv, "3")
	if got := (BulkOptions{}).resolved().MaxItems; got != 3 {
		t.Fatalf("MaxItems = %d, want 3", got)
	}
	// The pull-request digest's variable must not reach this one.
	t.Setenv(MaxItemsEnv, "")
	t.Setenv("RIGGS_BULK_MAX_ITEMS", "7")
	if got := (BulkOptions{}).resolved().MaxItems; got != DefaultMaxItems {
		t.Fatalf("MaxItems = %d, want the ticket default %d", got, DefaultMaxItems)
	}
}

func TestTicketBulkDefaultsToAThreeHourCooldown(t *testing.T) {
	if got := (BulkOptions{}).resolved().Cooldown; got != 3*time.Hour {
		t.Fatalf("Cooldown = %s, want 3h", got)
	}
}

// The label was the bug. "Ask for AI Assistance" tagged a colleague and started
// nothing, and everyone who read it expected an agent.
func TestTicketBulkRowSeparatesTheExpertFromTheAgent(t *testing.T) {
	r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)),
		BulkOptions{Actions: RowActions{AskAssist: true, RunAssist: true}})
	r.run(t)

	row := bulkRows(t, r.slack.Posts()[0])[0]
	if got := bulkOptionsOf(t, row); !sameStrings(got, []string{IntentOpenBrowser, IntentAskAssist, IntentRunAssist}) {
		t.Fatalf("options = %v", got)
	}
	labels := bulkOptionLabels(t, row)
	if !strings.Contains(labels[1], "Ask for SME Assistance") {
		t.Fatalf("the human ask still claims to be an AI: %q", labels[1])
	}
	if !strings.Contains(labels[2], "Run AI Assistance") {
		t.Fatalf("run label = %q", labels[2])
	}
	for _, label := range labels {
		if strings.Contains(label, "Ask for AI") {
			t.Fatalf("the retired label survives: %q", label)
		}
	}
}

func TestTicketBulkRowOffersOnlyWhatIsConfigured(t *testing.T) {
	for name, tc := range map[string]struct {
		actions RowActions
		want    []string
	}{
		"nothing configured": {RowActions{}, []string{IntentOpenBrowser}},
		"only an expert":     {RowActions{AskAssist: true}, []string{IntentOpenBrowser, IntentAskAssist}},
		"only a harness":     {RowActions{RunAssist: true}, []string{IntentOpenBrowser, IntentRunAssist}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newBulkRig(t, bulkJira(ticketAged("NYX-1", time.Hour)),
				BulkOptions{Actions: tc.actions})
			r.run(t)
			got := bulkOptionsOf(t, bulkRows(t, r.slack.Posts()[0])[0])
			if !sameStrings(got, tc.want) {
				t.Fatalf("options = %v, want %v", got, tc.want)
			}
		})
	}
}

// bulkOptionLabels lists a row's menu labels, in order.
func bulkOptionLabels(t *testing.T, row map[string]any) []string {
	t.Helper()
	acc, ok := row["accessory"].(map[string]any)
	if !ok {
		return nil
	}
	raw, _ := acc["options"].([]any)
	out := make([]string, 0, len(raw))
	for _, o := range raw {
		text := o.(map[string]any)["text"].(map[string]any)
		out = append(out, text["text"].(string))
	}
	return out
}
