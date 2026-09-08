// Package comms is how Riggs says what it is doing, without saying anything.
//
// Riggs used to narrate a click into the thread of the message it came from:
// "Approving PR — verifying with GitHub…", and then "Approved". Two messages,
// both notifications, both permanent, under a digest that already said
// everything either of them added. A queue you have learnt to skip is worse
// than one that says its piece once (§8c), and a thread of Riggs talking to
// itself is how a reader learns to skip a thread.
//
// So the running commentary becomes a REACTION on the message, and the thread
// is reserved for the one thing a reaction cannot carry: why something failed.
// Four states, one of them temporary:
//
//	Acknowledgement  Riggs has picked this up.       transient
//	Disregard        Riggs will not act on this.     terminal
//	Success          it is done.                     terminal
//	Warning          it failed, or half-worked.      terminal
//
// A transition ADDS the new emoji before removing the old ones, never the other
// way round. There is a moment in every transition where the message carries
// both, and a moment where it would carry neither; only one of those can be
// chosen, and "briefly both" reads as a machine working while "briefly neither"
// reads as a machine that forgot.
package comms

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/miere/riggs-mcp/internal/slack"
)

// State is one communication state.
type State string

const (
	// Acknowledgement means Riggs has recognised the task and started it. It is
	// the only non-terminal state: something always replaces it.
	Acknowledgement State = "acknowledgement"
	// Disregard means Riggs was handed something it does not answer. Terminal.
	Disregard State = "disregard"
	// Success means the task finished. Terminal.
	Success State = "success"
	// Warning means the task failed or finished badly. Terminal. The reason is
	// in the thread — this says only that there is one to read.
	Warning State = "warning"
)

// Terminal reports whether s is an end state.
//
// Nothing branches on it today; it is here because the distinction is the whole
// design, and a reader should find it stated rather than inferred from which
// states happen to be applied last.
func (s State) Terminal() bool { return s != Acknowledgement }

// Emojis supplies the shortcode for each state, and the whole set.
//
// It is an interface rather than four strings captured at wiring time, because
// the Customisation modal edits these while the daemon is running. A captured
// set would keep reacting with the old emoji until the next restart and —
// worse — would fail to REMOVE an acknowledgement it had placed with the new
// one, leaving a glyph nothing will ever clear.
type Emojis interface {
	// Emoji is the shortcode for one state, without colons.
	Emoji(s State) string
	// All is every emoji Riggs may currently have placed. It is read as one
	// call so a Customisation save landing mid-transition cannot produce a set
	// that is half old and half new.
	All() []string
}

// Reporter is the state machine, and holds the per-message serialisation that
// makes a transition atomic from Slack's point of view.
type Reporter struct {
	reactor slack.Reactor
	emojis  Emojis
	logger  *slog.Logger

	// mu guards inflight.
	mu sync.Mutex
	// inflight holds one lock per message currently being transitioned, with
	// the number of goroutines that still need it.
	//
	// A digest is ONE message carrying many rows, so two people approving two
	// different pull requests in the same digest are two transitions on the
	// same message. Without this their adds and removes interleave, and the
	// message can be left carrying an acknowledgement nothing will clear — the
	// second click's remove having run before the first click's add.
	//
	// Serialising them does not make one message mean two things at once; it
	// cannot, because a message has one reaction set. What it guarantees is
	// that the set is always some transition's intended OUTCOME rather than a
	// mixture of two, and that the last transition to finish is the one
	// showing.
	inflight map[string]*entry
}

// entry is one message's lock and its waiter count.
type entry struct {
	mu      sync.Mutex
	waiters int
}

// New builds a Reporter.
//
// A nil reactor or a nil emoji source disables it entirely: every Apply becomes
// a no-op. That is the rule the whole binary follows (§6) — a missing
// capability disables a feature and never fails the boot — and a daemon whose
// token lacks `reactions:write` must still route every click.
func New(reactor slack.Reactor, emojis Emojis, logger *slog.Logger) *Reporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reporter{reactor: reactor, emojis: emojis, logger: logger,
		inflight: map[string]*entry{}}
}

