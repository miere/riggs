package daemon

import (
	"context"
	"errors"
	"log/slog"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/riggs-mcp/internal/comms"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Handlers is what a Listener delivers into.
//
// It is a struct rather than a second callback argument because the two things
// arriving on this socket are genuinely different kinds. An interaction is a
// click on a message Riggs posted, routed by (action_id, intent); app_home
// _opened is an Events API subscription with no control and no message behind
// it — a user simply looked at the app. Passing them as one callback would mean
// inventing an Interaction for the second, and the router would have nothing to
// match it on.
//
// A nil field means "not handled here". The listener still acknowledges the
// event: Slack is owed an answer whether or not this process cares.
type Handlers struct {
	// Interaction receives block-action callbacks.
	Interaction func(slackgo.InteractionCallback)
	// AppHomeOpened receives the user id of someone opening the Home tab.
	AppHomeOpened func(userID string)
}

// Listener delivers Slack events until its context is cancelled.
//
// It is an interface so the daemon's routing, logging and error handling can be
// tested without a websocket. SocketListener is the live implementation.
type Listener interface {
	Listen(ctx context.Context, h Handlers) error
}

// AppHome publishes a user's Home tab.
type AppHome interface {
	// Publish renders the Home view for userID and publishes it, reporting
	// whether a Slack call was actually made — a view identical to the one
	// that user was last sent is skipped.
	//
	// The bool exists for the log line. Without it the daemon reported "app
	// home published" on every glance at the app, including the ones it had
	// deliberately skipped, which is a log that lies about a network call.
	Publish(ctx context.Context, userID string) (published bool, err error)
}

// Reporter tells whoever clicked that their click failed.
//
// It takes the whole interaction rather than a channel and a user because where
// a failure belongs depends on where the click came from, and only the
// implementation should decide that: a digest click has a channel and a thread,
// a Home tab click has neither.
//
// The wording is the implementation's too. This package knows a handler
// returned an error; it does not know how Riggs talks.
type Reporter interface {
	ReportFailure(ctx context.Context, in slack.Interaction, cause error) error
}

// States reports what Riggs is doing with a click, on the message the click
// came from.
//
// It is an interface here for the same reason Reporter is: this package knows
// that a click was recognised, ran and finished — it does not know that
// "recognised" is drawn as a saluting face, or that the emoji is configurable,
// or that two clicks on one digest have to be serialised. internal/comms knows
// all three.
//
// Apply returns nothing on purpose. Decorating a message is not the work, and a
// decoration that fails must not turn a successful approval into a failed
// click.
type States interface {
	Apply(ctx context.Context, target slack.Target, ref slack.Ref, s comms.State)
}

// Daemon holds the connection open and routes what arrives on it.
type Daemon struct {
	listener Listener
	router   *Router
	logger   *slog.Logger
	profile  string
	home     AppHome
	reporter Reporter
	states   States
	// target is the credentials reactions are placed with: this daemon's own
	// app, since a click is only ever delivered to the app that posted the
	// message it came from.
	target slack.Target
}

// New builds a daemon over a listener and a routing table.
func New(listener Listener, router *Router, profile string, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	return &Daemon{listener: listener, router: router, logger: logger, profile: profile}
}

// WithAppHome registers the Home tab publisher.
//
// Optional, and separate from New, because the Home tab is not what this daemon
// is for: a Riggs whose Slack app has no Home surface enabled still routes every
// click it is asked to. Left unset, app_home_opened is acknowledged and logged.
func (d *Daemon) WithAppHome(home AppHome) *Daemon {
	d.home = home
	return d
}

// WithReporter registers the failure reporter.
//
// Optional, like WithAppHome, and for the same reason: a daemon with none still
// routes every click it is asked to. Left unset, a failed click reports to the
// log alone — which is the behaviour this exists to end.
func (d *Daemon) WithReporter(r Reporter) *Daemon {
	d.reporter = r
	return d
}

// WithStates registers the communication-state machine and the credentials it
// reacts with.
//
// Optional, like the two above, and for the same reason: a daemon with none
// still routes every click, it simply says nothing about doing so. That is also
// what an app installed before `reactions:write` existed degrades to, rather
// than every button failing.
func (d *Daemon) WithStates(s States, target slack.Target) *Daemon {
	d.states, d.target = s, target
	return d
}

// Run connects and serves until ctx is cancelled.
//
// A handler error is logged and swallowed. One failed click must not take the
// connection down with it: the next click is a separate piece of work, and a
// daemon that exits on the first GitHub hiccup would need a human to notice and
// restart it before any button worked again.
//
// Swallowed is not the same as silent, and for a long time this conflated them.
// A handler that failed said so in the log and nowhere else, so from Slack a
// broken button and an ignored one looked identical — which is exactly how a
// card that Slack was rejecting with invalid_blocks went unnoticed. The error
// is still swallowed; it is now also reported to whoever clicked.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("riggs daemon starting",
		"profile", d.profile, "routes", d.router.Describe(), "app_home", d.home != nil)

	return d.listener.Listen(ctx, Handlers{
		Interaction:   func(cb slackgo.InteractionCallback) { d.handleInteraction(ctx, cb) },
		AppHomeOpened: func(userID string) { d.handleAppHomeOpened(ctx, userID) },
	})
}

