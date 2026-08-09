package pullrequest

import (
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/github"
)

func run(name, status, conclusion string) github.Check {
	return github.Check{Name: name, Status: status, Conclusion: conclusion}
}

func legacy(name, state string) github.Check {
	return github.Check{Name: name, Conclusion: state, Legacy: true}
}

func TestClassifyChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []github.Check
		want   CheckStatus
	}{
		{"no checks at all", nil, CheckStatus{}},
		{"all green", []github.Check{run("build", "COMPLETED", "SUCCESS")},
			CheckStatus{Passed: true, Total: 1}},
		{"neutral and skipped still pass", []github.Check{
			run("a", "COMPLETED", "NEUTRAL"), run("b", "COMPLETED", "SKIPPED")},
			CheckStatus{Passed: true, Total: 2}},
		{"one queued means running", []github.Check{
			run("a", "COMPLETED", "SUCCESS"), run("b", "QUEUED", "")},
			CheckStatus{Running: true, Total: 2}},
		{"failure wins over running", []github.Check{
			run("a", "IN_PROGRESS", ""), run("b", "COMPLETED", "FAILURE")},
			CheckStatus{Running: true, Failed: true, Total: 2}},
		{"cancelled counts as failed", []github.Check{run("a", "COMPLETED", "CANCELLED")},
			CheckStatus{Failed: true, Total: 1}},
		// A required status context that has not reported yet is the reason a
		// PR must not be called green prematurely.
		{"pending legacy status blocks green", []github.Check{
			run("a", "COMPLETED", "SUCCESS"), legacy("ci/required", "PENDING")},
			CheckStatus{Running: true, Total: 2}},
		{"legacy error is a failure", []github.Check{legacy("ci", "ERROR")},
			CheckStatus{Failed: true, Total: 1}},
		{"legacy success passes", []github.Check{legacy("ci", "SUCCESS")},
			CheckStatus{Passed: true, Total: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyChecks(tc.checks); got != tc.want {
				t.Errorf("ClassifyChecks = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func review(author, state string, at time.Time) github.Review {
	return github.Review{Author: author, State: state, SubmittedAt: at}
}

// The verdict comes from GitHub's decision; the reviews only name who asked
// for changes. Deriving approval from the review list was wrong twice — see
// github.ReviewDecision.
func TestDeriveVerdict(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		decision string
		reviews  []github.Review
		want     Verdict
	}{
		{"no decision", "", nil, Verdict{}},
		{"approved", github.DecisionApproved, nil, Verdict{Approved: true}},
		{"review required is not approval", github.DecisionReviewRequired, nil, Verdict{}},
		{"changes requested names the reviewer", github.DecisionChangesRequested,
			[]github.Review{review("a", "APPROVED", base), review("b", "CHANGES_REQUESTED", base)},
			Verdict{ChangesRequested: true, ChangesRequestedBy: "b"}},
		// A standing approval from someone else does NOT make it approved:
		// live counter-example nct-intelligence-beholder#1315.
		{"another approval without a decision", "",
			[]github.Review{review("a", "APPROVED", base)}, Verdict{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveVerdict(tc.decision, tc.reviews); got != tc.want {
				t.Errorf("DeriveVerdict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// GitHub keeps one standing review per author; a later one supersedes an
// earlier one, and a comment is not a verdict.
func TestLatestByAuthorIsWhatVerdictsAreBuiltFrom(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	reviews := github.LatestByAuthor([]github.Review{
		review("hjed", "CHANGES_REQUESTED", base),
		review("hjed", "APPROVED", base.Add(time.Hour)),
		review("other", "COMMENTED", base.Add(2*time.Hour)),
	})
	if len(reviews) != 1 || reviews[0].Author != "hjed" || reviews[0].State != "APPROVED" {
		t.Errorf("latest = %+v, want only hjed's superseding approval", reviews)
	}
}

func detail(state string, merged bool, requested ...string) github.Detail {
	return github.Detail{
		Repo: "o/r", Number: 1, State: state, Merged: merged,
		RequestedUsers: requested,
	}
}

// The precedence table, which is the heart of the port.
func TestDerive(t *testing.T) {
	green := CheckStatus{Passed: true, Total: 1}
	running := CheckStatus{Running: true, Total: 1}
	failed := CheckStatus{Failed: true, Total: 1}

	for _, tc := range []struct {
		name      string
		detail    github.Detail
		checks    CheckStatus
		verdict   Verdict
		requested bool
		want      State
	}{
		{"merged outranks everything", detail("closed", true), failed,
			Verdict{ChangesRequested: true}, true, State{Reason: ReasonMerged}},
		{"closed outranks a verdict", detail("closed", false), green,
			Verdict{ChangesRequested: true}, true, State{Reason: ReasonClosed}},
		{"changes requested outranks failing checks", detail("open", false), failed,
			Verdict{ChangesRequested: true, ChangesRequestedBy: "hjed"}, true,
			State{Reason: ReasonChangesRequested, ChangesRequestedBy: "hjed"}},
		{"failing checks", detail("open", false), failed, Verdict{}, true,
			State{Reason: ReasonChecksFailed}},
		{"reviewable", detail("open", false, "miere"), green, Verdict{}, true,
			State{Reviewable: true}},
		// GitHub's own APPROVED decision does dequeue us (gcp-jsm-bridge#80);
		// another reviewer approving with no decision does not
		// (nct-intelligence-beholder#1315, covered in TestDeriveVerdict).
		{"github's approved decision dequeues us", detail("open", false, "miere"), green,
			Verdict{Approved: true}, true, State{Reason: ReasonApproved}},
		{"not requested and green collapses to approved", detail("open", false), green,
			Verdict{}, false, State{Reason: ReasonApproved}},
		{"checks running", detail("open", false), running, Verdict{}, true,
			State{Reason: ReasonChecksRunning}},
		// Nothing has reported yet: that is "running", not "green".
		{"no checks at all is running, not green", detail("open", false), CheckStatus{},
			Verdict{}, true, State{Reason: ReasonChecksRunning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Derive(tc.detail, tc.checks, tc.verdict, tc.requested)
			if got != tc.want {
				t.Errorf("Derive = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestTerminalStates(t *testing.T) {
	for reason, want := range map[string]bool{
		ReasonMerged: true, ReasonClosed: true,
		ReasonApproved: false, ReasonChecksRunning: false, ReasonChangesRequested: false,
	} {
		if got := (State{Reason: reason}).Terminal(); got != want {
			t.Errorf("%s terminal = %v, want %v", reason, got, want)
		}
	}
	if (State{Reviewable: true}).Terminal() {
		t.Error("a reviewable state must never be terminal")
	}
}

func TestToken(t *testing.T) {
	if got := (State{Reviewable: true}).Token(); got != "reviewable" {
		t.Errorf("Token = %q, want reviewable", got)
	}
	if got := (State{Reason: ReasonMerged}).Token(); got != "merged" {
		t.Errorf("Token = %q, want merged", got)
	}
}

// Timestamps are shown in the reviewer's timezone. A card claiming 05:42 for
// something that happened at 15:42 is worse than no timestamp.
func TestFormatTimeIsLocal(t *testing.T) {
	utc := time.Date(2026, 5, 14, 5, 42, 0, 0, time.UTC)
	got := FormatTime(&utc)
	if got != "May 14, 2026 at 3:42 PM" {
		t.Errorf("FormatTime = %q, want the Sydney wall-clock rendering", got)
	}
	if FormatTime(nil) != "unknown time" {
		t.Errorf("FormatTime(nil) = %q", FormatTime(nil))
	}
}

func TestLabels(t *testing.T) {
	merged := time.Date(2026, 5, 14, 5, 42, 0, 0, time.UTC)
	closed := merged
	d := github.Detail{MergedAt: &merged, ClosedAt: &closed}

	for _, tc := range []struct {
		name     string
		state    State
		closedBy string
		want     string
	}{
		{"merged", State{Reason: ReasonMerged}, "", "Merged at: May 14, 2026 at 3:42 PM"},
		{"closed by someone", State{Reason: ReasonClosed}, "hjed",
			"Closed by @hjed at May 14, 2026 at 3:42 PM"},
		{"closed by nobody known", State{Reason: ReasonClosed}, "",
			"Closed at May 14, 2026 at 3:42 PM"},
		{"changes requested", State{Reason: ReasonChangesRequested, ChangesRequestedBy: "hjed"}, "",
			"Changes requested by @hjed"},
		{"checks failing", State{Reason: ReasonChecksFailed}, "", "Checks failing"},
		{"checks running", State{Reason: ReasonChecksRunning}, "", "Checks running"},
		{"approved", State{Reason: ReasonApproved}, "", "Approved — awaiting merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Label(d, tc.closedBy); got != tc.want {
				t.Errorf("Label = %q, want %q", got, tc.want)
			}
		})
	}
}

// A team-based request is not a direct one; the mirror deliberately ignores
// those, or it would carry everyone else's work.
func TestRequestedIsDirectOnly(t *testing.T) {
	d := github.Detail{RequestedUsers: []string{"Miere"}, RequestedTeams: []string{"cloud-services"}}
	if !d.Requested("miere") {
		t.Error("a direct request should match case-insensitively")
	}
	if d.Requested("someone-else") {
		t.Error("matched a login that was not requested")
	}
	if (github.Detail{RequestedTeams: []string{"cloud-services"}}).Requested("miere") {
		t.Error("a team request must not count as a direct request")
	}
}
