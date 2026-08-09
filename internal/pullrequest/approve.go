package pullrequest

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Verification settings. GitHub can lag between accepting a review and
// reporting it, which is the source of the intermittent "didn't get marked
// approved" the fire-and-forget bash rule used to produce.
const (
	verifyAttempts = 4
	verifyDelay    = 2 * time.Second
)

// Writer is the mutating GitHub seam.
type Writer interface {
	AuthenticatedLogin(ctx context.Context) (string, error)
	Reviews(ctx context.Context, repo string, number int) ([]github.Review, error)
	Approve(ctx context.Context, repo string, number int, body string) error
	Merge(ctx context.Context, repo string, number int) error
}

// Approver approves a pull request from a Slack button, and says what
// actually happened.
type Approver struct {
	gh    Writer
	store *notify.Store
	// poster sends the ad-hoc thread messages. This deliberately bypasses the
	// ledger: an acknowledgement is a one-off reply, not a card to maintain,
	// and recording it would pollute the fingerprint the reconcile loop
	// depends on.
	poster slack.Poster
	sleep  func(time.Duration)
	// reviewBody is the comment left with the approval.
	reviewBody string
}

// NewApprover builds the approver.
func NewApprover(gh Writer, store *notify.Store, poster slack.Poster) *Approver {
	return &Approver{gh: gh, store: store, poster: poster, sleep: time.Sleep,
		reviewBody: "Approved via Riggs."}
}

// WithSleep overrides the retry delay; intended for tests.
func (a *Approver) WithSleep(f func(time.Duration)) *Approver {
	a.sleep = f
	return a
}

