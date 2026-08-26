package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/riggs-mcp/internal/slack"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeListener replays a fixed set of callbacks and returns.
type fakeListener struct {
	callbacks []slackgo.InteractionCallback
	err       error
}

func (f *fakeListener) Listen(ctx context.Context, deliver func(slackgo.InteractionCallback)) error {
	for _, cb := range f.callbacks {
		deliver(cb)
	}
	return f.err
}

func overflowCallback(actionID, value, blockID string) slackgo.InteractionCallback {
	a := &slackgo.BlockAction{ActionID: actionID, BlockID: blockID}
	a.SelectedOption.Value = value
	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	cb.ActionCallback.BlockActions = []*slackgo.BlockAction{a}
	return cb
}

func TestRouterDispatchesOnActionAndIntent(t *testing.T) {
	r := NewRouter()
	var got []string
	r.Handle("pr_overflow", "approve_merge", HandlerFunc(func(_ context.Context, in slack.Interaction) error {
		got = append(got, "merge:"+in.Item)
		return nil
	}))
	r.Handle("pr_overflow", "ask_review", HandlerFunc(func(_ context.Context, in slack.Interaction) error {
		got = append(got, "ask:"+in.Item)
		return nil
	}))

	for _, intent := range []string{"ask_review", "approve_merge"} {
		in, _ := slack.DecodeInteraction(overflowCallback("pr_overflow", intent, "o/r#1"))
		matched, err := r.Route(context.Background(), in)
		if err != nil || !matched {
			t.Fatalf("Route(%s) = %v, %v", intent, matched, err)
		}
	}

	if len(got) != 2 || got[0] != "ask:o/r#1" || got[1] != "merge:o/r#1" {
		t.Fatalf("dispatch order = %v", got)
	}
}

// Same action_id, unknown intent: the table must not fall back to the other
// option on the same menu.
func TestRouterDoesNotMatchAnUnknownIntent(t *testing.T) {
	r := NewRouter()
	r.Handle("pr_overflow", "approve_merge", HandlerFunc(func(context.Context, slack.Interaction) error {
		t.Fatal("handler ran for an intent it was not registered for")
		return nil
	}))

	in, _ := slack.DecodeInteraction(overflowCallback("pr_overflow", "run_local_review", "o/r#1"))
	matched, err := r.Route(context.Background(), in)
	if matched || err != nil {
		t.Fatalf("Route = %v, %v; want no match and no error", matched, err)
	}
}

func TestRouterRejectsADuplicateRoute(t *testing.T) {
	r := NewRouter()
	noop := HandlerFunc(func(context.Context, slack.Interaction) error { return nil })
	r.Handle("pr_overflow", "approve_merge", noop)

	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate route did not panic")
		}
	}()
	r.Handle("pr_overflow", "approve_merge", noop)
}

// A handler that fails must not stop the daemon: the next click is separate
// work, and an exit here needs a human before any button works again.
func TestDaemonSurvivesAFailingHandler(t *testing.T) {
	r := NewRouter()
	calls := 0
	r.Handle("pr_overflow", "approve_merge", HandlerFunc(func(context.Context, slack.Interaction) error {
		calls++
		return errors.New("github is down")
	}))

	listener := &fakeListener{callbacks: []slackgo.InteractionCallback{
		overflowCallback("pr_overflow", "approve_merge", "o/r#1"),
		overflowCallback("pr_overflow", "approve_merge", "o/r#2"),
	}}

	if err := New(listener, r, "riggs", quietLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("handler ran %d time(s), want 2", calls)
	}
}

// Callbacks that carry no block action are acknowledged and dropped, not
// treated as failures.
func TestDaemonIgnoresCallbacksThatAreNotOurs(t *testing.T) {
	listener := &fakeListener{callbacks: []slackgo.InteractionCallback{
		{Type: slackgo.InteractionTypeViewSubmission},
	}}
	if err := New(listener, NewRouter(), "riggs", quietLogger()).Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}

func TestDaemonReportsAListenerFailure(t *testing.T) {
	listener := &fakeListener{err: errors.New("websocket closed")}
	err := New(listener, NewRouter(), "riggs", quietLogger()).Run(context.Background())
	if err == nil {
		t.Fatal("Run swallowed a listener failure")
	}
}

func TestRouterDescribesItsRoutes(t *testing.T) {
	r := NewRouter()
	if got := r.Describe(); got != "no routes registered" {
		t.Fatalf("empty Describe = %q", got)
	}
	noop := HandlerFunc(func(context.Context, slack.Interaction) error { return nil })
	r.Handle("pr_overflow", "approve_merge", noop)
	r.Handle("pr_overflow", "ask_review", noop)

	if got := r.Describe(); got != "pr_overflow/approve_merge, pr_overflow/ask_review" {
		t.Fatalf("Describe = %q", got)
	}
}
