package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/riggs-mcp/internal/slack"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeListener replays a fixed set of callbacks and returns.
type fakeListener struct {
	callbacks []slackgo.InteractionCallback
	homeOpens []string
	err       error
}

func (f *fakeListener) Listen(ctx context.Context, h Handlers) error {
	for _, cb := range f.callbacks {
		h.Interaction(cb)
	}
	for _, user := range f.homeOpens {
		h.AppHomeOpened(user)
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

// --- acknowledgement --------------------------------------------------------

// ackRecorder records which envelopes were acknowledged, which is the only
// thing Slack cares about.
type ackRecorder struct{ acked []string }

func (a *ackRecorder) Ack(req socketmode.Request, _ ...any) error {
	a.acked = append(a.acked, req.EnvelopeID)
	return nil
}

// Slack marks a control with a ⚠ when it does not get a response to the request
// it sent. EVERY event carrying a request must therefore be acknowledged,
// including the ones this daemon deliberately does nothing with — a link button
// raises an interaction even though Slack itself opens the URL.
//
// The real dispatch is exercised, not a stand-in: the bug this guards against
// was a missing branch, and a re-implementation in the test would have had the
// same shape as the code and agreed with it.
func TestEveryRequestIsAcked(t *testing.T) {
	l := NewSocketListener(slack.Credentials{Profile: "riggs"}, quietLogger())
	rec := &ackRecorder{}
	handlers := Handlers{Interaction: func(slackgo.InteractionCallback) {}}

	events := []socketmode.Event{
		{Type: socketmode.EventTypeInteractive, Data: overflowCallback("a", "b", "c")},
		{Type: socketmode.EventTypeHello},
		{Type: socketmode.EventTypeSlashCommand},
		// A type this switch has never heard of. It still gets an ack.
		{Type: socketmode.EventType("something_slack_added_later")},
		// An interactive event whose payload will not cast: acked before the
		// decode is even attempted, because Slack is owed an answer either way.
		{Type: socketmode.EventTypeInteractive, Data: "not a callback"},
	}
	for i, ev := range events {
		ev.Request = &socketmode.Request{EnvelopeID: fmt.Sprintf("env-%d", i)}
		l.dispatch(rec, ev, handlers)
	}

	if len(rec.acked) != len(events) {
		t.Fatalf("acked %v, want all %d events", rec.acked, len(events))
	}
}

// An event with no request has nothing to acknowledge, and must not panic
// reaching for one.
func TestAnEventWithNoRequestIsNotAcked(t *testing.T) {
	l := NewSocketListener(slack.Credentials{Profile: "riggs"}, quietLogger())
	rec := &ackRecorder{}

	l.dispatch(rec, socketmode.Event{Type: socketmode.EventTypeConnected}, Handlers{})
	if len(rec.acked) != 0 {
		t.Fatalf("acked %v, want nothing", rec.acked)
	}
}

// A link button raises an interaction Riggs does nothing with. It must still be
// delivered and acked, not dropped — the click that started this had no handler
// AND no log line, so nothing recorded that it had happened.
func TestALinkButtonClickIsStillDelivered(t *testing.T) {
	l := NewSocketListener(slack.Credentials{Profile: "riggs"}, quietLogger())
	rec := &ackRecorder{}

	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	cb.ActionCallback.BlockActions = []*slackgo.BlockAction{{ActionID: "pr_ask_open", BlockID: "o/r#7"}}

	// dispatch delivers on its own goroutine, so the delivery is awaited rather
	// than slept on: a shared counter read after a fixed pause is a data race,
	// and one the race detector fails the build over.
	delivered := make(chan slackgo.InteractionCallback, 1)
	l.dispatch(rec, socketmode.Event{
		Type: socketmode.EventTypeInteractive, Data: cb,
		Request: &socketmode.Request{EnvelopeID: "env"},
	}, Handlers{Interaction: func(cb slackgo.InteractionCallback) { delivered <- cb }})

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("the link-button click was never delivered")
	}
	if len(rec.acked) != 1 {
		t.Fatalf("acked %v, want the click acknowledged", rec.acked)
	}
}

// The Home tab's counterpart: app_home_opened arrives as an Events API event,
// not an interaction, and has to reach a different handler.
func TestAppHomeOpenedIsDelivered(t *testing.T) {
	l := NewSocketListener(slack.Credentials{Profile: "riggs"}, quietLogger())
	rec := &ackRecorder{}

	opened := make(chan string, 1)
	l.dispatch(rec, appHomeEvent("U0ADMIN", "home"), Handlers{
		AppHomeOpened: func(user string) { opened <- user },
	})

	select {
	case user := <-opened:
		if user != "U0ADMIN" {
			t.Fatalf("delivered user %q", user)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app_home_opened was never delivered")
	}
	if len(rec.acked) != 1 {
		t.Fatalf("acked %v, want the event acknowledged", rec.acked)
	}
}

// The same event fires when someone opens the app's *messages* tab, which is
// just a DM being read. Publishing a Home view for it is work nobody asked for.
func TestAppHomeOpenedOnTheMessagesTabIsIgnored(t *testing.T) {
	l := NewSocketListener(slack.Credentials{Profile: "riggs"}, quietLogger())
	rec := &ackRecorder{}

	opened := make(chan string, 1)
	l.dispatch(rec, appHomeEvent("U0ADMIN", "messages"), Handlers{
		AppHomeOpened: func(user string) { opened <- user },
	})

	select {
	case user := <-opened:
		t.Fatalf("published a Home view for the messages tab (user %q)", user)
	case <-time.After(100 * time.Millisecond):
	}
	// Ignored is not the same as unanswered: Slack is still owed an ack.
	if len(rec.acked) != 1 {
		t.Fatalf("acked %v, want the event acknowledged", rec.acked)
	}
}

// appHomeEvent builds the socket event Slack delivers for app_home_opened.
func appHomeEvent(user, tab string) socketmode.Event {
	return socketmode.Event{
		Type: socketmode.EventTypeEventsAPI,
		Data: slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Type: "app_home_opened",
				Data: &slackevents.AppHomeOpenedEvent{
					Type: "app_home_opened", User: user, Tab: tab,
				},
			},
		},
		Request: &socketmode.Request{EnvelopeID: "env-home"},
	}
}
