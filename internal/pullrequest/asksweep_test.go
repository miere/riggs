package pullrequest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// sweepRig is an engine with an ask card already posted, so the sweep has one
// to act on. The card is posted through the real Asker rather than written
// straight into the ledger: what the sweep reads has to be what an ask writes.
type sweepRig struct {
	engine *Engine
	gh     *fakeGH
	slack  *slacktest.Fake
	store  *notify.Store
}

// askChannel is where the review request lands. Deliberately not target's
// channel: an ask goes where the reviewer is.
const askChannel = "C-reviews"

func newSweepRig(t *testing.T, gh *fakeGH, refs ...string) *sweepRig {
	t.Helper()
	r := newRig(t, gh)
	notifier := notify.New(r.store, r.slack)
	for _, ref := range refs {
		asker := NewAsker(gh, r.store, notifier, r.slack, "U0B6HK02YBB", askChannel, "please review")
		if _, err := asker.Ask(context.Background(), ref, "U0B20G0ET9T", target); err != nil {
			t.Fatalf("Ask(%s): %v", ref, err)
		}
	}
	r.slack.Reset()
	return &sweepRig{engine: r.engine, gh: gh, slack: r.slack, store: r.store}
}

// sweep runs one pass and fails the test if it could not complete.
func (r *sweepRig) sweep(t *testing.T, dryRun bool) []AskOutcome {
	t.Helper()
	out, err := r.engine.SettleAsks(context.Background(), target, dryRun)
	if err != nil {
		t.Fatalf("SettleAsks: %v", err)
	}
	return out
}

// state reads back what the ledger thinks of a card.
func (r *sweepRig) state(t *testing.T, ref string) string {
	t.Helper()
	entry, found, err := r.store.Card(context.Background(), AskKey(ref))
	if err != nil || !found {
		t.Fatalf("Card(%s): found=%v err=%v", ref, found, err)
	}
	return entry.State
}

// mergedPR is an open pull request after it has been merged upstream.
func mergedPR(ref string) github.Detail {
	d := openPR(ref)
	d.Merged = true
	d.State = "closed"
	d.RequestedUsers = nil // GitHub drops us from the list once the review lands
	return d
}

