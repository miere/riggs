package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/riggs-mcp/internal/comms"
	"github.com/miere/riggs-mcp/internal/slack"
)

// stateLog records the transitions a click produced, in order. Order is the
// assertion in every test here: an acknowledgement that lands after the outcome
// it was supposed to precede is the bug this whole mechanism can have.
type stateLog struct {
	mu   sync.Mutex
	seen []comms.State
	refs []slack.Ref
}

func (s *stateLog) Apply(_ context.Context, _ slack.Target, ref slack.Ref, st comms.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, st)
	s.refs = append(s.refs, ref)
}

func (s *stateLog) states() []comms.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]comms.State(nil), s.seen...)
}

func quietDaemon(router *Router, states States) *Daemon {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(nil, router, "riggs", logger).
		WithStates(states, slack.Target{Profile: "riggs", BotToken: "xoxb"})
}

func assertStates(t *testing.T, got []comms.State, want ...comms.State) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("states = %v, want %v", got, want)
		}
	}
}

// A handled click acknowledges FIRST and reports the outcome after.
//
// The order is the point. Approving and merging takes several GitHub calls with
// retries, and the transient state exists to cover exactly that gap — an
// acknowledgement applied afterwards would appear at the same moment as the tick
// replacing it, which is the same as not applying it at all.
func TestAHandledClickAcknowledgesThenSucceeds(t *testing.T) {
	r := NewRouter()
	var order []string
	r.Handle("pr_overflow", "approve_merge", HandlerFunc(func(context.Context, slack.Interaction) error {
		order = append(order, "handler")
		return nil
	}))
	log := &stateLog{}
	quietDaemon(r, log).handleInteraction(context.Background(),
		overflowCallback("pr_overflow", "approve_merge", "o/r#1"))

	assertStates(t, log.states(), comms.Acknowledgement, comms.Success)
	if len(order) != 1 {
		t.Fatalf("the handler ran %d times", len(order))
	}
}

// A handler that fails ends in warning, not success. The reason itself goes in
// the thread — a reaction can say that something went wrong and cannot say
// what.
func TestAFailedClickEndsInWarning(t *testing.T) {
	r := NewRouter()
	r.Handle("pr_overflow", "approve_merge", HandlerFunc(func(context.Context, slack.Interaction) error {
		return errors.New("branch is protected")
	}))
	log := &stateLog{}
	quietDaemon(r, log).handleInteraction(context.Background(),
		overflowCallback("pr_overflow", "approve_merge", "o/r#1"))

	assertStates(t, log.states(), comms.Acknowledgement, comms.Warning)
}

// A control nothing is registered for is a task Riggs will not tackle: a
// retired button whose message is still in the channel, or a Home tab published
// by an older build. It is terminal and there is no acknowledgement before it,
// because nothing was picked up.
func TestAnUnroutedClickIsDisregarded(t *testing.T) {
	log := &stateLog{}
	quietDaemon(NewRouter(), log).handleInteraction(context.Background(),
		overflowCallback("retired_menu", "some_intent", "o/r#1"))

	assertStates(t, log.states(), comms.Disregard)
}

// A link button gets NOTHING. Slack opened the URL itself; stamping a
// zipper-mouth on the digest every time somebody opens a pull request in their
// browser is not what that state is for.
//
// This is the entire reason Ignore exists as a distinct outcome from Unrouted.
func TestAnIgnoredClickGetsNoReactionAtAll(t *testing.T) {
	r := NewRouter()
	r.Ignore("pr_overflow", "open_browser")
	log := &stateLog{}
	quietDaemon(r, log).handleInteraction(context.Background(),
		overflowCallback("pr_overflow", "open_browser", "o/r#1"))

	if got := log.states(); len(got) != 0 {
		t.Fatalf("states = %v, want none", got)
	}
}

// The reaction goes on the message the control is attached to, which for a
// digest row is the digest itself — the only message a click carries back.
func TestTheReactionLandsOnTheClickedMessage(t *testing.T) {
	r := NewRouter()
	r.Handle("pr_overflow", "approve_merge",
		HandlerFunc(func(context.Context, slack.Interaction) error { return nil }))
	log := &stateLog{}
	cb := overflowCallback("pr_overflow", "approve_merge", "o/r#1")
	cb.Channel.ID = "C-digest"
	cb.Container.MessageTs = "1700.5"
	quietDaemon(r, log).handleInteraction(context.Background(), cb)

	log.mu.Lock()
	defer log.mu.Unlock()
	for _, ref := range log.refs {
		if ref.Channel != "C-digest" || ref.TS != "1700.5" {
			t.Fatalf("ref = %+v, want the clicked message", ref)
		}
	}
}

// A daemon with no state machine still routes every click. That is what an app
// installed before `reactions:write` existed degrades to, rather than every
// button failing (§6).
func TestADaemonWithNoStatesStillRoutes(t *testing.T) {
	r := NewRouter()
	ran := false
	r.Handle("pr_overflow", "approve_merge", HandlerFunc(func(context.Context, slack.Interaction) error {
		ran = true
		return nil
	}))
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	New(nil, r, "riggs", logger).handleInteraction(context.Background(),
		overflowCallback("pr_overflow", "approve_merge", "o/r#1"))

	if !ran {
		t.Fatal("the handler did not run without a state machine")
	}
}

// A modal submission has no message behind it, so the states are applied
// against an empty ref and comms drops them. The click is still routed.
func TestAViewSubmissionCarriesNoMessage(t *testing.T) {
	r := NewRouter()
	ran := false
	r.Handle("customisation", slack.ViewSubmitIntent, HandlerFunc(func(context.Context, slack.Interaction) error {
		ran = true
		return nil
	}))
	log := &stateLog{}
	quietDaemon(r, log).handleInteraction(context.Background(), slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{CallbackID: "customisation"},
		User: slackgo.User{ID: "U1"},
	})

	if !ran {
		t.Fatal("the submission was not routed")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, ref := range log.refs {
		if ref.Channel != "" || ref.TS != "" {
			t.Fatalf("ref = %+v, want an empty one for a modal", ref)
		}
	}
}
