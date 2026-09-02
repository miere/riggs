package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// fakeGH is a scripted GitHub, recording which endpoints were reached so the
// call-avoidance behaviour can be asserted directly.
type fakeGH struct {
	search   []github.PullRequest
	details  map[string]github.Detail
	reviews  map[string][]github.Review
	checks   map[string][]github.Check
	closedBy map[string]string
	// decisions is GitHub's own reviewDecision per ref; absent means none.
	decisions map[string]string
	calls     []string
	failOn    string
}

func (f *fakeGH) record(what string) { f.calls = append(f.calls, what) }

func (f *fakeGH) ReviewRequested(context.Context, string, int) ([]github.PullRequest, error) {
	f.record("search")
	if f.failOn == "search" {
		return nil, errors.New("search exploded")
	}
	return f.search, nil
}

func (f *fakeGH) PullRequestDetail(_ context.Context, repo string, n int) (github.Detail, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("detail:" + ref)
	d, ok := f.details[ref]
	if !ok {
		return github.Detail{}, fmt.Errorf("no such PR %s", ref)
	}
	return d, nil
}

func (f *fakeGH) ReviewDecision(_ context.Context, repo string, n int) (string, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("decision:" + ref)
	return f.decisions[ref], nil
}

func (f *fakeGH) Reviews(_ context.Context, repo string, n int) ([]github.Review, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("reviews:" + ref)
	return f.reviews[ref], nil
}

func (f *fakeGH) Checks(_ context.Context, repo, sha string) ([]github.Check, error) {
	f.record("checks:" + sha)
	return f.checks[sha], nil
}

func (f *fakeGH) ClosedBy(_ context.Context, repo string, n int) (string, error) {
	ref := fmt.Sprintf("%s#%d", repo, n)
	f.record("closedby:" + ref)
	return f.closedBy[ref], nil
}

func (f *fakeGH) called(what string) bool {
	for _, c := range f.calls {
		if c == what {
			return true
		}
	}
	return false
}

var target = slack.Target{Profile: "default", BotToken: "xoxb", Channel: "C1"}

// rig assembles an engine over a temp ledger and a fake Slack.
type rig struct {
	engine *Engine
	gh     *fakeGH
	slack  *slacktest.Fake
	store  *notify.Store
}

func newRig(t *testing.T, gh *fakeGH) *rig {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	fake := slacktest.New()
	return &rig{
		engine: NewEngine(gh, store, notify.New(store, fake), "miere", "U1"),
		gh:     gh, slack: fake, store: store,
	}
}

// openPR builds a green, review-requested pull request.
func openPR(ref string) github.Detail {
	repo, n, _ := SplitRef(ref)
	return github.Detail{
		Repo: repo, Number: n, Title: "Fix the thing", Body: "body",
		URL:    "https://github.com/" + repo + "/pull/" + fmt.Sprint(n),
		Author: "alex", State: "open", HeadSHA: "sha-" + ref,
		RequestedUsers: []string{"miere"},
	}
}

func greenGH(ref string) *fakeGH {
	return &fakeGH{
		search:  []github.PullRequest{{Repo: strings.Split(ref, "#")[0], Number: 1}},
		details: map[string]github.Detail{ref: openPR(ref)},
		checks:  map[string][]github.Check{"sha-" + ref: {run("build", "COMPLETED", "SUCCESS")}},
	}
}

const ref = "o/r#1"

func TestPostsAndTagsANewReviewablePR(t *testing.T) {
	r := newRig(t, greenGH(ref))

	report, err := r.engine.Run(context.Background(), target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want one", report.Outcomes)
	}
	o := report.Outcomes[0]
	if o.Action != string(notify.Posted) || o.State != "reviewable" || !o.Tagged {
		t.Errorf("outcome = %+v, want a posted, tagged, reviewable card", o)
	}
	// The card, then the tag in its thread.
	if got := r.slack.Kinds(); len(got) != 2 || got[0] != "post" || got[1] != "post" {
		t.Errorf("slack calls = %v, want the card and the tag", got)
	}
	if body := r.slack.Calls[1].Msg.Text; !strings.Contains(body, "<@U1>") ||
		!strings.Contains(body, "ready for your review") {
		t.Errorf("tag text = %q, want a first-review ping", body)
	}
}

