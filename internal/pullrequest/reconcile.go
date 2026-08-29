package pullrequest

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
)

// KeyPrefix namespaces this domain's cards in the ledger.
const KeyPrefix = "git.pr:"

// tagLatch fires the reviewer ping once per episode of being asked to review.
const tagLatch = "tagged"

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

// Resolve reads a pull request and derives its state in full. It is what the
// parity check uses, where every entry matters.
func (e *Engine) Resolve(ctx context.Context, repo string, number int) (Resolved, error) {
	return e.resolve(ctx, repo, number, true)
}

// resolve reads a pull request, ordering the calls cheapest-first so a pull
// request that will be discarded costs as little as possible.
//
// Discovery matches team-based review requests too, so a tick sees far more
// pull requests than it acts on — 48 against 8 on the live queue. Resolving
// all of them fully cost more GraphQL than the `gh pr list` this replaces,
// which would have defeated the point. So:
//
//   - merged or closed short-circuits: that reason outranks anything reviews
//     or checks could say;
//   - checks (REST, conditionally cached) come before the decision (GraphQL,
//     billed per query);
//   - an untracked pull request that is not green can never be reviewable, so
//     it is answered without asking GitHub for a decision at all.
//
// mustDecide forces the decision read for a pull request we are tracking,
// where the exact reason is what the card says.
func (e *Engine) resolve(ctx context.Context, repo string, number int, mustDecide bool) (Resolved, error) {
	d, err := e.gh.PullRequestDetail(ctx, repo, number)
	if err != nil {
		return Resolved{}, err
	}
	return e.resolveFrom(ctx, d, mustDecide)
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

// Outcome records what happened to one card.
type Outcome struct {
	Ref    string `json:"ref"`
	State  string `json:"state"`
	Action string `json:"action"`
	Tagged bool   `json:"tagged,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Report is the result of one reconcile pass.
type Report struct {
	Considered int          `json:"considered"`
	Outcomes   []Outcome    `json:"outcomes"`
	Stats      github.Stats `json:"github_requests"`
	DryRun     bool         `json:"dry_run"`
}

// String renders the report for a human.
func (r Report) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[dry run] ")
	}
	fmt.Fprintf(&b, "considered %d pull request(s)\n", r.Considered)
	for _, o := range r.Outcomes {
		line := fmt.Sprintf("  %-40s %-16s %s", o.Ref, o.State, o.Action)
		if o.Tagged {
			line += " +tagged"
		}
		if o.Error != "" {
			line += " ERROR: " + o.Error
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "github: %d request(s), %d not-modified",
		r.Stats.Requests, r.Stats.NotModified)
	return b.String()
}

// Run performs one reconcile pass.
//
// In dry run nothing is sent and nothing is written: the intended action is
// computed from the stored fingerprint and reported. A preview with side
// effects is not a preview.
func (e *Engine) Run(ctx context.Context, target slack.Target, dryRun bool) (Report, error) {
	refs, err := e.scope(ctx)
	if err != nil {
		return Report{}, err
	}

	report := Report{Considered: len(refs), DryRun: dryRun}
	for _, ref := range refs {
		outcome := e.reconcile(ctx, ref, target, dryRun)
		if outcome != nil {
			report.Outcomes = append(report.Outcomes, *outcome)
		}
	}
	if c, ok := e.gh.(interface{ Stats() github.Stats }); ok {
		report.Stats = c.Stats()
	}
	return report, nil
}

// scope decides which pull requests this pass is responsible for:
//
//	(we are a direct requested reviewer) ∪ (already tracked, not yet terminal)
//
// The second half is what lets a card collapse to its final state after the
// review lands and GitHub drops us from the request list. Terminal cards are
// never re-fetched: nothing about them can change again.
func (e *Engine) scope(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}

	prs, err := e.gh.ReviewRequested(ctx, e.login, searchLimit)
	if err != nil {
		return nil, fmt.Errorf("discovering review requests: %w", err)
	}
	for _, p := range prs {
		if p.Repo != "" {
			seen[p.Ref()] = true
		}
	}

	tracked, err := e.store.CardsWithPrefix(ctx, KeyPrefix)
	if err != nil {
		return nil, err
	}
	for _, ke := range tracked {
		if TerminalReasons[ke.State] {
			continue
		}
		seen[strings.TrimPrefix(ke.Key, KeyPrefix)] = true
	}

	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs, nil
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

// reconcile brings one pull request's card into line. A nil return means the
// pull request was deliberately ignored and nothing was touched.
func (e *Engine) reconcile(ctx context.Context, ref string, target slack.Target, dryRun bool) *Outcome {
	repo, number, err := SplitRef(ref)
	if err != nil {
		return &Outcome{Ref: ref, Action: "error", Error: err.Error()}
	}
	key := Key(ref)

	entry, tracked, err := e.store.Card(ctx, key)
	if err != nil {
		return &Outcome{Ref: ref, Action: "error", Error: err.Error()}
	}

	// The detail alone settles both cheap exclusions, so a pull request we
	// will never act on costs exactly one conditionally-cached read.
	d, err := e.gh.PullRequestDetail(ctx, repo, number)
	if err != nil {
		return &Outcome{Ref: ref, Action: "error", Error: err.Error()}
	}
	if d.Draft {
		// A draft is not up for review; leave whatever is tracked untouched.
		return nil
	}
	if !d.Requested(e.login) && !tracked {
		// A team-only request we never adopted. Discovery matches those, and
		// mirroring them would flood the channel with other people's work.
		return nil
	}

	r, err := e.resolveFrom(ctx, d, tracked)
	if err != nil {
		return &Outcome{Ref: ref, Action: "error", Error: err.Error()}
	}
	if !tracked && !r.State.Reviewable {
		// Dead on arrival: never shown, and not reviewable now. Nothing is
		// posted while a PR builds, and nothing at all if it dies before ever
		// going green.
		return nil
	}

	summary, err := e.summaryFor(ctx, key, r, dryRun)
	if err != nil {
		return &Outcome{Ref: ref, Action: "error", Error: err.Error()}
	}

	card := Card(r.Detail, summary, r.State, r.Label)
	text := FallbackText(r.Detail, r.State, r.Label)
	out := Outcome{Ref: ref, State: r.State.Token()}

	if dryRun {
		out.Action = string(plan(entry, tracked, card.Fingerprint(), r.State.Token()))
		out.Tagged = r.State.Reviewable && !e.alreadyTagged(ctx, key)
		return &out
	}

	action, err := e.notifier.Upsert(ctx, key, target, card, text, r.State.Token())
	if err != nil {
		return &Outcome{Ref: ref, State: r.State.Token(), Action: "error", Error: err.Error()}
	}
	out.Action = string(action)

	if !r.State.Reviewable {
		// Leaving the reviewable state reopens the latch, which is what
		// re-tags the reviewer if the PR comes back around.
		if err := e.notifier.ClearLatch(ctx, key, tagLatch); err != nil {
			out.Error = err.Error()
		}
		return &out
	}

	sent, err := e.notifier.Thread(ctx, key, target,
		TagText(e.slackUserID, action == notify.Posted), notify.Once(tagLatch))
	if err != nil {
		out.Error = err.Error()
	}
	out.Tagged = sent
	return &out
}

// summaryFor returns the card body: the opening of the description, converted
// (§7d). Pure — there is nothing left that a preview would be expensive to do,
// so the dry-run special case and the cache both went with the LLM call.
func (e *Engine) summaryFor(_ context.Context, _ string, r Resolved, _ bool) (string, error) {
	return Body(r.Detail), nil
}

// alreadyTagged reports whether the once-latch has fired, for dry-run
// reporting only.
func (e *Engine) alreadyTagged(ctx context.Context, key string) bool {
	_, fired, err := e.store.LatchFiredAt(ctx, key, tagLatch)
	return err == nil && fired
}

// plan computes what Upsert would do, without doing it.
func plan(entry notify.Entry, tracked bool, fingerprint, state string) notify.Outcome {
	switch {
	case !tracked:
		return notify.Posted
	case entry.Fingerprint == fingerprint && entry.State == state:
		return notify.Unchanged
	default:
		return notify.Updated
	}
}

// timeNow is the clock, indirected so tests can freeze it.
var timeNow = time.Now