// Apply moves the message at ref into state s.
//
// The order is fixed and is the specification: the new emoji goes on FIRST,
// then every other state's emoji comes off.
//
// A failure is logged and swallowed, never returned. This is Riggs describing
// its own work; it is not the work. An approval that lands and then cannot be
// decorated is still an approval, and turning a missing `reactions:write` scope
// into a failed click would break every button on the way to fixing nothing.
func (r *Reporter) Apply(ctx context.Context, target slack.Target, ref slack.Ref, s State) {
	if !r.enabled() {
		return
	}
	// A Home tab click and a modal submission have no message behind them.
	// There is nothing to react to and nothing missing: that surface is the
	// admin's own, and its outcomes reach them by DM.
	if ref.Channel == "" || ref.TS == "" {
		return
	}
	incoming := r.emojis.Emoji(s)
	if incoming == "" {
		r.logger.Warn("no emoji configured for a communication state", "state", string(s))
		return
	}

	defer r.lock(ref)()

	if err := r.reactor.AddReaction(ctx, target, ref, incoming); err != nil {
		// The removals are skipped after a failed add. Adding failed, so the
		// message still carries whatever it carried before — which is a truer
		// picture than one with the old state stripped and no new one put on.
		r.logger.Error("could not react to a message",
			"state", string(s), "emoji", incoming, "channel", ref.Channel, "ts", ref.TS,
			"error", err, "hint", Hint(err))
		return
	}

	for _, emoji := range r.emojis.All() {
		if emoji == incoming || emoji == "" {
			continue
		}
		// Blind, rather than reading the message's reactions first. Slack scopes
		// `reactions.remove` to the CALLING user's own reaction and answers
		// `no_reaction` when there is none — which the client swallows — so the
		// worst case is a couple of wasted calls, and the best case is one fewer
		// round trip and one fewer OAuth scope (`reactions:read`) to ask for.
		//
		// That scoping is also the safety property the design rests on: this
		// cannot remove a colleague's reaction, however the two happen to
		// overlap.
		if err := r.reactor.RemoveReaction(ctx, target, ref, emoji); err != nil {
			// Not fatal and not returned. A stale glyph beside a correct one is
			// untidy; failing the click over it would be worse.
			r.logger.Warn("could not clear a previous reaction",
				"emoji", emoji, "channel", ref.Channel, "ts", ref.TS, "error", err)
		}
	}
}

// enabled reports whether anything is wired up to react.
func (r *Reporter) enabled() bool {
	return r != nil && r.reactor != nil && r.emojis != nil
}

// lock serialises transitions on one message and returns the release.
//
// The entry is dropped as soon as nobody needs it, so a daemon running for
// weeks does not accumulate one mutex per message it has ever answered. It is
// reference-counted rather than deleted on unlock: the entry has to outlive
// anybody still queued on it, or two goroutines end up serialising on two
// different mutexes for the same message — which is the exact bug this exists
// to prevent.
func (r *Reporter) lock(ref slack.Ref) func() {
	key := ref.Channel + "/" + ref.TS

	r.mu.Lock()
	e, held := r.inflight[key]
	if !held {
		e = &entry{}
		r.inflight[key] = e
	}
	e.waiters++
	r.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		r.mu.Lock()
		e.waiters--
		if e.waiters == 0 {
			delete(r.inflight, key)
		}
		r.mu.Unlock()
	}
}

// Hint turns the two reaction failures an admin can actually fix into a
// sentence saying how.
//
// Neither is repairable from inside this process, and neither says so on its
// own: an operator reading "slack: reactions.add failed: missing_scope" has to
// already know that the app must be re-authorised to act on it.
func Hint(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, slack.ErrInvalidEmoji):
		return "no emoji by that name in this workspace; fix it under Customisation on the Home tab"
	case missingScope(err):
		return "the Slack app is missing the reactions:write scope; re-install it at api.slack.com"
	}
	return ""
}

// missingScope recognises Slack's scope refusal by its wording, which is the
// only place it appears: the code arrives inside the `ok:false` envelope's
// error string and nothing structured comes back beside it.
func missingScope(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "missing_scope") || strings.Contains(text, "not_allowed_token_type")
}