// The bug, exactly as it was hit: the pull request was approved and merged on
// github.com rather than through Riggs' own button, so nothing ever settled the
// card and it sat open, still offering an Approve that could only fail.
func TestSweepCollapsesACardMergedOnGitHub(t *testing.T) {
	gh := greenGH(ref)
	r := newSweepRig(t, gh, ref)
	gh.details[ref] = mergedPR(ref)

	settled := r.sweep(t, false)

	if len(settled) != 1 || settled[0].Ref != ref || settled[0].State != ReasonMerged {
		t.Fatalf("settled = %+v, want one merged %s", settled, ref)
	}
	if settled[0].Error != "" {
		t.Fatalf("settle reported %q", settled[0].Error)
	}
	if got := r.state(t, ref); got != AskStateDone {
		t.Errorf("ledger state = %q, want %q", got, AskStateDone)
	}

	kinds := r.slack.Kinds()
	if len(kinds) != 1 || kinds[0] != "update" {
		t.Fatalf("slack calls = %v, want one in-place update", kinds)
	}
	raw, err := json.Marshal(r.slack.Calls[0].Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if blocks[0]["default_collapsed"] != true {
		t.Error("the card was not collapsed")
	}
	if strings.Contains(string(raw), `"value":"`+IntentApprove+`"`) {
		t.Error("Approve survived the sweep")
	}
}

// The label is derived, not the button's fixed wording: this path knows the
// pull request was merged rather than approved-and-merged, and says so.
func TestSweepLabelsTheCardWithWhatActuallyHappened(t *testing.T) {
	gh := greenGH(ref)
	r := newSweepRig(t, gh, ref)
	gh.details[ref] = mergedPR(ref)

	r.sweep(t, false)

	raw, err := json.Marshal(r.slack.Calls[0].Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The subtitle specifically: it is the only part of a collapsed container
	// that is visible without expanding it.
	subtitle, _ := blocks[0]["subtitle"].(map[string]any)
	text, _ := subtitle["text"].(string)
	if !strings.Contains(text, ref) || !strings.Contains(text, "Merged at") {
		t.Errorf("subtitle = %q, want the reference and a merged-at label", text)
	}
}

// The ask went to the reviewer's channel; the sweep has to update it there, not
// wherever the pass that ran it would otherwise write.
func TestSweepUpdatesTheCardInItsOwnChannel(t *testing.T) {
	gh := greenGH(ref)
	r := newSweepRig(t, gh, ref)
	gh.details[ref] = mergedPR(ref)

	r.sweep(t, false)

	if got := r.slack.Calls[0].Target.Channel; got != askChannel {
		t.Errorf("updated channel = %q, want the card's own %q", got, askChannel)
	}
}

// An approval that has not been merged yet still settles, because that is what
// the Approve button does and the two must not disagree.
func TestSweepCollapsesAnApprovedButUnmergedCard(t *testing.T) {
	gh := greenGH(ref)
	gh.decisions = map[string]string{ref: github.DecisionApproved}
	r := newSweepRig(t, gh, ref)

	settled := r.sweep(t, false)

	if len(settled) != 1 || settled[0].State != ReasonApproved {
		t.Fatalf("settled = %+v, want one approved %s", settled, ref)
	}
	if got := r.state(t, ref); got != AskStateDone {
		t.Errorf("ledger state = %q, want %q", got, AskStateDone)
	}
}

// A review still worth doing is left entirely alone — no update, no state
// change, and nothing reported.
func TestSweepLeavesAnOpenReviewAlone(t *testing.T) {
	r := newSweepRig(t, greenGH(ref), ref)

	settled := r.sweep(t, false)

	if len(settled) != 0 {
		t.Errorf("settled = %+v, want nothing", settled)
	}
	if got := r.state(t, ref); got != AskStateOpen {
		t.Errorf("ledger state = %q, want %q", got, AskStateOpen)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
}

// Changes requested is a moment in a review, not the end of one: the card stays
// open, because the build can go green and the request can come back around.
func TestSweepLeavesChangesRequestedOpen(t *testing.T) {
	gh := greenGH(ref)
	gh.decisions = map[string]string{ref: github.DecisionChangesRequested}
	r := newSweepRig(t, gh, ref)

	if settled := r.sweep(t, false); len(settled) != 0 {
		t.Errorf("settled = %+v, want nothing", settled)
	}
	if got := r.state(t, ref); got != AskStateOpen {
		t.Errorf("ledger state = %q, want %q", got, AskStateOpen)
	}
}

// Once settled a card is never looked at again — not re-drawn, and not even
// re-read from GitHub. That is what the state on the ledger entry buys.
func TestSweepNeverReReadsASettledCard(t *testing.T) {
	gh := greenGH(ref)
	r := newSweepRig(t, gh, ref)
	gh.details[ref] = mergedPR(ref)

	r.sweep(t, false)
	r.slack.Reset()
	gh.calls = nil

	if settled := r.sweep(t, false); len(settled) != 0 {
		t.Errorf("second sweep settled %+v, want nothing left to do", settled)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
	if len(gh.calls) != 0 {
		t.Errorf("github calls = %v, want none", gh.calls)
	}
}

// A preview with side effects is not a preview: the dry run says what it would
// collapse and writes nothing, in Slack or in the ledger.
func TestSweepDryRunChangesNothing(t *testing.T) {
	gh := greenGH(ref)
	r := newSweepRig(t, gh, ref)
	gh.details[ref] = mergedPR(ref)

	settled := r.sweep(t, true)

	if len(settled) != 1 || settled[0].Ref != ref {
		t.Fatalf("settled = %+v, want the merged card reported", settled)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
	if got := r.state(t, ref); got != AskStateOpen {
		t.Errorf("ledger state = %q, want the card left %q", got, AskStateOpen)
	}
}

// One unreachable pull request must not leave every other card open.
func TestSweepCarriesOnPastAFailedRead(t *testing.T) {
	const other = "o/r#2"
	gh := greenGH(ref)
	gh.search = append(gh.search, github.PullRequest{Repo: "o/r", Number: 2})
	gh.details[other] = openPR(other)
	gh.checks["sha-"+other] = gh.checks["sha-"+ref]

	r := newSweepRig(t, gh, ref, other)
	gh.details[other] = mergedPR(other)
	delete(gh.details, ref) // the read for this one now fails

	settled := r.sweep(t, false)

	byRef := map[string]AskOutcome{}
	for _, o := range settled {
		byRef[o.Ref] = o
	}
	if got, ok := byRef[ref]; !ok || got.Error == "" {
		t.Errorf("outcome for the failed read = %+v, want an error reported", got)
	}
	if got, ok := byRef[other]; !ok || got.Error != "" || got.State != ReasonMerged {
		t.Errorf("outcome for the reachable card = %+v, want it settled anyway", got)
	}
	if got := r.state(t, other); got != AskStateDone {
		t.Errorf("reachable card state = %q, want %q", got, AskStateDone)
	}
	if got := r.state(t, ref); got != AskStateOpen {
		t.Errorf("unreadable card state = %q, want it left %q for the next pass", got, AskStateOpen)
	}
}

// A pull request nobody was ever asked to review has no card, and the sweep has
// nothing to enumerate. It must not invent one.
func TestSweepWithNoAskCardsIsSilent(t *testing.T) {
	r := newRig(t, greenGH(ref))

	settled, err := r.engine.SettleAsks(context.Background(), target, false)
	if err != nil {
		t.Fatalf("SettleAsks: %v", err)
	}
	if len(settled) != 0 {
		t.Errorf("settled = %+v, want nothing", settled)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
}

// A ledger that cannot be read is the sweep's own failure, not one card's.
func TestSweepReportsALedgerFailure(t *testing.T) {
	r := newSweepRig(t, greenGH(ref), ref)
	r.store.Close()

	if _, err := r.engine.SettleAsks(context.Background(), target, false); err == nil {
		t.Fatal("SettleAsks succeeded over a closed ledger")
	}
}

// The digest pass is the one on a schedule, so it is the one that has to sweep.
func TestDigestPassSettlesAskCards(t *testing.T) {
	gh := bulkGH(bulkPR(ref, 0, "alex"))
	br := newBulkRig(t, gh, BulkOptions{})

	asker := NewAsker(gh, br.store, br.notifier, br.slack, "U0B6HK02YBB", askChannel, "please review")
	if _, err := asker.Ask(context.Background(), ref, "U0B20G0ET9T", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	gh.details[ref] = mergedPR(ref)
	br.slack.Reset()

	report, err := br.bulk.Run(context.Background(), target, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.SettledAsks) != 1 || report.SettledAsks[0].Ref != ref {
		t.Fatalf("SettledAsks = %+v, want the merged card", report.SettledAsks)
	}
	if !strings.Contains(report.String(), ref) {
		t.Errorf("report = %q, want the settled card named", report.String())
	}
}

// Guard: the sweep must not swallow a Slack failure into a settled card. A
// card the ledger calls done but Slack still renders open is the worst of both.
func TestSweepReportsAFailedUpdate(t *testing.T) {
	gh := greenGH(ref)
	r := newSweepRig(t, gh, ref)
	gh.details[ref] = mergedPR(ref)
	r.slack.UpdateErr = errors.New("slack said no")

	settled := r.sweep(t, false)

	if len(settled) != 1 || settled[0].Error == "" {
		t.Fatalf("settled = %+v, want the failure reported", settled)
	}
	if got := r.state(t, ref); got != AskStateOpen {
		t.Errorf("ledger state = %q, want the card left %q to retry", got, AskStateOpen)
	}
}
