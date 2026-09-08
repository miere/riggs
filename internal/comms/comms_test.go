package comms

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/miere/riggs-mcp/internal/slack"
)

// reactor records every call in order, which is the whole point: the
// specification is about ORDER, not about the final set.
type reactor struct {
	mu     sync.Mutex
	calls  []string
	addErr error
	rmErr  error
}

func (r *reactor) AddReaction(_ context.Context, _ slack.Target, ref slack.Ref, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "+"+name+"@"+ref.TS)
	return r.addErr
}

func (r *reactor) RemoveReaction(_ context.Context, _ slack.Target, ref slack.Ref, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "-"+name+"@"+ref.TS)
	return r.rmErr
}

func (r *reactor) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// emojis is the default set, spelled here rather than imported from config: this
// package must not know where the names come from, and a test that reached into
// config would be asserting the wiring instead of the machine.
type emojis struct{ ack, disregard, success, warning string }

func defaults() emojis {
	return emojis{ack: "saluting_face", disregard: "zipper_mouth_face",
		success: "white_check_mark", warning: "warning"}
}

func (e emojis) Emoji(s State) string {
	switch s {
	case Acknowledgement:
		return e.ack
	case Disregard:
		return e.disregard
	case Success:
		return e.success
	case Warning:
		return e.warning
	}
	return ""
}

func (e emojis) All() []string { return []string{e.ack, e.disregard, e.success, e.warning} }

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

var digest = slack.Ref{Channel: "C1", TS: "1700.1"}

// The specification is explicit about the order and it is not the obvious one:
// the new emoji goes on FIRST, and only then does the old one come off. There
// is a moment either way; "briefly both" reads as a machine working, and
// "briefly neither" reads as a machine that forgot.
func TestTheNewEmojiGoesOnBeforeTheOldOnesComeOff(t *testing.T) {
	r := &reactor{}
	New(r, defaults(), quiet()).Apply(context.Background(), slack.Target{}, digest, Success)

	got := r.seen()
	if len(got) == 0 || got[0] != "+white_check_mark@1700.1" {
		t.Fatalf("calls = %v, want the tick added first", got)
	}
	for _, later := range got[1:] {
		if later[0] != '-' {
			t.Fatalf("calls = %v, want every call after the add to be a removal", got)
		}
	}
}

// Every OTHER state's emoji is removed, and the incoming one is not — removing
// what was just added would leave the message bare.
func TestTheIncomingEmojiIsNeverRemoved(t *testing.T) {
	r := &reactor{}
	New(r, defaults(), quiet()).Apply(context.Background(), slack.Target{}, digest, Acknowledgement)

	want := map[string]bool{
		"+saluting_face@1700.1":     true,
		"-zipper_mouth_face@1700.1": true,
		"-white_check_mark@1700.1":  true,
		"-warning@1700.1":           true,
	}
	got := r.seen()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %d of them", got, len(want))
	}
	for _, call := range got {
		if !want[call] {
			t.Fatalf("unexpected call %q in %v", call, got)
		}
	}
}

// A failed add stops the transition. The message still carries whatever it
// carried before, which is a truer picture than one with the old state stripped
// and no new one put on.
func TestAFailedAddSkipsTheRemovals(t *testing.T) {
	r := &reactor{addErr: errors.New("missing_scope")}
	New(r, defaults(), quiet()).Apply(context.Background(), slack.Target{}, digest, Success)

	got := r.seen()
	if len(got) != 1 {
		t.Fatalf("calls = %v, want the failed add and nothing else", got)
	}
}

// A failed removal is untidy, not fatal: the new state is already on the
// message and the click it describes has already happened.
func TestAFailedRemovalIsSwallowed(t *testing.T) {
	r := &reactor{rmErr: errors.New("channel_not_found")}
	// Apply returns nothing at all, so "it did not panic and it kept going" is
	// the assertion; the call count is what proves it did not stop at the first
	// failure.
	New(r, defaults(), quiet()).Apply(context.Background(), slack.Target{}, digest, Warning)

	if got := r.seen(); len(got) != 4 {
		t.Fatalf("calls = %v, want the add and all three removals attempted", got)
	}
}

// A Home tab click and a modal submission have no message behind them. That is
// not a failure to react — there is nothing there to react to.
func TestASurfaceWithNoMessageIsNotReactedTo(t *testing.T) {
	for _, ref := range []slack.Ref{{}, {Channel: "C1"}, {TS: "1700.1"}} {
		r := &reactor{}
		New(r, defaults(), quiet()).Apply(context.Background(), slack.Target{}, ref, Success)
		if got := r.seen(); len(got) != 0 {
			t.Fatalf("ref %+v produced %v, want nothing", ref, got)
		}
	}
}

