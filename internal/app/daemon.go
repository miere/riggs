package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/miere/riggs-mcp/internal/apphome"
	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/daemon"
	"github.com/miere/riggs-mcp/internal/launchd"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/ticket"
	"github.com/miere/riggs-mcp/internal/updates"
	"github.com/miere/riggs-mcp/internal/version"
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	router := daemon.NewRouter()
	a.registerInteractions(router, creds)

	home := a.appHome(creds, logger)
	a.registerHomeInteractions(router, home)

	listener := daemon.NewSocketListener(creds, logger)
	return daemon.New(listener, router, creds.Profile, logger).WithAppHome(home).Run(ctx)
}

// appHome assembles the Home tab publisher.
//
// Everything it needs is process-wide and cheap to hold: a version string, one
// HTTP client, and a release lookup that caches for an hour. That is the
// opposite of the click handlers below, which build and close their
// dependencies per interaction — but the reason for those is a ledger and a
// GitHub token that go stale between the handful of clicks a week anyone makes,
// and neither applies here.
func (a *Application) appHome(creds slack.Credentials, logger *slog.Logger) *apphome.Publisher {
	checker := updates.New(updates.Deps{
		Current: version.String(),
		HTTPGet: updates.HTTPGetter(),
	})
	return apphome.New(apphome.Deps{
		Version:     version.String(),
		BotToken:    creds.BotToken,
		AdminUserID: a.cfg.Admin.SlackUserID,
		Views:       slack.NewAPI(),
		Notify:      slack.NewAPI(),
		Checker:     checker,
		Installer:   updates.NewInstaller(checker),
		Restart:     restartViaLaunchd,
		Logger:      logger,
	})
}

// registerHomeInteractions installs the Home tab's one control.
//
// It goes in the same routing table as the digest's buttons, on the same
// (action_id, intent) match, because it is the same kind of thing: a control
// Riggs rendered, clicked by a human, delivered to the app that drew it. The
// Home tab being a different surface from a channel message changes where the
// click came from, not what dispatching it means.
func (a *Application) registerHomeInteractions(router *daemon.Router, home *apphome.Publisher) {
	router.Handle(blockkit.HomeUpdateActionID, blockkit.HomeUpdateIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			return home.Update(ctx, in.UserID)
		}))

	router.Handle(blockkit.HomeMenuActionID, blockkit.HomeRestartIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			return home.Restart(ctx, in.UserID)
		}))
}

// restartViaLaunchd asks launchd to restart this daemon, so it comes back
// running the binary that was just installed.
//
// Options carries only what the restart needs. The plist is not rewritten here
// — that is `riggs launchd install`'s job — so the binary path, config path and
// profile baked into it are left exactly as they were, which is the point: the
// agent restarts onto the same path, and the file at that path is new.
func restartViaLaunchd(ctx context.Context) error {
	return launchd.New(nil, launchd.Options{}).Restart(ctx)
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
			asker, closer, err := askerFor(a.cfg)
			if err != nil {
				return err
			}
			defer closer.Close()
			// in.UserID is whoever clicked: they are copied in on the tag, so
			// the reviewer can see whose request it is.
			_, err = asker.Ask(ctx, in.Item, in.UserID, a.targetFor(creds, in))
			return err
		}))

	// The ask-review card's own Approve button. Same approver, one difference:
	// no comment is left on the pull request.
	router.Handle(pullrequest.AskActionID, pullrequest.IntentApprove,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			approver, closer, err := approverFor(a.cfg)
			if err != nil {
				return err
			}
			defer closer.Close()
			result, err := approver.WithoutReviewBody().Run(ctx, in.Item, false, a.targetFor(creds, in), in.MessageTS)
			return a.settle(ctx, creds, in, result, err, pullrequest.ReasonApproved)
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
			result, err := approver.Run(ctx, in.Item, true, a.targetFor(creds, in), in.MessageTS)
			return a.settle(ctx, creds, in, result, err, pullrequest.ReasonMerged)
		}))

	// The ticket digest's only verb. It reaches a different person, in a
	// different channel, with different wording — all of which come from
	// `ai-assistance`, never from the pull-request queue's `review-request`.
	//
	// "Assign to Me" is deliberately unregistered, because it is deliberately
	// not rendered (§7bb). The verb behind it exists; the option does not.
	router.Handle(ticket.BulkActionID, ticket.IntentAskAssist,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			asker, closer, err := assistAskerFor(a.cfg)
			if err != nil {
				return err
			}
			defer closer.Close()
			// in.UserID is whoever clicked: they are copied in on the tag, so
			// the recipient can see whose request it is.
			_, err = asker.Ask(ctx, in.Item, in.UserID, a.targetFor(creds, in))
			return err
		}))

	// IntentOpenBrowser is deliberately unregistered, on both digests. Slack
	// opens the link itself; the interaction it also sends has nothing to do,
	// and a handler that exists only to return nil is worse than the router's
	// own "no handler" log line.
}

// settle records what an approval did to the digest.
//
// On success the row is struck through immediately. The reconcile pass would
// reach the same conclusion on its own, but up to three minutes later — and a
// button that visibly does nothing for three minutes reads as one that did not
// work.
//
// On failure the reason is posted in the DIGEST's thread, not the clicked
// message's: the digest is where the row still sits waiting, and where somebody
// looking for the outcome will look. The approver has already narrated into the
// thread that was clicked; for a digest click those are the same place, and for
// an ask-review card they are deliberately not.
//
// Neither is allowed to mask the approval itself. A row that fails to redraw is
// still an approved pull request, so the original error is what comes back.
func (a *Application) settle(ctx context.Context, creds slack.Credentials,
	in slack.Interaction, result pullrequest.ApproveResult, runErr error, status string) error {

	completer, closer, err := completerFor(a.cfg)
	if err != nil {
		return errors.Join(runErr, err)
	}
	defer closer.Close()
	target := a.targetFor(creds, in)

	if runErr != nil {
		if _, err := completer.Fail(ctx, in.Item, result.Message, target); err != nil {
			return errors.Join(runErr, err)
		}
		return runErr
	}
	// Both, because they are independent facts: a pull request can be in a
	// digest, or have an ask card, or both, or neither.
	var problems []error
	if _, err := completer.Complete(ctx, in.Item, status, target); err != nil {
		problems = append(problems, fmt.Errorf("could not mark %s done in the digest: %w", in.Item, err))
	}
	if _, err := completer.Settle(ctx, in.Item, settledLabel(status), target); err != nil {
		problems = append(problems, fmt.Errorf("could not collapse the review request for %s: %w", in.Item, err))
	}
	if len(problems) > 0 {
		// The approval landed. Say what did not follow it, without pretending
		// the approval itself failed.
		return fmt.Errorf("approved %s, but: %w", in.Item, errors.Join(problems...))
	}
	return nil
}

// settledLabel is the line a collapsed ask card carries.
func settledLabel(status string) string {
	if status == pullrequest.ReasonMerged {
		return "Approved and merged"
	}
	return "Approved"
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

// LogLevelEnv raises or lowers the daemon's log level.
//
// It exists because the one time this mattered, the answer was in a Debug line
// nobody could see: the level was fixed at Info, so a callback the daemon chose
// to ignore left no trace at all.
const LogLevelEnv = "RIGGS_LOG_LEVEL"

// logLevel reads that variable. Anything unrecognised is Info, because a typo
// in a launch agent's environment should not silence the daemon.
func logLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(LogLevelEnv))) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
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
