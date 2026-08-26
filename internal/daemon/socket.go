package daemon

import (
	"context"
	"errors"
	"log/slog"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/riggs-mcp/internal/slack"
)

// SocketListener is the live Socket Mode connection.
//
// This is the one place Riggs uses slack-go, and it uses it inbound only.
// Outbound messages stay hand-built JSON through internal/slack: the block
// types Riggs renders are ordered structs precisely so the encoded bytes are
// stable, which is what makes blockkit's fingerprint — and therefore the
// ledger's "update only when it actually changed" — mean anything. Handing that
// to a typed SDK with map-backed fields would quietly break it.
//
// Decoding is the mirror image. An inbound callback is a payload someone else
// composed, its shape is Slack's to change, and the SDK already models it.
type SocketListener struct {
	creds  slack.Credentials
	logger *slog.Logger
}

// NewSocketListener builds a listener for one profile's app.
func NewSocketListener(creds slack.Credentials, logger *slog.Logger) *SocketListener {
	if logger == nil {
		logger = slog.Default()
	}
	return &SocketListener{creds: creds, logger: logger}
}

// Listen connects and pumps interaction callbacks into deliver until ctx is
// cancelled.
//
// Each callback is acknowledged *before* it is handled, and handled on its own
// goroutine. Slack expects an ack within three seconds and a handler here can
// legitimately take longer than that — approving a pull request makes several
// GitHub calls with retries — so acknowledging after the work would leave Slack
// re-sending an interaction that is already being acted on.
func (l *SocketListener) Listen(ctx context.Context, deliver func(slackgo.InteractionCallback)) error {
	api := slackgo.New(l.creds.BotToken, slackgo.OptionAppLevelToken(l.creds.AppToken))
	client := socketmode.New(api)

	runErr := make(chan error, 1)
	go func() { runErr <- client.RunContext(ctx) }()

	for {
		select {
		case <-ctx.Done():
			// A cancelled context is how this process is asked to stop, so it is
			// a clean exit rather than an error to report up the stack.
			return nil

		case err := <-runErr:
			if err == nil || errors.Is(err, context.Canceled) {
				return nil
			}
			return err

		case ev, open := <-client.Events:
			if !open {
				return nil
			}
			l.dispatch(client, ev, deliver)
		}
	}
}

// dispatch acknowledges one socket event and hands the interactive ones on.
func (l *SocketListener) dispatch(client *socketmode.Client, ev socketmode.Event, deliver func(slackgo.InteractionCallback)) {
	switch ev.Type {
	case socketmode.EventTypeConnecting:
		l.logger.Info("connecting to Slack", "profile", l.creds.Profile)
	case socketmode.EventTypeConnected:
		l.logger.Info("connected to Slack", "profile", l.creds.Profile)
	case socketmode.EventTypeConnectionError:
		// The client reconnects on its own; this is worth seeing but is not
		// terminal.
		l.logger.Warn("Slack connection error", "profile", l.creds.Profile, "detail", ev.Data)
	case socketmode.EventTypeInvalidAuth:
		l.logger.Error("Slack rejected the credentials", "profile", l.creds.Profile)
	case socketmode.EventTypeInteractive:
		cb, ok := ev.Data.(slackgo.InteractionCallback)
		if !ok {
			l.logger.Warn("interactive event carried an unexpected payload")
			return
		}
		if ev.Request != nil {
			client.Ack(*ev.Request)
		}
		go deliver(cb)
	}
}
