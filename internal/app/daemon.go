package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/miere/riggs-mcp/internal/daemon"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
)

// profileFlag selects which configured Slack app the daemon listens as.
const profileFlag = "--slack-profile"

// runDaemon wires and starts the inbound half.
//
// It is deliberately a separate construction path from the tool registry. A
// tool is a one-shot invoked with a resolved destination; the daemon is a
// long-lived listener bound to an app. They share the config and the ledger,
// and nothing else.
func (a *Application) runDaemon(ctx context.Context) error {
	profile, err := daemonProfile(a.args)
	if err != nil {
		return err
	}

	creds, err := slack.NewResolver(a.cfg).Credentials(profile)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	router := daemon.NewRouter()
	a.registerInteractions(router, creds)

	listener := daemon.NewSocketListener(creds, logger)
	return daemon.New(listener, router, creds.Profile, logger).Run(ctx)
}

// registerInteractions installs the handlers for the controls Riggs renders.
//
// Every handler builds its own dependencies per click and closes them again.
// A daemon that held the ledger and a GitHub client open for its lifetime would
// be holding them open all day for the handful of seconds a week anyone spends
// clicking, and would keep a stale connection across the reconcile pass that
// runs in a different process entirely.
func (a *Application) registerInteractions(router *daemon.Router, creds slack.Credentials) {
	router.Handle(pullrequest.BulkActionID, pullrequest.IntentAskReview,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			asker := pullrequest.NewAsker(slack.NewAPI(),
				a.cfg.ReviewReviewer(), a.cfg.ReviewRequest.Channel, a.cfg.ReviewPrompt())
			_, err := asker.Ask(ctx, in.Item, a.targetFor(creds, in))
			return err
		}))

	router.Handle(pullrequest.BulkActionID, pullrequest.IntentApproveMerge,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			approver, closer, err := approverFor(a.cfg)
			if err != nil {
				return err
			}
			defer closer.Close()
			// The outcome is narrated into the digest's own thread, so the
			// answer lands next to the row that was clicked.
			_, err = approver.Run(ctx, in.Item, true, a.targetFor(creds, in), in.MessageTS)
			return err
		}))

	// IntentOpenBrowser is deliberately unregistered. Slack opens the link
	// itself; the interaction it also sends has nothing to do, and a handler
	// that exists only to return nil is worse than the router's own "no handler"
	// log line.
}

// targetFor builds the delivery target for a click: this daemon's credentials,
// the conversation the click came from.
func (a *Application) targetFor(creds slack.Credentials, in slack.Interaction) slack.Target {
	return slack.Target{
		Profile:     creds.Profile,
		BotToken:    creds.BotToken,
		Channel:     in.Channel,
		AdminUserID: a.cfg.Admin.SlackUserID,
	}
}

// daemonProfile reads --slack-profile from the daemon's arguments. Empty means
// the default profile, which Credentials resolves.
func daemonProfile(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == profileFlag:
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a profile name", profileFlag)
			}
			return args[i+1], nil
		case len(args[i]) > len(profileFlag)+1 && args[i][:len(profileFlag)+1] == profileFlag+"=":
			return args[i][len(profileFlag)+1:], nil
		default:
			return "", fmt.Errorf("daemon: unexpected argument %q (only %s is accepted)", args[i], profileFlag)
		}
	}
	return "", nil
}
