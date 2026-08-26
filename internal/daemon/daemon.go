package daemon

import (
	"context"
	"log/slog"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/riggs-mcp/internal/slack"
)

// Listener delivers interaction callbacks until its context is cancelled.
//
// It is an interface so the daemon's routing, logging and error handling can be
// tested without a websocket. SocketListener is the live implementation.
type Listener interface {
	Listen(ctx context.Context, deliver func(slackgo.InteractionCallback)) error
}

// Daemon holds the connection open and routes what arrives on it.
type Daemon struct {
	listener Listener
	router   *Router
	logger   *slog.Logger
	profile  string
}

// New builds a daemon over a listener and a routing table.
func New(listener Listener, router *Router, profile string, logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.Default()
	}
	return &Daemon{listener: listener, router: router, logger: logger, profile: profile}
}

// Run connects and serves until ctx is cancelled.
//
// A handler error is logged and swallowed. One failed click must not take the
// connection down with it: the next click is a separate piece of work, and a
// daemon that exits on the first GitHub hiccup would need a human to notice and
// restart it before any button worked again.
func (d *Daemon) Run(ctx context.Context) error {
	d.logger.Info("riggs daemon starting",
		"profile", d.profile, "routes", d.router.Describe())

	return d.listener.Listen(ctx, func(cb slackgo.InteractionCallback) {
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
	})
}
