package daemon

import (
	"context"
	"log/slog"

	slackgo "github.com/slack-go/slack"

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

// Daemon holds the connection open and routes what arrives on it.
type Daemon struct {
	listener Listener
	router   *Router
	logger   *slog.Logger
	profile  string
	home     AppHome
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

// Run connects and serves until ctx is cancelled.
//
// A handler error is logged and swallowed. One failed click must not take the
// connection down with it: the next click is a separate piece of work, and a
// daemon that exits on the first GitHub hiccup would need a human to notice and
// restart it before any button worked again.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("riggs daemon starting",
		"profile", d.profile, "routes", d.router.Describe(), "app_home", d.home != nil)

	return d.listener.Listen(ctx, Handlers{
		Interaction:   func(cb slackgo.InteractionCallback) { d.handleInteraction(ctx, cb) },
		AppHomeOpened: func(userID string) { d.handleAppHomeOpened(ctx, userID) },
	})
}

// handleInteraction routes one click.
func (d *Daemon) handleInteraction(ctx context.Context, cb slackgo.InteractionCallback) {
	in, ok := slack.DecodeInteraction(cb)
	if !ok {
		d.logger.Debug("ignoring non-block-action callback", "type", string(cb.Type))
		return
	}
	matched, err := d.router.Route(ctx, in)
	switch {
	case err != nil:
		d.logger.Error("interaction handler failed",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item,
			"user", in.UserID, "error", err)
	case !matched:
		d.logger.Info("no handler for interaction",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item)
	default:
		d.logger.Info("interaction handled",
			"action_id", in.ActionID, "intent", in.Intent, "item", in.Item,
			"user", in.UserID)
	}
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
