package ticket

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slackmd"
)

// Engine advertises tickets and keeps their cards in step.
type Engine struct {
	jira     Source
	store    *notify.Store
	notifier *notify.Notifier
	// admin is who may act on a card, and who idle nudges tag.
	admin Admin
	now   func() time.Time
}

// Admin is the single human this queue serves.
type Admin struct {
	SlackUserID string
	JiraEmail   string
}

// NewEngine builds the reconciler.
func NewEngine(src Source, store *notify.Store, n *notify.Notifier, admin Admin) *Engine {
	return &Engine{jira: src, store: store, notifier: n, admin: admin, now: time.Now}
}

// WithClock overrides the clock; intended for tests.
func (e *Engine) WithClock(f func() time.Time) *Engine {
	e.now = f
	return e
}

// Outcome records what happened to one ticket.
type Outcome struct {
	Key    string `json:"key"`
	State  string `json:"state"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}

// Report is the result of one pass.
type Report struct {
	Query    string    `json:"query,omitempty"`
	Found    int       `json:"found"`
	Outcomes []Outcome `json:"outcomes"`
	DryRun   bool      `json:"dry_run,omitempty"`
}

// String renders the report for a human.
func (r Report) String() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("[dry run] ")
	}
	fmt.Fprintf(&b, "%d ticket(s) matched\n", r.Found)
	for _, o := range r.Outcomes {
		line := fmt.Sprintf("  %-14s %-10s %s", o.Key, o.State, o.Action)
		if o.Error != "" {
			line += " ERROR: " + o.Error
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// Poll advertises new tickets matching jql, and collapses any tracked card
// whose ticket has since been handled outside Slack.
func (e *Engine) Poll(ctx context.Context, jql string, target slack.Target, dryRun bool) (Report, error) {
	issues, err := e.jira.Search(ctx, jql, 20)
	if err != nil {
		return Report{}, err
	}
	report := Report{Query: jql, Found: len(issues), DryRun: dryRun}

	seen := map[string]bool{}
	for _, issue := range issues {
		seen[issue.Key] = true
		if o := e.advertise(ctx, issue, target, dryRun); o != nil {
			report.Outcomes = append(report.Outcomes, *o)
		}
	}

	// Anything tracked and still live but no longer matching the query has
	// been handled elsewhere — assigned in Jira, or moved out of Ready. The
	// query is the source of truth for "still up for grabs", so a card that
	// falls out of it must collapse rather than keep advertising work that is
	// already taken.
	tracked, err := e.store.CardsWithPrefix(ctx, KeyPrefix)
	if err != nil {
		return report, err
	}
	for _, ke := range tracked {
		key := strings.TrimPrefix(ke.Key, KeyPrefix)
		if seen[key] || !State(ke.State).Live() {
			continue
		}
		if o := e.collapseHandled(ctx, key, target, dryRun); o != nil {
			report.Outcomes = append(report.Outcomes, *o)
		}
	}
	return report, nil
}

// advertise posts or refreshes one ticket's card.
func (e *Engine) advertise(ctx context.Context, issue jira.Issue, target slack.Target, dryRun bool) *Outcome {
	key := Key(issue.Key)
	out := Outcome{Key: issue.Key, State: string(Pending)}

	_, tracked, err := e.store.Card(ctx, key)
	if err != nil {
		return &Outcome{Key: issue.Key, Action: "error", Error: err.Error()}
	}

	summary, err := e.summaryFor(ctx, key, issue, dryRun)
	if err != nil {
		return &Outcome{Key: issue.Key, Action: "error", Error: err.Error()}
	}
	card := Card(issue, summary, Pending)

	if dryRun {
		out.Action = "post"
		if tracked {
			out.Action = "already advertised"
		}
		return &out
	}
	action, err := e.notifier.Upsert(ctx, key, target, card,
		AvailableText(e.jira.BrowseURL(issue.Key)), string(Pending))
	if err != nil {
		return &Outcome{Key: issue.Key, Action: "error", Error: err.Error()}
	}
	out.Action = string(action)
	return &out
}

// collapseHandled rewrites a card whose ticket was claimed outside Slack.
func (e *Engine) collapseHandled(ctx context.Context, issueKey string, target slack.Target, dryRun bool) *Outcome {
	issue, err := e.jira.Get(ctx, issueKey)
	if err != nil {
		// A ticket we cannot read is left alone: collapsing it on a transient
		// failure would claim it was handled when it may not have been.
		return &Outcome{Key: issueKey, Action: "error", Error: err.Error()}
	}
	out := Outcome{Key: issueKey, State: string(Resolved)}
	if dryRun {
		out.Action = "collapse"
		return &out
	}
	action, err := e.notifier.Upsert(ctx, Key(issueKey), target,
		Card(issue, "", Resolved), UnavailableText(e.jira.BrowseURL(issueKey)), string(Resolved))
	if err != nil {
		return &Outcome{Key: issueKey, Action: "error", Error: err.Error()}
	}
	out.Action = string(action)
	return &out
}

// summaryFor returns the card body, computing it only once per ticket.
// summaryFor is the card body: the opening of the ticket's own description,
// translated into Slack's mrkdwn.
//
// It replaced an LLM summary for the same three reasons the pull-request card
// did (§7d) — the seconds it cost on a per-ticket loop, the dependency on a
// local `claude` binary, and output that changed between renders and so could
// not be honestly fingerprinted. A reporter's description is not obviously
// improved by being paraphrased.
//
// Pure now: no I/O, no cache, no dry-run special case. There is nothing left
// that a preview would be expensive to do.
func (e *Engine) summaryFor(_ context.Context, _ string, issue jira.Issue, _ bool) (string, error) {
	if body := strings.TrimSpace(slackmd.Excerpt(issue.Description, BodyParagraphs)); body != "" {
		return body, nil
	}
	// A card with no body renders no section at all, which reads as though
	// something failed.
	return slackmd.Convert(issue.Summary).Text, nil
}

// BodyParagraphs is how much of a description a ticket card shows.
const BodyParagraphs = 2

// ActionResult reports a button click's outcome.
type ActionResult struct {
	Key      string `json:"key"`
	Action   string `json:"action"`
	State    string `json:"state"`
	Assignee string `json:"assignee,omitempty"`
	Message  string `json:"message"`
}

// String renders the result for the CLI.
func (r ActionResult) String() string { return r.Message }

// Assign claims a ticket for the admin: it assigns in Jira, moves it to In
// Progress, collapses the card and confirms in-thread.
//
// actor is the Slack user who clicked. Only the configured admin may act — a
// card is visible to a whole channel, and a button that anyone can press would
// assign work to someone who never asked for it.
func (e *Engine) Assign(ctx context.Context, issueKey, actor string, target slack.Target) (ActionResult, error) {
	if err := e.authorise(actor); err != nil {
		return ActionResult{}, err
	}
	if e.admin.JiraEmail == "" {
		return ActionResult{}, fmt.Errorf("no admin.jira-email configured: nobody to assign %s to", issueKey)
	}

	user, err := e.jira.FindUser(ctx, e.admin.JiraEmail)
	if err != nil {
		return ActionResult{}, err
	}
	if err := e.jira.Assign(ctx, issueKey, user.AccountID); err != nil {
		return ActionResult{}, err
	}
	// A ticket assigned but left in Ready would be advertised again by the
	// next poll, so the transition is part of claiming it, not a nicety.
	if err := e.jira.Transition(ctx, issueKey, "In Progress"); err != nil {
		return ActionResult{}, fmt.Errorf("assigned %s but could not move it to In Progress: %w", issueKey, err)
	}

	issue, err := e.jira.Get(ctx, issueKey)
	if err != nil {
		// The assignment landed; fall back to what we know so the card still
		// collapses rather than staying claimable.
		issue = jira.Issue{Key: issueKey, Assignee: user.DisplayName, Updated: e.now()}
	}
	if issue.Assignee == "" {
		issue.Assignee = user.DisplayName
	}

	if _, err := e.notifier.Upsert(ctx, Key(issueKey), target, Card(issue, "", Assigned),
		UnavailableText(e.jira.BrowseURL(issueKey)), string(Assigned)); err != nil {
		return ActionResult{}, err
	}
	if _, err := e.notifier.Thread(ctx, Key(issueKey), target, AssignedText(actor), notify.Latch{}); err != nil {
		return ActionResult{}, err
	}
	return ActionResult{
		Key: issueKey, Action: "assign", State: string(Assigned), Assignee: issue.Assignee,
		Message: fmt.Sprintf("✓ %s assigned to %s and moved to In Progress.", issueKey, issue.Assignee),
	}, nil
}

// Dismiss waves a ticket off without touching Jira.
func (e *Engine) Dismiss(ctx context.Context, issueKey, actor string, target slack.Target) (ActionResult, error) {
	if err := e.authorise(actor); err != nil {
		return ActionResult{}, err
	}
	// Jira is deliberately untouched: dismissing means "not for me", not
	// "handled". The ticket stays exactly as it is for anyone else.
	issue, err := e.jira.Get(ctx, issueKey)
	if err != nil {
		issue = jira.Issue{Key: issueKey, Updated: e.now()}
	}
	if _, err := e.notifier.Upsert(ctx, Key(issueKey), target, Card(issue, "", Dismissed),
		UnavailableText(e.jira.BrowseURL(issueKey)), string(Dismissed)); err != nil {
		return ActionResult{}, err
	}
	if _, err := e.notifier.Thread(ctx, Key(issueKey), target, DismissedText(actor), notify.Latch{}); err != nil {
		return ActionResult{}, err
	}
	return ActionResult{
		Key: issueKey, Action: "dismiss", State: string(Dismissed),
		Message: fmt.Sprintf("✓ %s dismissed.", issueKey),
	}, nil
}

// authorise enforces the allow-list.
func (e *Engine) authorise(actor string) error {
	if e.admin.SlackUserID == "" {
		return fmt.Errorf("no admin.slack-user-id configured: refusing to act on an unverified click")
	}
	if actor == "" {
		return fmt.Errorf("no acting user given: refusing to act on an unattributed click")
	}
	if actor != e.admin.SlackUserID {
		return fmt.Errorf("user %s is not allowed to act on these cards", actor)
	}
	return nil
}
