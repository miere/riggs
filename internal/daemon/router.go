// Package daemon is Riggs' inbound half: it holds a Socket Mode connection
// open, decodes the interactions Slack pushes down it, and dispatches them to
// the handler registered for that control.
//
// This inverts the original design. Riggs began as a pure callee — Murtaugh
// owned the Slack connection, received every event, and invoked Riggs through a
// workflow rule to act on one. That indirection only made sense while Riggs
// posted as Murtaugh's app. Once Riggs owns its own app, the interactions on
// its own messages are delivered to it directly, and routing them back out
// through another process's config would be a detour with nothing at the end of
// it.
//
// The scheduler stays where it was. Murtaugh still owns *when* the reconcile
// runs and still invokes the CLI to run it; this package only owns *reactions*.
// So the two processes write the same ledger, which is what its WAL and busy
// timeout were chosen for.
package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/miere/riggs-mcp/internal/slack"
)

// Handler acts on one decoded interaction.
type Handler interface {
	Handle(ctx context.Context, in slack.Interaction) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, in slack.Interaction) error

// Handle satisfies Handler.
func (f HandlerFunc) Handle(ctx context.Context, in slack.Interaction) error { return f(ctx, in) }

// route is the dispatch key: which control, and which intent on it.
type route struct {
	actionID string
	intent   string
}

// Outcome is what routing one interaction amounted to.
//
// Three values rather than a bool, because the third one now MEANS something.
// Riggs reacts to a click with a communication state (internal/comms), and
// "nothing is registered for this" and "this is registered as nothing to do"
// deserve opposite answers: the first is a control Riggs no longer understands
// and is worth a disregard; the second is the link button on every digest row,
// and putting a zipper-mouth on the digest every time somebody opens a pull
// request in their browser would be absurd.
//
// Before the states existed the two were indistinguishable and it cost nothing,
// which is why they were one branch.
type Outcome int

const (
	// Unrouted means nothing is registered for the control. A retired button
	// whose message is still in the channel, or a Home tab published by an
	// older build.
	Unrouted Outcome = iota
	// Handled means a handler ran. Whether it succeeded is the returned error's
	// business.
	Handled
	// Ignored means the pair is registered as deliberately not acted on.
	Ignored
)

// Router maps (action_id, intent) onto handlers.
//
// Matching is exact, on both halves. That is inherited from the workflow rules
// this replaces, and it is why every option's value is a bare token
// ("approve_merge") with the per-row reference kept out of it: a value that
// varied per row could not be matched by a table.
type Router struct {
	routes map[route]Handler
}

// NewRouter builds an empty router.
func NewRouter() *Router { return &Router{routes: map[route]Handler{}} }

// Handle registers h for one control and intent. Registering the same pair
// twice is a programming error and panics at wiring time rather than silently
// dropping one of them — the composition root runs this before the daemon
// serves anything.
func (r *Router) Handle(actionID, intent string, h Handler) {
	k := route{actionID: actionID, intent: intent}
	if _, dup := r.routes[k]; dup {
		panic(fmt.Sprintf("daemon: duplicate route %s/%s", actionID, intent))
	}
	r.routes[k] = h
}

// Ignore registers a control as deliberately not acted on.
//
// It replaces a comment. `open_browser` was simply left unregistered, with a
// note saying a handler that returns nil is worse than the router's own "no
// handler" log line — and that was true right up until an unregistered pair
// started meaning "Riggs does not understand this", which a link button is not.
//
// Registering it costs one table entry and buys the distinction: the option is
// declared, so a future reader can see that nothing acting on it is a decision
// rather than an omission, and the daemon can tell it apart from a genuinely
// stray click.
func (r *Router) Ignore(actionID, intent string) {
	r.Handle(actionID, intent, ignored{})
}

// ignored is the sentinel Ignore registers. It is a distinct type rather than a
// no-op HandlerFunc so Route can recognise it: two functions are not comparable,
// and "did a handler run" has to be answerable without calling it.
type ignored struct{}

// Handle satisfies Handler and does nothing, which is the point.
func (ignored) Handle(context.Context, slack.Interaction) error { return nil }

// Lookup reports what routing in WOULD amount to, without running anything.
//
// It exists because the acknowledgement has to be applied before the handler
// runs and must not be applied to a control that has no handler — so the
// decision is needed a moment earlier than Route can give it. Splitting the
// table read from the dispatch is the whole of it: both read the same map with
// the same key, and Route is the one that acts.
func (r *Router) Lookup(in slack.Interaction) Outcome {
	h, ok := r.routes[route{actionID: in.ActionID, intent: in.Intent}]
	switch {
	case !ok:
		return Unrouted
	case isIgnored(h):
		return Ignored
	default:
		return Handled
	}
}

// isIgnored reports whether h is the sentinel Ignore registers.
func isIgnored(h Handler) bool {
	_, yes := h.(ignored)
	return yes
}

// Route dispatches in to its handler and reports what that amounted to.
//
// Unrouted is the ordinary case for a control Riggs has retired whose message
// is still in the channel, so it is reported rather than treated as an error.
func (r *Router) Route(ctx context.Context, in slack.Interaction) (Outcome, error) {
	outcome := r.Lookup(in)
	if outcome != Handled {
		return outcome, nil
	}
	return Handled, r.routes[route{actionID: in.ActionID, intent: in.Intent}].Handle(ctx, in)
}

// Routes lists the registered pairs as "action_id/intent", sorted. It exists
// for the daemon's startup log: an operator staring at a button that does
// nothing wants to see what the process believes it can handle.
func (r *Router) Routes() []string {
	out := make([]string, 0, len(r.routes))
	for k := range r.routes {
		out = append(out, k.actionID+"/"+k.intent)
	}
	sort.Strings(out)
	return out
}

// Describe renders the routes on one line for that log.
func (r *Router) Describe() string {
	routes := r.Routes()
	if len(routes) == 0 {
		return "no routes registered"
	}
	return strings.Join(routes, ", ")
}