// handleInteraction routes one click, and says so on the message it came from.
//
// The communication states are driven from HERE rather than from the eight
// handlers below, because here is the only place that knows all four answers
// without being told: whether the control is one Riggs answers, whether the
// work has started, and whether it finished or failed. Pushing that into the
// handlers would mean eight copies of the same four calls, and the first one
// somebody forgot would be a click that silently never acknowledged.
//
// The acknowledgement goes on BEFORE the handler runs, not after. Approving and
// merging a pull request takes several GitHub calls with retries; the whole
// point of the transient state is that it covers exactly that gap, and a
// saluting face applied afterwards would appear at the same moment as the tick
// that replaces it.
func (d *Daemon) handleInteraction(ctx context.Context, cb slackgo.InteractionCallback) {
	in, ok := slack.DecodeInteraction(cb)
	if !ok {
		d.logger.Debug("ignoring non-block-action callback", "type", string(cb.Type))
		return
	}

	// Peeked before dispatching, so the acknowledgement can be applied to a
	// control that is about to be handled and withheld from one that is not.
	// The alternative — acknowledge everything, then correct it — puts a
	// saluting face on a message for a click nobody was ever going to act on.
	switch d.router.Lookup(in) {
	case Handled:
		d.apply(ctx, in, comms.Acknowledgement)
	case Unrouted:
		// Terminal, and applied without dispatching: there is nothing to run.
		d.apply(ctx, in, comms.Disregard)
		d.logger.Info("no handler for interaction",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item)
		return
	case Ignored:
		// No state at all. A link button is not a task Riggs declined; Slack
		// opened the URL and the interaction is a formality (§7b).
		d.logger.Debug("interaction deliberately not acted on",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item)
		return
	}

	_, err := d.router.Route(ctx, in)
	if err != nil {
		d.apply(ctx, in, comms.Warning)
		d.logger.Error("interaction handler failed",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item,
			"user", in.UserID, "error", err)
		d.report(ctx, in, err)
		return
	}
	d.apply(ctx, in, comms.Success)
	d.logger.Info("interaction handled",
		"action_id", in.ActionID, "intent", in.Intent, "item", in.Item,
		"user", in.UserID)
}

// apply moves the clicked message into a state, when a state machine is wired.
//
// The message is the one the control is attached to. A Home tab click and a
// modal submission carry neither a channel nor a ts, and comms treats that as
// "nothing to react to" rather than as a failure — those surfaces are the
// admin's own and report their outcomes by DM.
func (d *Daemon) apply(ctx context.Context, in slack.Interaction, s comms.State) {
	if d.states == nil {
		return
	}
	target := d.target
	target.Channel = in.Channel
	d.states.Apply(ctx, target, slack.Ref{Channel: in.Channel, TS: in.MessageTS}, s)
}

// report shows a failed click's cause to the person who made it.
//
// A handler that has already reported the failure itself is left alone. Some of
// them narrate into the thread the click came from — that is a better message
// than this one can write, because the handler knows what it was attempting —
// and a click that answers twice is its own kind of broken. The signal is an
// error exposing `Reported() bool`, which is a plain interface assertion rather
// than a shared sentinel so no domain package has to import this one.
//
// A failure to report is logged and goes no further. There is nothing sensible
// left to do about a Slack call that fails while explaining a Slack call that
// failed, and recursing into it would be worse.
func (d *Daemon) report(ctx context.Context, in slack.Interaction, cause error) {
	if d.reporter == nil || alreadyReported(cause) {
		return
	}
	if err := d.reporter.ReportFailure(ctx, in, cause); err != nil {
		d.logger.Error("could not tell the user their click failed",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item,
			"user", in.UserID, "error", err, "cause", cause)
	}
}

// alreadyReported reports whether err has been shown to the user by whoever
// produced it.
func alreadyReported(err error) bool {
	var r interface{ Reported() bool }
	return errors.As(err, &r) && r.Reported()
}

// handleAppHomeOpened republishes the Home tab for whoever opened it.
//
// A failure is logged and dropped, like a failed click: the user sees whatever
// was last published, which is a stale panel rather than a broken one, and the
// next open tries again.
func (d *Daemon) handleAppHomeOpened(ctx context.Context, userID string) {
	if d.home == nil {
		d.logger.Debug("app home opened, but no publisher is wired", "user", userID)
		return
	}
	published, err := d.home.Publish(ctx, userID)
	if err != nil {
		d.logger.Error("publishing the app home failed", "user", userID, "error", err)
		return
	}
	if !published {
		d.logger.Debug("app home unchanged, not republished", "user", userID)
		return
	}
	d.logger.Info("app home published", "user", userID)
}
