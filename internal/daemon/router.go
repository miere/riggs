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

// Route dispatches in to its handler.
//
// matched is false when nothing is registered for the control. That is the
// ordinary case for a link button (which raises no interaction at all) and for
// a control Riggs has retired but whose message is still in the channel, so it
// is reported rather than treated as an error.
func (r *Router) Route(ctx context.Context, in slack.Interaction) (matched bool, err error) {
	h, ok := r.routes[route{actionID: in.ActionID, intent: in.Intent}]
	if !ok {
		return false, nil
	}
	return true, h.Handle(ctx, in)
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
