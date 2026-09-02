package ticket

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// fakeJira scripts the Jira seam.
type fakeJira struct {
	search     []jira.Issue
	searchErr  error
	issues     map[string]jira.Issue
	getErr     error
	user       jira.User
	userErr    error
	assignErr  error
	transErr   error
	assigned   []string
	transition []string
}

func (f *fakeJira) Search(context.Context, string, int) ([]jira.Issue, error) {
	return f.search, f.searchErr
}

func (f *fakeJira) Get(_ context.Context, key string) (jira.Issue, error) {
	if f.getErr != nil {
		return jira.Issue{}, f.getErr
	}
	i, ok := f.issues[key]
	if !ok {
		return jira.Issue{}, errors.New("no such issue " + key)
	}
	return i, nil
}

func (f *fakeJira) FindUser(context.Context, string) (jira.User, error) { return f.user, f.userErr }

func (f *fakeJira) Assign(_ context.Context, key, _ string) error {
	if f.assignErr != nil {
		return f.assignErr
	}
	f.assigned = append(f.assigned, key)
	return nil
}

func (f *fakeJira) Transition(_ context.Context, key, name string) error {
	if f.transErr != nil {
		return f.transErr
	}
	f.transition = append(f.transition, key+"->"+name)
	return nil
}

func (f *fakeJira) BrowseURL(key string) string { return "https://jira.test/browse/" + key }

type stubSummariser struct{ runs int }

func (s *stubSummariser) Summarise(_ context.Context, title, _ string) (string, error) {
	s.runs++
	return "goal: " + title, nil
}

var target = slack.Target{Profile: "default", BotToken: "xoxb", Channel: "C1"}

const adminID = "U0B20G0ET9T"

type rig struct {
	engine *Engine
	jira   *fakeJira
	slack  *slacktest.Fake
	store  *notify.Store
	now    time.Time
}

func newRig(t *testing.T, fj *fakeJira) *rig {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	fake := slacktest.New()
	r := &rig{jira: fj, slack: fake, store: store,
		now: time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)}
	r.engine = NewEngine(fj, store, notify.New(store, fake).WithClock(func() time.Time { return r.now }),
		Admin{SlackUserID: adminID, JiraEmail: "miere@nurturecloud.com"}).
		WithClock(func() time.Time { return r.now })
	return r
}

func ready(key, summary string) jira.Issue {
	return jira.Issue{Key: key, Summary: summary, Status: ReadyStatus,
		Description: "do the thing", Updated: time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)}
}

func TestPollAdvertisesNewTickets(t *testing.T) {
	fj := &fakeJira{search: []jira.Issue{ready("NYX-1", "Add a health probe")}}
	r := newRig(t, fj)

	report, err := r.engine.Poll(context.Background(), "project = NYX", target, false)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if report.Found != 1 || len(report.Outcomes) != 1 ||
		report.Outcomes[0].Action != string(notify.Posted) {
		t.Fatalf("report = %+v, want one posted card", report)
	}
	if len(r.slack.Calls) != 1 {
		t.Fatalf("slack calls = %v, want the card", r.slack.Kinds())
	}
	if !strings.Contains(r.slack.Calls[0].Msg.Text, "available for implementation") {
		t.Errorf("fallback = %q", r.slack.Calls[0].Msg.Text)
	}
}

