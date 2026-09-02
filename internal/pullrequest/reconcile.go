package pullrequest

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
)

// searchLimit caps the discovery query. The search API has its own tight
// bucket (30/minute), so this is one call per tick and never fanned out.
const searchLimit = 100

// Source is the GitHub seam. github.Client satisfies it.
type Source interface {
	ReviewRequested(ctx context.Context, login string, limit int) ([]github.PullRequest, error)
	PullRequestDetail(ctx context.Context, repo string, number int) (github.Detail, error)
	Reviews(ctx context.Context, repo string, number int) ([]github.Review, error)
	ReviewDecision(ctx context.Context, repo string, number int) (string, error)
	Checks(ctx context.Context, repo, sha string) ([]github.Check, error)
	ClosedBy(ctx context.Context, repo string, number int) (string, error)
}

// Engine reconciles the review queue: it decides what each card should say and
// keeps Slack in step.
type Engine struct {
	gh       Source
	store    *notify.Store
	notifier *notify.Notifier
	// login is the GitHub handle whose review requests are mirrored.
	login string
	// slackUserID is who gets tagged in-thread.
	slackUserID string
}

// NewEngine builds the reconciler.
func NewEngine(gh Source, store *notify.Store, n *notify.Notifier, login, slackUserID string) *Engine {
	return &Engine{gh: gh, store: store, notifier: n, login: login, slackUserID: slackUserID}
}

// Token renders the state as the ledger's opaque label: "reviewable", or the
// reason it is not. Terminal detection reads this back without re-deriving
// anything from GitHub.
func (s State) Token() string {
	if s.Reviewable {
		return "reviewable"
	}
	return s.Reason
}

// Resolved is everything known about one pull request after the reads.
type Resolved struct {
	Detail  github.Detail `json:"-"`
	Checks  CheckStatus   `json:"checks"`
	Verdict Verdict       `json:"verdict"`
	State   State         `json:"state"`
	// Requested reports whether our login is a direct requested reviewer.
	Requested bool   `json:"requested"`
	Label     string `json:"label,omitempty"`
}

// resolveFrom derives state from an already-fetched pull request, so a caller
// that can rule it out from the detail alone pays for nothing further.
func (e *Engine) resolveFrom(ctx context.Context, d github.Detail, mustDecide bool) (Resolved, error) {
	repo, number := d.Repo, d.Number
	r := Resolved{Detail: d, Requested: d.Requested(e.login)}

	// Archived joins the short-circuit for the same reason merged and closed
	// are here: nothing checks or reviews could say would change the answer,
	// so the pull request costs exactly one read.
	if d.Merged || d.State == "closed" || d.Archived {
		r.State = Derive(d, CheckStatus{}, Verdict{}, r.Requested)
		closedBy := ""
		if r.State.Reason == ReasonClosed {
			// Best effort: the label degrades to a timestamp on failure.
			closedBy, _ = e.gh.ClosedBy(ctx, repo, number)
		}
		r.Label = r.State.Label(d, closedBy)
		return r, nil
	}

	checks, err := e.gh.Checks(ctx, repo, d.HeadSHA)
	if err != nil {
		return Resolved{}, err
	}
	r.Checks = ClassifyChecks(checks)

	// A pull request we do not track and that is not green cannot be
	// reviewable, whatever GitHub's decision says — so do not pay for one.
	if !mustDecide && !r.Checks.Passed {
		r.State = Derive(d, r.Checks, Verdict{}, r.Requested)
		r.Label = r.State.Label(d, "")
		return r, nil
	}

	decision, err := e.gh.ReviewDecision(ctx, repo, number)
	if err != nil {
		return Resolved{}, err
	}
	// The review list is only consulted to name who asked for changes, so it
	// is skipped entirely when nobody did.
	var reviews []github.Review
	if decision == github.DecisionChangesRequested {
		if reviews, err = e.gh.Reviews(ctx, repo, number); err != nil {
			return Resolved{}, err
		}
	}
	r.Verdict = DeriveVerdict(decision, reviews)

	r.State = Derive(d, r.Checks, r.Verdict, r.Requested)
	if !r.State.Reviewable {
		r.Label = r.State.Label(d, "")
	}
	return r, nil
}

// SplitRef parses "owner/repo#123".
func SplitRef(ref string) (repo string, number int, err error) {
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return "", 0, fmt.Errorf("not an owner/repo#number ref: %q", ref)
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("not an owner/repo#number ref: %q", ref)
	}
	return ref[:i], n, nil
}