// The point of the whole design: a tick with nothing changed touches nothing.
func TestUnchangedTickIsSilent(t *testing.T) {
	r := newRig(t, greenGH(ref))
	ctx := context.Background()

	if _, err := r.engine.Run(ctx, target, false); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	r.slack.Reset()

	for i := 0; i < 3; i++ {
		report, err := r.engine.Run(ctx, target, false)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if report.Outcomes[0].Action != string(notify.Unchanged) {
			t.Fatalf("tick %d action = %s, want unchanged", i, report.Outcomes[0].Action)
		}
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
	// The body is derived from the description now. Two ticks producing no
	// Slack calls (asserted above) is only possible because it is stable, which
	// is what the cache used to buy.
}

// When the review lands GitHub drops us from the request list, the card
// collapses, and the latch reopens so a re-request pings again.
func TestLeavingReviewableClearsTheLatchAndReEntryRetags(t *testing.T) {
	gh := greenGH(ref)
	r := newRig(t, gh)
	ctx := context.Background()

	if _, err := r.engine.Run(ctx, target, false); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Reviewed: no longer a requested reviewer.
	d := gh.details[ref]
	d.RequestedUsers = nil
	gh.details[ref] = d
	r.slack.Reset()

	report, err := r.engine.Run(ctx, target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcomes[0].State != ReasonApproved {
		t.Errorf("state = %s, want it collapsed to approved", report.Outcomes[0].State)
	}
	if _, fired, _ := r.store.LatchFiredAt(ctx, Key(ref), tagLatch); fired {
		t.Fatal("the tag latch survived leaving the reviewable state")
	}

	// Re-requested.
	d.RequestedUsers = []string{"miere"}
	gh.details[ref] = d
	r.slack.Reset()

	report, err = r.engine.Run(ctx, target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Outcomes[0].Tagged {
		t.Error("re-entering the reviewable state did not re-tag the reviewer")
	}
	if body := r.slack.Calls[len(r.slack.Calls)-1].Msg.Text; !strings.Contains(body, "re-review") {
		t.Errorf("tag text = %q, want the re-review wording", body)
	}
}

// Nothing is posted while a PR builds, and nothing at all if it dies before
// ever going green.
func TestDeadOnArrivalIsSilent(t *testing.T) {
	gh := greenGH(ref)
	gh.checks["sha-"+ref] = []github.Check{run("build", "IN_PROGRESS", "")}
	r := newRig(t, gh)

	report, err := r.engine.Run(context.Background(), target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Outcomes) != 0 {
		t.Errorf("outcomes = %+v, want none for a never-shown, not-reviewable PR", report.Outcomes)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
}

func TestDraftsAreIgnored(t *testing.T) {
	gh := greenGH(ref)
	d := gh.details[ref]
	d.Draft = true
	gh.details[ref] = d
	r := newRig(t, gh)

	report, _ := r.engine.Run(context.Background(), target, false)
	if len(report.Outcomes) != 0 {
		t.Errorf("outcomes = %+v, want a draft ignored", report.Outcomes)
	}
}

// A team-only request we never adopted stays out: mirroring those would flood
// the channel with other people's work.
func TestTeamOnlyRequestIsIgnored(t *testing.T) {
	gh := greenGH(ref)
	d := gh.details[ref]
	d.RequestedUsers = nil
	d.RequestedTeams = []string{"cloud-services"}
	gh.details[ref] = d
	r := newRig(t, gh)

	report, _ := r.engine.Run(context.Background(), target, false)
	if len(report.Outcomes) != 0 {
		t.Errorf("outcomes = %+v, want a team-only request ignored", report.Outcomes)
	}
}

// A merged PR's reason outranks anything reviews or checks could say, so those
// calls are skipped — most of the saving on a mostly-finished queue.
func TestTerminalPRSkipsReviewAndCheckReads(t *testing.T) {
	gh := greenGH(ref)
	merged := time.Date(2026, 5, 14, 5, 42, 0, 0, time.UTC)
	d := gh.details[ref]
	d.State, d.Merged, d.MergedAt = "closed", true, &merged
	gh.details[ref] = d
	r := newRig(t, gh)
	ctx := context.Background()

	// Track it first, so it is not skipped as dead-on-arrival.
	if err := r.store.SaveCard(ctx, Key(ref), notify.Entry{
		Profile: "default", Channel: "C1", TS: "1700.1", State: "reviewable",
	}); err != nil {
		t.Fatal(err)
	}
	gh.calls = nil

	report, err := r.engine.Run(ctx, target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcomes[0].State != ReasonMerged {
		t.Errorf("state = %s, want merged", report.Outcomes[0].State)
	}
	if gh.called("reviews:"+ref) || gh.called("checks:sha-"+ref) {
		t.Errorf("calls = %v, want reviews and checks skipped for a merged PR", gh.calls)
	}
}

// Once terminal, a card is never re-fetched: nothing about it can change.
func TestTerminalCardsLeaveScope(t *testing.T) {
	gh := &fakeGH{} // no search results
	r := newRig(t, gh)
	ctx := context.Background()

	if err := r.store.SaveCard(ctx, Key(ref), notify.Entry{
		Profile: "default", Channel: "C1", TS: "1700.1", State: ReasonMerged,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := r.engine.Run(ctx, target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Considered != 0 {
		t.Errorf("considered %d, want a terminal card out of scope", report.Considered)
	}
	if gh.called("detail:" + ref) {
		t.Errorf("calls = %v, want no fetch for a terminal card", gh.calls)
	}
}

// A tracked PR stays in scope after we drop off the request list, so its card
// can reach its final state.
func TestTrackedNonTerminalStaysInScope(t *testing.T) {
	gh := greenGH(ref)
	gh.search = nil // no longer a requested reviewer
	d := gh.details[ref]
	d.RequestedUsers = nil
	gh.details[ref] = d
	r := newRig(t, gh)
	ctx := context.Background()

	if err := r.store.SaveCard(ctx, Key(ref), notify.Entry{
		Profile: "default", Channel: "C1", TS: "1700.1", State: "reviewable",
	}); err != nil {
		t.Fatal(err)
	}
	report, err := r.engine.Run(ctx, target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Considered != 1 || len(report.Outcomes) != 1 {
		t.Fatalf("report = %+v, want the tracked card finalised", report)
	}
	if report.Outcomes[0].State != ReasonApproved {
		t.Errorf("state = %s, want approved", report.Outcomes[0].State)
	}
}

// A preview with side effects is not a preview.
func TestDryRunSendsNothingAndWritesNothing(t *testing.T) {
	r := newRig(t, greenGH(ref))
	ctx := context.Background()

	report, err := r.engine.Run(ctx, target, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.DryRun || report.Outcomes[0].Action != string(notify.Posted) {
		t.Errorf("report = %+v, want a dry-run plan to post", report)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none in a dry run", r.slack.Kinds())
	}
	if _, found, _ := r.store.Card(ctx, Key(ref)); found {
		t.Error("the dry run wrote a card to the ledger")
	}
	if _, found, _ := r.store.Summary(ctx, Key(ref)); found {
		t.Error("the dry run cached a summary")
	}
}

// One failing PR must not fail the tick: the others still reconcile.
func TestOnePRFailureIsIsolated(t *testing.T) {
	gh := greenGH(ref)
	gh.search = append(gh.search, github.PullRequest{Repo: "o/r", Number: 99})
	r := newRig(t, gh)

	report, err := r.engine.Run(context.Background(), target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var ok, failed int
	for _, o := range report.Outcomes {
		if o.Error != "" {
			failed++
		} else {
			ok++
		}
	}
	if ok != 1 || failed != 1 {
		t.Errorf("outcomes = %+v, want one success and one isolated failure", report.Outcomes)
	}
}

// Discovery failing is fatal — a tick that cannot see the queue must not go on
// to conclude the queue is empty.
func TestDiscoveryFailureIsFatal(t *testing.T) {
	gh := greenGH(ref)
	gh.failOn = "search"
	r := newRig(t, gh)

	if _, err := r.engine.Run(context.Background(), target, false); err == nil {
		t.Fatal("Run = nil error when discovery failed")
	}
}

// Archiving a repository makes it read-only. A card already on the board then
// has to collapse, because the queue's discovery query no longer returns it
// and it can never merge. Left alone such a card sits there reading
// "reviewable" and answers every approval with `422 lock prevents review`.
func TestArchivingCollapsesATrackedCard(t *testing.T) {
	gh := greenGH(ref)
	r := newRig(t, gh)
	ctx := context.Background()

	if _, err := r.engine.Run(ctx, target, false); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// The repository is archived, and so drops out of discovery.
	d := gh.details[ref]
	d.Archived = true
	gh.details[ref] = d
	gh.search = nil
	gh.calls = nil
	r.slack.Reset()

	report, err := r.engine.Run(ctx, target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want the tracked card still reconciled once", report.Outcomes)
	}
	if report.Outcomes[0].State != ReasonArchived {
		t.Errorf("state = %s, want it collapsed to archived", report.Outcomes[0].State)
	}
	// One read and no more: checks and the decision cannot change the answer.
	if gh.called("checks:sha-"+ref) || gh.called("decision:"+ref) {
		t.Errorf("calls = %v, want the detail read to settle it alone", gh.calls)
	}

	// Archived is terminal, so the next tick stops reading the PR entirely.
	gh.calls = nil
	if _, err := r.engine.Run(ctx, target, false); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if gh.called("detail:" + ref) {
		t.Errorf("calls = %v, want a terminal card never re-fetched", gh.calls)
	}
}