// A second poll with nothing changed must make no Slack call.
func TestPollIsIdempotent(t *testing.T) {
	fj := &fakeJira{search: []jira.Issue{ready("NYX-1", "Add a health probe")}}
	r := newRig(t, fj)
	ctx := context.Background()

	if _, err := r.engine.Poll(ctx, "q", target, false); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	r.slack.Reset()
	report, err := r.engine.Poll(ctx, "q", target, false)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if report.Outcomes[0].Action != string(notify.Unchanged) {
		t.Errorf("action = %s, want unchanged", report.Outcomes[0].Action)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
	// The body is derived from the description now, so repeating a pass costs
	// nothing and needs no cache to stay stable.
	if got := ticketBody(t, r); got != ticketBody(t, r) {
		t.Errorf("the card body is not stable across renders: %q", got)
	}
}

// A ticket claimed in Jira drops out of the query. Its card must collapse
// rather than keep advertising work someone has already taken.
func TestPollCollapsesTicketsHandledOutsideSlack(t *testing.T) {
	fj := &fakeJira{search: []jira.Issue{ready("NYX-1", "Add a health probe")}}
	r := newRig(t, fj)
	ctx := context.Background()

	if _, err := r.engine.Poll(ctx, "q", target, false); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Someone assigned it directly in Jira, so it no longer matches.
	claimed := ready("NYX-1", "Add a health probe")
	claimed.Assignee, claimed.Status = "Annie", "In Progress"
	fj.search = nil
	fj.issues = map[string]jira.Issue{"NYX-1": claimed}
	r.slack.Reset()

	report, err := r.engine.Poll(ctx, "q", target, false)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].State != string(Resolved) {
		t.Fatalf("report = %+v, want the card collapsed", report)
	}
	if got := r.slack.Kinds(); len(got) != 1 || got[0] != "update" {
		t.Errorf("slack calls = %v, want one update", got)
	}
	entry, _, _ := r.store.Card(ctx, Key("NYX-1"))
	if entry.State != string(Resolved) {
		t.Errorf("stored state = %q, want resolved", entry.State)
	}
}

// A ticket we cannot read is left alone: collapsing on a transient failure
// would claim it was handled when it may not have been.
func TestPollLeavesUnreadableTicketsAlone(t *testing.T) {
	fj := &fakeJira{search: []jira.Issue{ready("NYX-1", "T")}}
	r := newRig(t, fj)
	ctx := context.Background()
	if _, err := r.engine.Poll(ctx, "q", target, false); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	fj.search = nil
	fj.getErr = errors.New("503 service unavailable")
	r.slack.Reset()

	report, _ := r.engine.Poll(ctx, "q", target, false)
	if len(report.Outcomes) != 1 || report.Outcomes[0].Error == "" {
		t.Errorf("report = %+v, want the failure reported", report)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want the card untouched", r.slack.Kinds())
	}
	entry, _, _ := r.store.Card(ctx, Key("NYX-1"))
	if entry.State != string(Pending) {
		t.Errorf("state = %q, want it still pending", entry.State)
	}
}

func TestPollDryRunWritesNothing(t *testing.T) {
	fj := &fakeJira{search: []jira.Issue{ready("NYX-1", "T")}}
	r := newRig(t, fj)
	ctx := context.Background()

	if _, err := r.engine.Poll(ctx, "q", target, true); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", r.slack.Kinds())
	}
	if _, found, _ := r.store.Card(ctx, Key("NYX-1")); found {
		t.Error("the dry run wrote a card")
	}
}

// Collapsing a card whose ticket was claimed outside Slack was the nudge's job
// too, and it re-checked Jira before pinging. With the nudge gone, the poll is
// the only thing that notices — so this is the pass that has to.
func TestPollCollapsesATicketClaimedOutsideSlack(t *testing.T) {
	fj := &fakeJira{search: []jira.Issue{ready("NYX-1", "T")}}
	r := newRig(t, fj)
	ctx := context.Background()
	if _, err := r.engine.Poll(ctx, "q", target, false); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	claimed := ready("NYX-1", "T")
	claimed.Assignee = "Annie"
	fj.issues = map[string]jira.Issue{"NYX-1": claimed}
	fj.search = nil
	r.now = r.now.Add(48 * time.Hour)
	r.slack.Reset()

	report, err := r.engine.Poll(ctx, "q", target, false)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(report.Outcomes) != 1 || report.Outcomes[0].State != string(Resolved) {
		t.Fatalf("report = %+v, want the card collapsed", report)
	}
	for _, c := range r.slack.Calls {
		if c.Msg.ThreadTS != "" {
			t.Errorf("posted in-thread on a claimed ticket: %q", c.Msg.Text)
		}
	}
}

