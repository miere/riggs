package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/miere/riggs-mcp/internal/daemon"
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
// Empty for now: the transport lands before the controls that use it, so this
// commit can be reviewed and run on its own. A daemon with no routes still
// connects and still logs every click it cannot place, which is exactly what
// you want while wiring the app up in Slack.
func (a *Application) registerInteractions(router *daemon.Router, creds slack.Credentials) {}

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