// ApproveResult reports the outcome.
type ApproveResult struct {
	Ref string `json:"ref"`
	// Approved is true when an approval is standing at the end, however it got
	// there.
	Approved bool `json:"approved"`
	// AlreadyApproved is true when our approval was already standing and
	// nothing was submitted.
	AlreadyApproved bool `json:"already_approved"`
	Merged          bool `json:"merged"`
	DryRun          bool `json:"dry_run,omitempty"`
	// Message is the human sentence, and is what gets posted to the thread.
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// String renders the result for the CLI.
func (r ApproveResult) String() string { return r.Message }

// Run approves ref, optionally rebase-merging, and reports the real outcome to
// the card's thread.
//
// The Python this replaces posted "Approved" unconditionally. Every branch
// here posts what actually happened instead, because a button that always
// claims success is worse than no button.
func (a *Approver) Run(ctx context.Context, ref string, merge bool, target slack.Target, threadTS string) (ApproveResult, error) {
	return a.run(ctx, ref, merge, target, threadTS, false)
}

// DryRun reports what Run would do, touching neither GitHub nor Slack. It
// exists because the write half of this flow has no undo: approving the wrong
// pull request cannot be taken back from a Slack thread.
func (a *Approver) DryRun(ctx context.Context, ref string, merge bool, target slack.Target, threadTS string) (ApproveResult, error) {
	return a.run(ctx, ref, merge, target, threadTS, true)
}

func (a *Approver) run(ctx context.Context, ref string, merge bool, target slack.Target, threadTS string, dry bool) (ApproveResult, error) {
	repo, number, err := SplitRef(ref)
	if err != nil {
		return ApproveResult{}, err
	}
	result := ApproveResult{Ref: ref, DryRun: dry}

	// Resolve where to reply. An explicit thread wins (the workflow rule knows
	// which message was clicked); otherwise the ledger knows where the card
	// is, which is what makes this work when invoked by hand.
	channel, thread := a.thread(ctx, ref, target, threadTS)

	action := "Approving PR"
	if merge {
		action = "Approving & rebase-merging"
	}
	if !dry {
		a.say(ctx, target, channel, thread, fmt.Sprintf("⏺ %s — verifying with GitHub…", action))
	}

	login, err := a.gh.AuthenticatedLogin(ctx)
	if err != nil {
		// Not fatal: without a login we cannot tell whose approval is
		// standing, so we fall through and submit one.
		login = ""
	}

	standing, err := a.approvalStanding(ctx, repo, number, login)
	if err != nil {
		return a.fail(ctx, result, target, channel, thread,
			fmt.Sprintf("✗ Could not read reviews for %s — %v", ref, err))
	}

	if dry {
		result.AlreadyApproved, result.Approved = standing, standing
		switch {
		case standing && merge:
			result.Message = fmt.Sprintf("[dry run] %s is already approved; would rebase-merge it. "+
				"Reply would go to %s/%s.", ref, channel, thread)
		case standing:
			result.Message = fmt.Sprintf("[dry run] %s is already approved — nothing would be done.", ref)
		case merge:
			result.Message = fmt.Sprintf("[dry run] would approve %s as %s, verify, then rebase-merge. "+
				"Reply would go to %s/%s.", ref, loginOr(login), channel, thread)
		default:
			result.Message = fmt.Sprintf("[dry run] would approve %s as %s and verify. "+
				"Reply would go to %s/%s.", ref, loginOr(login), channel, thread)
		}
		return result, nil
	}

	if standing {
		result.AlreadyApproved, result.Approved = true, true
	} else {
		if err := a.gh.Approve(ctx, repo, number, a.reviewBody); err != nil {
			return a.fail(ctx, result, target, channel, thread,
				fmt.Sprintf("✗ Could not approve %s — %v", ref, err))
		}
		// Verify it registered. `gh` occasionally returned success without the
		// review landing; the same is true of the API.
		if !a.verify(ctx, repo, number, login) {
			return a.fail(ctx, result, target, channel, thread,
				fmt.Sprintf("⚠ Approval submitted for %s but GitHub has not recorded it yet — "+
					"double-check on GitHub.", ref))
		}
		result.Approved = true
	}

	if merge {
		if err := a.gh.Merge(ctx, repo, number); err != nil {
			return a.fail(ctx, result, target, channel, thread,
				fmt.Sprintf("✗ Approved %s but the rebase-merge failed — %v", ref, err))
		}
		result.Merged = true
		result.Message = fmt.Sprintf("✓ Approved & rebase-merged — %s.", ref)
	} else if result.AlreadyApproved {
		result.Message = fmt.Sprintf("✓ %s was already approved — nothing to do.", ref)
	} else {
		result.Message = fmt.Sprintf("✓ Approved %s.", ref)
	}

	a.say(ctx, target, channel, thread, result.Message)
	return result, nil
}

// approvalStanding reports whether login's *current* review is an approval.
//
// GitHub keeps one standing review per author: when new commits land a prior
// approval is dismissed. Only a still-standing APPROVED counts, so a dismissed
// review correctly falls through and gets re-approved.
//
// With no login known, it reports false: submitting a redundant approval is a
// no-op, whereas wrongly skipping one leaves the pull request unapproved.
func (a *Approver) approvalStanding(ctx context.Context, repo string, number int, login string) (bool, error) {
	if login == "" {
		return false, nil
	}
	reviews, err := a.gh.Reviews(ctx, repo, number)
	if err != nil {
		return false, err
	}
	for _, r := range reviews {
		if strings.EqualFold(r.Author, login) && r.State == "APPROVED" {
			return true, nil
		}
	}
	return false, nil
}

// verify confirms the approval landed, retrying briefly.
func (a *Approver) verify(ctx context.Context, repo string, number int, login string) bool {
	for attempt := 0; attempt < verifyAttempts; attempt++ {
		reviews, err := a.gh.Reviews(ctx, repo, number)
		if err == nil {
			for _, r := range reviews {
				if r.State != "APPROVED" {
					continue
				}
				// With no login known, any approval is taken as evidence.
				if login == "" || strings.EqualFold(r.Author, login) {
					return true
				}
			}
		}
		if attempt < verifyAttempts-1 {
			a.sleep(verifyDelay)
		}
	}
	return false
}

// thread decides where the outcome is posted: the explicit thread if given,
// else the tracked card's own message.
func (a *Approver) thread(ctx context.Context, ref string, target slack.Target, threadTS string) (channel, thread string) {
	if threadTS != "" {
		return target.Channel, threadTS
	}
	entry, found, err := a.store.Card(ctx, Key(ref))
	if err != nil || !found {
		return target.Channel, ""
	}
	return entry.Channel, entry.TS
}

// say posts a line into the thread. A failure to narrate must never change the
// outcome of the approval itself, so the error is deliberately dropped.
func (a *Approver) say(ctx context.Context, target slack.Target, channel, thread, text string) {
	if a.poster == nil {
		return
	}
	t := target
	if channel != "" {
		t.Channel = channel
	}
	_, _ = a.poster.Post(ctx, t, slack.Message{
		Text:     text,
		Blocks:   blockkit.ContextBlocks(text),
		ThreadTS: thread,
	})
}

// loginOr renders the acting login for a dry run.
func loginOr(login string) string {
	if login == "" {
		return "an unknown user (gh is not authenticated)"
	}
	return "@" + login
}

// fail records and announces a failure. The error is returned as part of the
// result rather than as a Go error so the CLI still prints the sentence that
// was posted to Slack.
func (a *Approver) fail(ctx context.Context, r ApproveResult, target slack.Target, channel, thread, msg string) (ApproveResult, error) {
	r.Message, r.Error = msg, msg
	a.say(ctx, target, channel, thread, msg)
	return r, fmt.Errorf("%s", msg)
}
