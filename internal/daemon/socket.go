package daemon

import (
	"context"
	"errors"
	"log/slog"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/riggs-mcp/internal/slack"
)

// acker is the acknowledgement half of socketmode.Client, extracted so dispatch
// can be driven in a test. The rule it enforces — ack everything — is the kind
// that is broken by a MISSING branch, which is only catchable by exercising the
// real function.
type acker interface {
	Ack(req socketmode.Request, payload ...any) error
}

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

// Listen connects and pumps events into h until ctx is cancelled.
//
// Each callback is acknowledged *before* it is handled, and handled on its own
// goroutine. Slack expects an ack within three seconds and a handler here can
// legitimately take longer than that — approving a pull request makes several
// GitHub calls with retries — so acknowledging after the work would leave Slack
// re-sending an interaction that is already being acted on.
func (l *SocketListener) Listen(ctx context.Context, h Handlers) error {
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
			l.dispatch(client, ev, h)
		}
	}
}

// dispatch acknowledges one socket event and hands the interactive ones on.
//
// The acknowledgement comes FIRST, and for every event that carries a request —
// not only the ones this switch understands.
//
// Slack expects a response to each request it sends down the socket, and marks
// the control with a ⚠ when it does not get one. An earlier version acked only
// inside the interactive branch, with no default arm, so any event slack-go
// classified as anything else was dropped in total silence: no ack, so Slack
// warned; and no log line, so nothing recorded that it had happened. A link
// button, whose interaction Riggs deliberately does nothing with, still needs
// answering.
func (l *SocketListener) dispatch(ack acker, ev socketmode.Event, h Handlers) {
	if ev.Request != nil {
		// A failed ack is worth seeing but changes nothing: Slack will not be
		// told twice, and the work still needs doing.
		if err := ack.Ack(*ev.Request); err != nil {
			l.logger.Warn("could not acknowledge a socket request", "type", string(ev.Type), "error", err)
		}
	}

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
		if h.Interaction == nil {
			l.logger.Debug("no interaction handler wired")
			return
		}
		go h.Interaction(cb)

	case socketmode.EventTypeEventsAPI:
		l.dispatchEventsAPI(ev, h)

	case socketmode.EventTypeHello, socketmode.EventTypeDisconnect,
		socketmode.EventTypeSlashCommand:
		// Known, and not this daemon's business. Acked above; named here so
		// they do not reach the default arm and read as a surprise.
		l.logger.Debug("ignoring socket event", "type", string(ev.Type))

	default:
		// Anything else is acked and reported. A silent default is how the
		// missing ack went unnoticed.
		l.logger.Info("unhandled socket event", "type", string(ev.Type), "acked", ev.Request != nil)
	}
}

// appHomeTab is the value app_home_opened carries for the Home surface. The
// same event fires for the "messages" tab, which is somebody typing in the DM
// and has nothing to publish.
const appHomeTab = "home"

// dispatchEventsAPI handles the one Events API subscription Riggs has:
// app_home_opened.
//
// Everything else is logged and dropped. It has already been acked, so an
// accidental subscription added in the app manifest costs a log line rather
// than a ⚠ on the user's screen.
func (l *SocketListener) dispatchEventsAPI(ev socketmode.Event, h Handlers) {
	api, ok := ev.Data.(slackevents.EventsAPIEvent)
	if !ok {
		l.logger.Warn("events API event carried an unexpected payload")
		return
	}
	switch inner := api.InnerEvent.Data.(type) {
	case *slackevents.AppHomeOpenedEvent:
		if inner.Tab != appHomeTab {
			l.logger.Debug("app_home_opened on another tab", "tab", inner.Tab)
			return
		}
		if h.AppHomeOpened == nil {
			l.logger.Debug("no app home handler wired")
			return
		}
		// On its own goroutine, for the same reason a click is: publishing the
		// view checks GitHub for a release, and the ack is already sent.
		go h.AppHomeOpened(inner.User)

	default:
		l.logger.Debug("ignoring events API event", "type", api.InnerEvent.Type)
	}
}
