// Package pullrequest turns raw GitHub data into the review queue's state.
//
// It is free of I/O on purpose: the reconcile loop stays correct ("update only
// when the derived state actually changed") precisely because deriving the
// state is a deterministic function of the PR data. That also makes it
// testable against captured payloads.
//
// The model is flattened to two visual states, exactly as the Python it
// replaces:
//
//   - Reviewable (A) — a direct review request, checks green, PR open, no
//     verdict yet. The only state with buttons, and the only one that pings.
//   - Not-reviewable (B) — everything else, collapsed, with a label saying
//     why.
//
// Both are derived live every tick, so the model is non-sticky: when the
// review lands GitHub drops the reviewer from the requested list and the card
// collapses; a re-request flips it straight back.
package pullrequest

import (
	"fmt"
	"time"

	"github.com/miere/riggs-mcp/internal/github"
)

// Reasons a pull request is not reviewable, highest precedence first.
const (
	ReasonMerged           = "merged"
	ReasonClosed           = "closed"
	ReasonArchived         = "archived"
	ReasonChangesRequested = "changes_requested"
	ReasonChecksFailed     = "checks_failed"
	ReasonChecksRunning    = "checks_running"
	ReasonApproved         = "approved"
)

// TerminalReasons can never be left. Once here nothing more will change, so
// the loop stops re-fetching the pull request entirely.
//
// Archived counts, with one caveat: a repository *can* be unarchived. That is
// safe here because scope() is the union of discovery and tracked-non-terminal
// pull requests — unarchiving puts the pull request back in the search results
// and discovery re-adopts it. Treating it as terminal only stops us paying
// reads for a card that cannot move.
var TerminalReasons = map[string]bool{
	ReasonMerged: true, ReasonClosed: true, ReasonArchived: true,
}

// Check conclusions treated as a hard failure. Everything else that has
// completed (success, neutral, skipped) counts as passing.
var failedConclusions = map[string]bool{
	"FAILURE": true, "TIMED_OUT": true, "CANCELLED": true,
	"ACTION_REQUIRED": true, "STARTUP_FAILURE": true, "STALE": true,
}

// Legacy commit-status states.
var (
	legacyRunning = map[string]bool{"PENDING": true, "EXPECTED": true}
	legacyFailed  = map[string]bool{"FAILURE": true, "ERROR": true}
)

// CheckStatus is the rollup of every check on the head commit.
type CheckStatus struct {
	Passed  bool `json:"passed"`
	Running bool `json:"running"`
	Failed  bool `json:"failed"`
	Total   int  `json:"total"`
}

// ClassifyChecks reduces the checks on a commit to a rollup, mirroring how
// GitHub itself does it:
//
//   - a check run that has not completed is still running;
//   - a legacy status context in pending/expected is still running — crucially
//     this includes *required* checks that have not reported yet, so a PR is
//     never called green prematurely;
//   - passing requires at least one check and nothing running or failed.
func ClassifyChecks(checks []github.Check) CheckStatus {
	var s CheckStatus
	for _, c := range checks {
		s.Total++
		if c.Legacy {
			switch {
			case legacyRunning[c.Conclusion]:
				s.Running = true
			case legacyFailed[c.Conclusion]:
				s.Failed = true
			}
			continue
		}
		switch {
		case c.Status != "COMPLETED":
			s.Running = true
		case failedConclusions[c.Conclusion]:
			s.Failed = true
		}
	}
	s.Passed = s.Total > 0 && !s.Running && !s.Failed
	return s
}

// Verdict is the review outcome across all reviewers.
type Verdict struct {
	// ChangesRequested is true when someone's standing review asks for
	// changes.
	ChangesRequested bool
	// ChangesRequestedBy is that reviewer's login.
	ChangesRequestedBy string
	// Approved mirrors GitHub's own reviewDecision == APPROVED.
	//
	// It is READ FROM GitHub, never reconstructed from the review list. Two
	// attempts to derive it were wrong in opposite directions and the parity
	// check caught both — see github.ReviewDecision for the two live
	// counter-examples.
	Approved bool
}