// Claiming a ticket must both assign it AND move it out of Ready — otherwise
// the next poll re-advertises work that is already taken.
func TestAssignAssignsAndTransitions(t *testing.T) {
	assigned := ready("NYX-1", "T")
	assigned.Assignee = "Miere"
	fj := &fakeJira{
		search: []jira.Issue{ready("NYX-1", "T")},
		issues: map[string]jira.Issue{"NYX-1": assigned},
		user:   jira.User{AccountID: "acc-1", DisplayName: "Miere"},
	}
	r := newRig(t, fj)
	ctx := context.Background()
	if _, err := r.engine.Poll(ctx, "q", target, false); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	r.slack.Reset()

	res, err := r.engine.Assign(ctx, "NYX-1", adminID, target)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if len(fj.assigned) != 1 || fj.assigned[0] != "NYX-1" {
		t.Errorf("assigned = %v", fj.assigned)
	}
	if len(fj.transition) != 1 || fj.transition[0] != "NYX-1->In Progress" {
		t.Errorf("transitions = %v, want a move to In Progress", fj.transition)
	}
	if res.State != string(Assigned) || !strings.Contains(res.Message, "In Progress") {
		t.Errorf("result = %+v", res)
	}
	// The card collapses, then the claim is confirmed in its thread.
	if got := r.slack.Kinds(); len(got) != 2 || got[0] != "update" || got[1] != "post" {
		t.Errorf("slack calls = %v, want an update then a threaded reply", got)
	}
}

// A failed transition leaves the ticket assigned but still in Ready, which the
// poll would re-advertise. That has to be reported, not swallowed.
func TestAssignReportsAFailedTransition(t *testing.T) {
	fj := &fakeJira{
		user:     jira.User{AccountID: "acc-1", DisplayName: "Miere"},
		transErr: errors.New("no such transition"),
	}
	r := newRig(t, fj)

	_, err := r.engine.Assign(context.Background(), "NYX-1", adminID, target)
	if err == nil {
		t.Fatal("Assign = nil error when the transition failed")
	}
	if !strings.Contains(err.Error(), "In Progress") {
		t.Errorf("err = %v, want it to name the missing move", err)
	}
}

// Dismissing means "not for me", not "handled" — Jira must be untouched.
func TestDismissLeavesJiraAlone(t *testing.T) {
	fj := &fakeJira{
		search: []jira.Issue{ready("NYX-1", "T")},
		issues: map[string]jira.Issue{"NYX-1": ready("NYX-1", "T")},
	}
	r := newRig(t, fj)
	ctx := context.Background()
	if _, err := r.engine.Poll(ctx, "q", target, false); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	res, err := r.engine.Dismiss(ctx, "NYX-1", adminID, target)
	if err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if len(fj.assigned) != 0 || len(fj.transition) != 0 {
		t.Errorf("dismiss touched Jira: assigned=%v transitions=%v", fj.assigned, fj.transition)
	}
	if res.State != string(Dismissed) {
		t.Errorf("result = %+v", res)
	}
	entry, _, _ := r.store.Card(ctx, Key("NYX-1"))
	if entry.State != string(Dismissed) {
		t.Errorf("stored state = %q", entry.State)
	}
}

// A card is visible to a whole channel. A button anyone could press would
// assign work to someone who never asked for it.
func TestOnlyTheAdminMayAct(t *testing.T) {
	fj := &fakeJira{user: jira.User{AccountID: "acc-1"}}
	r := newRig(t, fj)
	ctx := context.Background()

	for _, actor := range []string{"U-SOMEONE-ELSE", ""} {
		if _, err := r.engine.Assign(ctx, "NYX-1", actor, target); err == nil {
			t.Errorf("Assign as %q = nil error", actor)
		}
		if _, err := r.engine.Dismiss(ctx, "NYX-1", actor, target); err == nil {
			t.Errorf("Dismiss as %q = nil error", actor)
		}
	}
	if len(fj.assigned) != 0 {
		t.Errorf("an unauthorised click assigned %v", fj.assigned)
	}
	if len(r.slack.Calls) != 0 {
		t.Errorf("an unauthorised click posted %v", r.slack.Kinds())
	}
}

// ticketBody re-derives a card body, to assert it does not move between passes.
func ticketBody(t *testing.T, _ *rig) string {
	t.Helper()
	e := &Engine{}
	body, err := e.summaryFor(context.Background(), "", ready("NYX-1", "Add a health probe"), false)
	if err != nil {
		t.Fatalf("summaryFor: %v", err)
	}
	return body
}
