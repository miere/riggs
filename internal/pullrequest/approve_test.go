package pullrequest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// fakeWriter scripts the mutating GitHub seam and records what was called.
type fakeWriter struct {
	login    string
	loginErr error
	// reviews is returned on every read; reviewsAfter replaces it once an
	// approval has been submitted, modelling GitHub catching up.
	reviews      []github.Review
	reviewsAfter []github.Review
	reviewsErr   error
	approveErr   error
	mergeErr     error

	approved  int
	merged    int
	submitted bool
	// lagReads is how many reads after approving still show the old state,
	// modelling GitHub's replication lag.
	lagReads int
	reads    int
}

func (f *fakeWriter) AuthenticatedLogin(context.Context) (string, error) {
	return f.login, f.loginErr
}

func (f *fakeWriter) Reviews(context.Context, string, int) ([]github.Review, error) {
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	if !f.submitted {
		return f.reviews, nil
	}
	f.reads++
	if f.reads <= f.lagReads {
		return f.reviews, nil
	}
	return f.reviewsAfter, nil
}

func (f *fakeWriter) Approve(context.Context, string, int, string) error {
	if f.approveErr != nil {
		return f.approveErr
	}
	f.approved++
	f.submitted = true
	return nil
}

func (f *fakeWriter) Merge(context.Context, string, int) error {
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.merged++
	return nil
}

func approved(login string) []github.Review {
	return []github.Review{{Author: login, State: "APPROVED", SubmittedAt: time.Now()}}
}

// approverRig assembles an approver over a temp ledger and a fake Slack.
type approverRig struct {
	*Approver
	gh    *fakeWriter
	slack *slacktest.Fake
	store *notify.Store
}

func newApproverRig(t *testing.T, gh *fakeWriter) *approverRig {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	fake := slacktest.New()
	a := NewApprover(gh, store, fake).WithSleep(func(time.Duration) {})
	return &approverRig{Approver: a, gh: gh, slack: fake, store: store}
}

var approveTarget = slack.Target{Profile: "default", BotToken: "xoxb", Channel: "C1"}

func TestApprovesAndVerifies(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviewsAfter: approved("miere")}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.approved != 1 || !res.Approved || res.AlreadyApproved {
		t.Errorf("result = %+v, approvals = %d; want one fresh approval", res, gh.approved)
	}
	if gh.merged != 0 {
		t.Error("plain approve merged the pull request")
	}
	if !strings.Contains(res.Message, "✓ Approved o/r#1") {
		t.Errorf("message = %q", res.Message)
	}
	// The acknowledgement goes out before the work, the outcome after it.
	if len(r.slack.Calls) != 2 {
		t.Fatalf("slack calls = %d, want an ack and an outcome", len(r.slack.Calls))
	}
	if !strings.Contains(r.slack.Calls[0].Msg.Text, "verifying with GitHub") {
		t.Errorf("first message = %q, want the acknowledgement", r.slack.Calls[0].Msg.Text)
	}
	if r.slack.Calls[1].Msg.Text != res.Message {
		t.Errorf("last message = %q, want the real outcome", r.slack.Calls[1].Msg.Text)
	}
}

// A second click must not re-approve.
func TestStandingApprovalIsNotResubmitted(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviews: approved("miere")}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.approved != 0 {
		t.Errorf("approvals = %d, want none — ours was already standing", gh.approved)
	}
	if !res.AlreadyApproved || !res.Approved {
		t.Errorf("result = %+v, want already-approved", res)
	}
	if !strings.Contains(res.Message, "already approved") {
		t.Errorf("message = %q, want it to say nothing was needed", res.Message)
	}
}

// New commits dismiss a prior approval; that is not a standing one, so it must
// be re-approved.
func TestDismissedApprovalIsReapproved(t *testing.T) {
	gh := &fakeWriter{
		login:        "miere",
		reviews:      []github.Review{{Author: "miere", State: "DISMISSED"}},
		reviewsAfter: approved("miere"),
	}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.approved != 1 || res.AlreadyApproved {
		t.Errorf("result = %+v, approvals = %d; want a re-approval", res, gh.approved)
	}
}

// Someone else's approval is not ours.
func TestAnotherReviewersApprovalDoesNotCount(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviews: approved("hjed"), reviewsAfter: approved("miere")}
	r := newApproverRig(t, gh)

	if _, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.approved != 1 {
		t.Errorf("approvals = %d, want ours submitted despite hjed's", gh.approved)
	}
}

// GitHub lags between accepting a review and reporting it. That lag is the
// original "flaky approval", so the verification retries.
func TestVerificationRetriesThroughLag(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviewsAfter: approved("miere"), lagReads: 2}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Approved {
		t.Errorf("result = %+v, want the approval confirmed after the lag", res)
	}
}