// DeriveVerdict combines GitHub's own decision with the review list.
//
// The decision settles whether the pull request is approved or blocked; the
// reviews are consulted only to name *who* asked for changes, which the
// decision does not carry. Pass the output of github.LatestByAuthor.
func DeriveVerdict(decision string, reviews []github.Review) Verdict {
	v := Verdict{
		Approved:         decision == github.DecisionApproved,
		ChangesRequested: decision == github.DecisionChangesRequested,
	}
	if v.ChangesRequested {
		for _, r := range reviews {
			if r.State == "CHANGES_REQUESTED" {
				v.ChangesRequestedBy = r.Author
				break
			}
		}
	}
	return v
}

// State is the derived, renderable state of one pull request.
type State struct {
	Reviewable bool   `json:"reviewable"`
	Reason     string `json:"reason,omitempty"`
	// ChangesRequestedBy is carried through for the label.
	ChangesRequestedBy string `json:"changes_requested_by,omitempty"`
}

// Terminal reports whether this state can never change again.
func (s State) Terminal() bool { return TerminalReasons[s.Reason] }

// Derive maps (lifecycle, verdict, checks, requested?) onto the two states.
//
// Precedence for the not-reviewable reason, highest first:
//
//  1. merged
//  2. closed
//  3. archived — the repository is read-only, so no review can ever land
//  4. changes requested
//  5. a check failed
//  6. — reviewable slots in here: a direct request, green —
//  7. still building, or no checks at all
//  8. approved / not ours
//
// Archived ranks below merged and closed on purpose: how a pull request ended
// is more useful on the card than the state of the repository around it.
func Derive(d github.Detail, checks CheckStatus, v Verdict, requested bool) State {
	switch {
	case d.Merged:
		return State{Reason: ReasonMerged}
	case d.State == "closed":
		return State{Reason: ReasonClosed}
	case d.Archived:
		return State{Reason: ReasonArchived}
	case v.ChangesRequested:
		return State{Reason: ReasonChangesRequested, ChangesRequestedBy: v.ChangesRequestedBy}
	case checks.Failed:
		return State{Reason: ReasonChecksFailed}
	case requested && checks.Passed && !v.Approved:
		return State{Reviewable: true}
	case checks.Running || checks.Total == 0:
		return State{Reason: ReasonChecksRunning}
	default:
		// Green but not actionable by us: already approved, or we have dropped
		// off the request list because the review has landed.
		return State{Reason: ReasonApproved}
	}
}

// localZone is the reviewer's timezone. Timestamps are shown as wall-clock
// local time, never UTC — a card saying 05:42 for something that happened at
// 15:42 is worse than no timestamp.
var localZone = func() *time.Location {
	if loc, err := time.LoadLocation("Australia/Sydney"); err == nil {
		return loc
	}
	// No tzdata on the host: a fixed AEST offset beats UTC.
	return time.FixedZone("AEST", 10*60*60)
}()

// FormatTime renders a UTC instant as a local wall-clock label, e.g.
// "May 14, 2026 at 3:42 PM".
func FormatTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "unknown time"
	}
	local := t.In(localZone)
	hour := local.Hour() % 12
	if hour == 0 {
		hour = 12
	}
	return fmt.Sprintf("%s %d, %d at %d:%02d %s",
		local.Format("Jan"), local.Day(), local.Year(), hour, local.Minute(), local.Format("PM"))
}

// Label renders the bottom line of a collapsed card: what happened, and when.
// closedBy may be empty, in which case the label degrades to a timestamp.
func (s State) Label(d github.Detail, closedBy string) string {
	switch s.Reason {
	case ReasonMerged:
		return "Merged at: " + FormatTime(d.MergedAt)
	case ReasonClosed:
		if closedBy != "" {
			return fmt.Sprintf("Closed by @%s at %s", closedBy, FormatTime(d.ClosedAt))
		}
		return "Closed at " + FormatTime(d.ClosedAt)
	case ReasonArchived:
		return "Repository archived — read-only, cannot be reviewed or merged"
	case ReasonChangesRequested:
		if s.ChangesRequestedBy != "" {
			return "Changes requested by @" + s.ChangesRequestedBy
		}
		return "Changes requested"
	case ReasonChecksFailed:
		return "Checks failing"
	case ReasonChecksRunning:
		return "Checks running"
	case ReasonApproved:
		return "Approved — awaiting merge"
	default:
		return "Not reviewable"
	}
}
