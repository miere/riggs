package pullrequest

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Collapsing a review request that was settled somewhere else.
//
// The Approve button collapses the card it was clicked on, which covers exactly
// one way a review can end: through Riggs. Approve on github.com, or merge
// without approving at all, and nothing here ever hears about it — the click
// handler was the only reader the `git.pr.ask:` stream had, so the card sits
// open forever still offering an Approve that can only fail. Which is precisely
// the state the settle exists to avoid, reached by the ordinary route.
//
// So a pass sweeps them. It costs one conditionally-cached read per open card,
// on a stream that holds a handful of entries, and a card that settles is never
// read again: the state on the ledger entry is checked before GitHub is.

// settleableReasons are the states a review request has finished in.
//
// The three terminal ones, plus approved. An approval is what the card asked
// for, and it is what the button settles on today — the sweep and the button
// agreeing matters more here than the fact that an approval can later be
// dismissed, which would leave a collapsed card the button would have left
// collapsed too.
//
// Deliberately NOT changes_requested, nor either checks state. Those are
// moments in a review rather than the end of one: a red build goes green, and
// changes requested by somebody else says nothing about whether this reviewer
// is still wanted.
var settleableReasons = map[string]bool{
	ReasonMerged:   true,
	ReasonClosed:   true,
	ReasonArchived: true,
	ReasonApproved: true,
}

// AskOutcome records what a sweep did to one review-request card.
type AskOutcome struct {
	Ref   string `json:"ref"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// AskLines renders the settled cards for a human, as trailing lines on whatever
// report the sweep ran under. Empty for an empty sweep, which is the ordinary
// case: most ticks settle nothing.
func AskLines(outcomes []AskOutcome) string {
	var b strings.Builder
	for _, o := range outcomes {
		line := fmt.Sprintf("  %-40s %-16s settled", o.Ref, o.State)
		if o.Error != "" {
			line += " ERROR: " + o.Error
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// SettleAsks collapses every open review-request card whose pull request has
// stopped being reviewable, and reports the ones it touched. A card that is
// still worth reviewing is not reported: the interesting line is the one that
// changed.
//
// A read that fails is reported against its own card and the sweep carries on.
// One unreachable pull request must not leave every other card open.
func (e *Engine) SettleAsks(ctx context.Context, target slack.Target, dryRun bool) ([]AskOutcome, error) {
	cards, err := e.store.CardsWithPrefix(ctx, AskKeyPrefix)
	if err != nil {
		return nil, err
	}

	var out []AskOutcome
	for _, ke := range cards {
		// Settled once, never re-read. This is what the state on the entry is
		// for: knowing a card is finished without asking GitHub about it.
		if ke.State == AskStateDone {
			continue
		}
		if outcome := e.settleAsk(ctx, ke, target, dryRun); outcome != nil {
			out = append(out, *outcome)
		}
	}
	return out, nil
}

// settleAsk collapses one card, or returns nil when the pull request behind it
// is still up for review.
func (e *Engine) settleAsk(ctx context.Context, ke notify.KeyedEntry,
	target slack.Target, dryRun bool) *AskOutcome {

	ref := strings.TrimPrefix(ke.Key, AskKeyPrefix)
	repo, number, err := SplitRef(ref)
	if err != nil {
		return &AskOutcome{Ref: ref, Error: err.Error()}
	}

	d, err := e.gh.PullRequestDetail(ctx, repo, number)
	if err != nil {
		return &AskOutcome{Ref: ref, Error: err.Error()}
	}
	// mustDecide is false, and the sweep loses nothing by it: merged, closed and
	// archived answer from the detail alone, and a pull request that is not
	// green cannot be approved, so neither case pays for the decision query. A
	// green one falls through to it either way.
	r, err := e.resolveFrom(ctx, d, false)
	if err != nil {
		return &AskOutcome{Ref: ref, Error: err.Error()}
	}
	if !settleableReasons[r.State.Reason] {
		return nil
	}

	outcome := &AskOutcome{Ref: ref, State: r.State.Token()}
	if dryRun {
		return outcome
	}

	// The card's own channel, not the pass's. An ask is posted where the
	// reviewer is — a shared channel, or a DM — which is rarely where whatever
	// is running this sweep would otherwise write.
	dest := target
	dest.Channel = ke.Channel
	// The label the engine already derived, rather than the button's generic
	// "Approved and merged": this path knows whether the pull request was
	// merged, closed by somebody, or is still waiting on a merge, and the button
	// never could.
	card := AskSettledCard(d, Body(d), r.Label)
	if _, err := e.notifier.Upsert(ctx, ke.Key, dest, card,
		AskFallbackText(d), AskStateDone); err != nil {
		outcome.Error = err.Error()
	}
	return outcome
}