// An approval that never registers must say so, not claim success.
func TestUnverifiedApprovalIsReportedHonestly(t *testing.T) {
	gh := &fakeWriter{login: "miere"} // reviewsAfter stays empty: it never lands
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", true, approveTarget, "1700.1")
	if err == nil {
		t.Fatal("Run = nil error when the approval never registered")
	}
	if res.Approved {
		t.Errorf("result = %+v, want it not claiming approval", res)
	}
	if gh.merged != 0 {
		t.Error("merged despite an unverified approval")
	}
	if !strings.Contains(res.Message, "has not recorded it yet") {
		t.Errorf("message = %q, want an honest warning", res.Message)
	}
}

func TestApprovalFailureStopsBeforeMerge(t *testing.T) {
	gh := &fakeWriter{login: "miere", approveErr: errors.New("403 Resource not accessible")}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", true, approveTarget, "1700.1")
	if err == nil {
		t.Fatal("Run = nil error on a failed approval")
	}
	if gh.merged != 0 {
		t.Error("merged after the approval failed")
	}
	if !strings.Contains(res.Message, "Could not approve") ||
		!strings.Contains(res.Message, "Resource not accessible") {
		t.Errorf("message = %q, want GitHub's own explanation", res.Message)
	}
}

func TestApproveAndMerge(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviewsAfter: approved("miere")}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", true, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.approved != 1 || gh.merged != 1 || !res.Merged {
		t.Errorf("result = %+v, approvals=%d merges=%d", res, gh.approved, gh.merged)
	}
	if !strings.Contains(res.Message, "rebase-merged") {
		t.Errorf("message = %q, want it to name the rebase merge", res.Message)
	}
}

// A merge failure after a successful approval is its own outcome: the approval
// stands, and the message must not imply otherwise.
func TestMergeFailureAfterApprovalIsDistinct(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviewsAfter: approved("miere"),
		mergeErr: errors.New("Base branch was modified")}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", true, approveTarget, "1700.1")
	if err == nil {
		t.Fatal("Run = nil error on a failed merge")
	}
	if !res.Approved || res.Merged {
		t.Errorf("result = %+v, want approved but not merged", res)
	}
	if !strings.Contains(res.Message, "Approved") || !strings.Contains(res.Message, "merge failed") {
		t.Errorf("message = %q, want both facts", res.Message)
	}
}

// Without a login we cannot tell whose approval is standing. Submitting a
// redundant approval is a no-op; skipping a needed one is not.
func TestUnknownLoginStillApproves(t *testing.T) {
	gh := &fakeWriter{loginErr: errors.New("no token"), reviewsAfter: approved("anyone")}
	r := newApproverRig(t, gh)

	res, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gh.approved != 1 || !res.Approved {
		t.Errorf("result = %+v, approvals = %d; want it to approve anyway", res, gh.approved)
	}
}

// With no explicit thread, the outcome lands on the tracked card.
func TestFallsBackToTheTrackedCardsThread(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviewsAfter: approved("miere")}
	r := newApproverRig(t, gh)
	ctx := context.Background()

	if err := r.store.SaveCard(ctx, Key("o/r#1"), notify.Entry{
		Profile: "default", Channel: "C-CARD", TS: "1699.9", State: "reviewable",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Run(ctx, "o/r#1", false, approveTarget, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := r.slack.Calls[0]
	if call.Msg.ThreadTS != "1699.9" || call.Target.Channel != "C-CARD" {
		t.Errorf("replied to %s in %s, want the tracked card's thread",
			call.Msg.ThreadTS, call.Target.Channel)
	}
}

// A Slack outage must not turn a successful approval into a failure.
func TestSlackFailureDoesNotFailTheApproval(t *testing.T) {
	gh := &fakeWriter{login: "miere", reviewsAfter: approved("miere")}
	r := newApproverRig(t, gh)
	r.slack.PostErr = errors.New("invalid_auth")

	res, err := r.Run(context.Background(), "o/r#1", false, approveTarget, "1700.1")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Approved || gh.approved != 1 {
		t.Errorf("result = %+v, want the approval to stand regardless of Slack", res)
	}
}

func TestMalformedRefIsRejected(t *testing.T) {
	gh := &fakeWriter{login: "miere"}
	r := newApproverRig(t, gh)

	if _, err := r.Run(context.Background(), "not-a-ref", false, approveTarget, ""); err == nil {
		t.Fatal("Run = nil error on a malformed ref")
	}
	if gh.approved != 0 {
		t.Error("approved something despite a malformed ref")
	}
}