// An app installed before `reactions:write` existed must still route every
// click. A missing capability disables a feature; it never fails the boot (§6).
func TestNoReactorIsANoOp(t *testing.T) {
	New(nil, defaults(), quiet()).Apply(context.Background(), slack.Target{}, digest, Success)
	r := &reactor{}
	New(r, nil, quiet()).Apply(context.Background(), slack.Target{}, digest, Success)
	if got := r.seen(); len(got) != 0 {
		t.Fatalf("calls = %v, want nothing", got)
	}
}

// A state whose emoji is configured to nothing is skipped rather than sent to
// Slack as an empty name — which would fail per click, forever, with a message
// naming no setting.
func TestAnUnconfiguredEmojiIsSkipped(t *testing.T) {
	r := &reactor{}
	e := defaults()
	e.success = ""
	New(r, e, quiet()).Apply(context.Background(), slack.Target{}, digest, Success)
	if got := r.seen(); len(got) != 0 {
		t.Fatalf("calls = %v, want nothing", got)
	}
}

// The empty member of the set is not sent as a removal either, for the same
// reason: `reactions.remove` with no name is a guaranteed error per click.
func TestAnEmptyEmojiIsNotRemoved(t *testing.T) {
	r := &reactor{}
	e := defaults()
	e.disregard = ""
	New(r, e, quiet()).Apply(context.Background(), slack.Target{}, digest, Success)
	for _, call := range r.seen() {
		if call == "-@1700.1" {
			t.Fatalf("an empty emoji was sent to Slack: %v", r.seen())
		}
	}
}

// A digest is ONE message carrying many rows, so two people approving two
// different pull requests in it are two transitions on the same message.
//
// Interleaved, the second click's removal can run before the first click's add,
// leaving an acknowledgement nothing will ever clear. Serialised, the set is
// always some transition's intended outcome — which is what this asserts: every
// add is followed by its own removals before the next add begins.
func TestTransitionsOnOneMessageAreSerialised(t *testing.T) {
	r := &reactor{}
	rep := New(r, defaults(), quiet())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			state := Success
			if n%2 == 0 {
				state = Warning
			}
			rep.Apply(context.Background(), slack.Target{}, digest, state)
		}(i)
	}
	wg.Wait()

	// Each transition is one add and three removals. Any interleaving shows up
	// as two adds with no removals between them.
	got := r.seen()
	if len(got) != 8*4 {
		t.Fatalf("calls = %d, want %d", len(got), 8*4)
	}
	for i := 0; i < len(got); i += 4 {
		if got[i][0] != '+' {
			t.Fatalf("call %d = %q, want a transition to start with an add: %v", i, got[i], got)
		}
		for j := 1; j < 4; j++ {
			if got[i+j][0] != '-' {
				t.Fatalf("call %d = %q, want a removal inside the transition: %v", i+j, got[i+j], got)
			}
		}
	}
}

// Two different messages must NOT serialise against each other: a busy channel
// would otherwise queue every click behind the slowest one.
func TestDifferentMessagesDoNotBlockEachOther(t *testing.T) {
	r := &reactor{}
	rep := New(r, defaults(), quiet())

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rep.Apply(context.Background(), slack.Target{},
				slack.Ref{Channel: "C1", TS: fmt.Sprintf("170%d.1", n)}, Success)
		}(i)
	}
	wg.Wait()

	// The per-message locks are dropped once nobody holds them, so a daemon
	// running for weeks does not accumulate one per message it ever answered.
	rep.mu.Lock()
	left := len(rep.inflight)
	rep.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d message locks left behind", left)
	}
}

// Acknowledgement is the only state something else replaces. The distinction is
// the whole design, so it is stated rather than inferred.
func TestOnlyAcknowledgementIsTransient(t *testing.T) {
	if Acknowledgement.Terminal() {
		t.Error("acknowledgement is terminal")
	}
	for _, s := range []State{Disregard, Success, Warning} {
		if !s.Terminal() {
			t.Errorf("%s is not terminal", s)
		}
	}
}

// The two failures an admin can actually fix are named, because neither says
// how on its own.
func TestHintNamesTheFixableFailures(t *testing.T) {
	if got := Hint(errors.New("slack: reactions.add failed: missing_scope")); got == "" {
		t.Error("a missing scope got no hint")
	}
	if got := Hint(slack.ErrInvalidEmoji); got == "" {
		t.Error("an unknown emoji got no hint")
	}
	if got := Hint(errors.New("slack: reactions.add: connection refused")); got != "" {
		t.Errorf("a network failure got the hint %q", got)
	}
	if got := Hint(nil); got != "" {
		t.Errorf("no error got the hint %q", got)
	}
}
