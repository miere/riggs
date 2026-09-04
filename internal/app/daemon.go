package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/miere/riggs-mcp/internal/ai"
	"github.com/miere/riggs-mcp/internal/apphome"
	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/daemon"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/schedule"
	"github.com/miere/riggs-mcp/internal/service"
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

	// The scheduler and the Home tab share ONE ledger handle, held for the life
	// of the daemon. Everywhere else in this codebase a handler opens the
	// ledger per click and closes it again, because a handle held across the
	// reconcile pass in another process goes stale — and that reasoning ends
	// here, because the reconcile pass now runs in THIS process. What is left
	// is a scheduler that reads a small table every fifteen seconds, which is
	// not something to open a database for each time.
	store, scheduler, err := a.scheduler(logger)
	if err != nil {
		// Not fatal, on the rule the whole binary follows: a missing capability
		// disables a feature and never stops the boot. Every button still
		// works; nothing fires on its own until this is fixed.
		logger.Error("the schedule is unavailable; no jobs will run", "error", err)
	}
	if store != nil {
		defer store.Close()
	}

	router := daemon.NewRouter()
	a.registerInteractions(router, creds)
	a.registerRunInteractions(router, creds, logger)

	home := a.appHome(creds, logger, store, scheduler)
	a.registerHomeInteractions(router, home)
	a.registerJobInteractions(router, home)

	// Cancelled when the listener returns, so the scheduler stops with the
	// process rather than outliving the thing it reports to.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	if scheduler != nil {
		go func() {
			if err := scheduler.Run(ctx); err != nil {
				logger.Error("the scheduler stopped", "error", err)
			}
		}()
	}

	listener := daemon.NewSocketListener(creds, logger)
	return daemon.New(listener, router, creds.Profile, logger).
		WithAppHome(home).
		WithReporter(&clickReporter{api: slack.NewAPI(), creds: creds}).
		Run(ctx)
}

// scheduler opens the ledger and assembles the job loop.
//
// A nil scheduler with a nil error is impossible: either both come back, or an
// error explains why neither did. The store is returned separately because the
// Home tab reads and writes jobs through it directly — the scheduler runs them,
// it does not own them.
func (a *Application) scheduler(logger *slog.Logger) (*notify.Store, *schedule.Scheduler, error) {
	store, err := ledger(a.cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the ledger: %w", err)
	}
	exec, err := schedule.SelfExec(a.jobConfigFlag())
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return store, schedule.New(store, exec, logger), nil
}

// jobConfigFlag is the config path a job is told to use, and empty when it is
// where Riggs would look anyway.
//
// The same rule the Murtaugh installer followed (§12): passing it always would
// put an absolute path in every log line for no benefit, and passing it never
// would send a daemon started with --config-file to the wrong config in its own
// children.
func (a *Application) jobConfigFlag() string {
	if a.cfg == nil || a.cfg.Path == config.NoFilePath || a.cfg.Path == config.DefaultPath() {
		return ""
	}
	return a.cfg.Path
}

// clickReporter tells whoever clicked that their click failed.
//
// It is the backstop under the handlers that announce their own failures, not a
// replacement for them: those name the pull request and the verb, and land in
// the thread the row is in. This one knows only that something failed, so it
// says so to one person and gets out of the way. What it guarantees is the part
// that was missing — that no click is silent, including from a handler nobody
// has taught to speak yet.
type clickReporter struct {
	api   *slack.API
	creds slack.Credentials
}

// ReportFailure posts the cause where the click came from.
//
// Ephemeral in a channel: the failure is one person's to see, and a real
// message would leave it in front of everyone permanently. A Home tab click has
// no channel at all — the surface is private already — so it falls back to a DM,
// which is the only way to reach that user about it.
func (r *clickReporter) ReportFailure(ctx context.Context, in slack.Interaction, cause error) error {
	text := fmt.Sprintf("%s That did not work — %v", blockkit.MarkerFailed, cause)
	msg := slack.Message{Text: text, Blocks: blockkit.ContextBlocks(text)}

	target := slack.Target{Profile: r.creds.Profile, BotToken: r.creds.BotToken}
	if in.Channel == "" {
		target.AdminUserID = in.UserID
		_, err := r.api.Post(ctx, target, msg)
		return err
	}
	target.Channel = in.Channel
	// Threaded when the click came from a message, so the answer sits under the
	// card or digest it is about rather than at the bottom of the channel.
	msg.ThreadTS = in.MessageTS
	return r.api.PostEphemeral(ctx, target, in.UserID, msg)
}

// appHome assembles the Home tab publisher.
//
// Everything it needs is process-wide and cheap to hold: a version string, one
// HTTP client, and a release lookup that caches for an hour. That is the
// opposite of the click handlers below, which build and close their
// dependencies per interaction — but the reason for those is a ledger and a
// GitHub token that go stale between the handful of clicks a week anyone makes,
// and neither applies here.
func (a *Application) appHome(creds slack.Credentials, logger *slog.Logger,
	store *notify.Store, scheduler *schedule.Scheduler) *apphome.Publisher {
	checker := updates.New(updates.Deps{
		Current: version.String(),
		HTTPGet: updates.HTTPGetter(),
	})
	return apphome.New(apphome.Deps{
		Version:     version.String(),
		BotToken:    creds.BotToken,
		AdminUserID: a.cfg.Admin.SlackUserID,
		Views:       slack.NewAPI(),
		Modals:      slack.NewAPI(),
		Notify:      slack.NewAPI(),
		// The loaded config IS the store. An edit therefore lands in the file
		// and in the struct this process is already reading from, which is what
		// makes a reworded prompt take effect on the next click rather than the
		// next restart.
		Prompts: a.cfg,
		// Typed nils would satisfy the interfaces and then panic on first use,
		// so an unavailable ledger leaves the fields genuinely empty and the
		// Jobs section is not drawn at all.
		Jobs:      jobStoreOrNil(store),
		Runner:    jobRunnerOrNil(scheduler),
		Checker:   checker,
		Installer: updates.NewInstaller(checker),
		Restart:   restartViaSupervisor,
		Logger:    logger,
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

	// Which prompt a click is about rides in the row's block_id, exactly as a
	// pull request rides in a digest row's — an overflow reports its own
	// block_id and not its siblings' values, so it is the only place a per-row
	// identity can travel.
	router.Handle(blockkit.HomePromptActionID, blockkit.HomePromptEditIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			return home.EditPrompt(ctx, in.UserID, promptID(in.Item), in.TriggerID)
		}))

	router.Handle(blockkit.HomePromptActionID, blockkit.HomePromptResetIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			return home.ResetPrompt(ctx, in.UserID, promptID(in.Item))
		}))

	// The modal coming back. It is in the same table as the clicks because it
	// is the same kind of thing (§7b): the callback_id is the control and the
	// private_metadata is the item, which is the split a click already makes.
	router.Handle(blockkit.PromptModalCallbackID, slack.ViewSubmitIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			text := slack.ViewInput(in.Raw, blockkit.PromptModalBlockID, blockkit.PromptModalActionID)
			if strings.TrimSpace(text) == "" {
				// Slack's own required-input validation should have caught this
				// before it was sent. If it ever does not, an empty save would
				// silently RESET the prompt, and a reset the admin did not ask
				// for is worse than a refused save.
				return fmt.Errorf("the prompt was empty; use Reset to default to clear it")
			}
			return home.SavePrompt(ctx, in.UserID, in.Item, text)
		}))
}

// promptID strips the namespace a prompt row's block_id carries.
func promptID(blockID string) string {
	return strings.TrimPrefix(blockID, blockkit.HomePromptBlockPrefix)
}

// registerRunInteractions installs the two options that RUN the local harness,
// as opposed to the two that ask a person.
//
// Their runner is built ONCE and held, unlike every handler above it. Those
// build a ledger and a GitHub client per click because both go stale between
// the handful of clicks a week anyone makes; this holds a command line, a
// timeout and the set of items currently running — and that last one is the
// whole reason it is held. A runner rebuilt per click could not tell that the
// same pull request is already being reviewed by the process next door.
//
// Nothing is registered when no harness is configured. The options are not
// rendered either (§7bb), and a route with no control is a route that can only
// ever answer a click on a digest posted before the harness was removed.
func (a *Application) registerRunInteractions(router *daemon.Router, creds slack.Credentials, logger *slog.Logger) {
	if !a.cfg.AIEnabled() {
		logger.Info("no AI command configured; the Run options are off",
			"setting", "ai.command")
		return
	}
	harness := ai.New(a.cfg.AICommand(), a.cfg.AIWorkDir(), a.cfg.AITimeout())
	api := slack.NewAPI()

	// The prompts are read per run rather than captured here, so an edit on the
	// Home tab reaches the next click rather than the next restart.
	reviews := ai.NewRunner(harness, api, "a code review", a.cfg.AIReviewPrompt)
	assists := ai.NewRunner(harness, api, "AI assistance", a.cfg.AIAssistPrompt)

	router.Handle(pullrequest.BulkActionID, pullrequest.IntentRunReview,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			item := ai.Item{Ref: in.Item, URL: pullrequest.RefURL(in.Item)}
			_, err := reviews.Run(ctx, item, a.targetFor(creds, in), in.MessageTS)
			return err
		}))

	router.Handle(ticket.BulkActionID, ticket.IntentRunAssist,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			key := ticket.TicketKeyFromBlockID(in.Item)
			// Derived, not fetched: the browse URL is the tenant and the key,
			// and a run should not need a Jira round trip to start.
			item := ai.Item{Ref: key, URL: jiraClient(a.cfg).BrowseURL(key)}
			_, err := assists.Run(ctx, item, a.targetFor(creds, in), in.MessageTS)
			return err
		}))
}

// jobStoreOrNil keeps a nil *notify.Store out of a non-nil interface.
//
// The classic Go trap: a nil pointer in an interface is not a nil interface, so
// `Jobs != nil` would be true and the first read would panic. Both of these
// exist so the "not wired up" path is genuinely not wired up.
func jobStoreOrNil(store *notify.Store) apphome.JobStore {
	if store == nil {
		return nil
	}
	return store
}

// jobRunnerOrNil does the same for the scheduler.
func jobRunnerOrNil(s *schedule.Scheduler) apphome.JobRunner {
	if s == nil {
		return nil
	}
	return s
}

// registerJobInteractions installs the Jobs section's controls.
//
// Which job a click is about rides in the row's block_id, exactly as a prompt
// and a pull request do. Delete is the exception to "a click acts": it opens a
// confirmation modal (§7e), and the delete happens on that modal's submission,
// where the job name arrives in private_metadata instead.
func (a *Application) registerJobInteractions(router *daemon.Router, home *apphome.Publisher) {
	router.Handle(blockkit.HomeMenuActionID, blockkit.HomeNewJobIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			return home.NewJob(ctx, in.UserID, in.TriggerID)
		}))

	for intent, handle := range map[string]func(context.Context, *apphome.Publisher, slack.Interaction) error{
		blockkit.HomeJobEditIntent: func(ctx context.Context, h *apphome.Publisher, in slack.Interaction) error {
			return h.EditJob(ctx, in.UserID, jobName(in.Item), in.TriggerID)
		},
		blockkit.HomeJobRunIntent: func(ctx context.Context, h *apphome.Publisher, in slack.Interaction) error {
			return h.RunJob(ctx, in.UserID, jobName(in.Item))
		},
		blockkit.HomeJobToggleIntent: func(ctx context.Context, h *apphome.Publisher, in slack.Interaction) error {
			return h.ToggleJob(ctx, in.UserID, jobName(in.Item))
		},
		blockkit.HomeJobDeleteIntent: func(ctx context.Context, h *apphome.Publisher, in slack.Interaction) error {
			return h.ConfirmDeleteJob(ctx, in.UserID, jobName(in.Item), in.TriggerID)
		},
	} {
		handle := handle
		router.Handle(blockkit.HomeJobActionID, intent,
			daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
				return handle(ctx, home, in)
			}))
	}

	// The job editor coming back. Every field is read by (block_id, action_id),
	// which is how Slack reports a submission's state.
	router.Handle(blockkit.JobModalCallbackID, slack.ViewSubmitIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			field := func(blockID string) string {
				return slack.ViewInput(in.Raw, blockID, blockkit.JobModalActionID)
			}
			return home.SaveJob(ctx, in.UserID, in.Item,
				field(blockkit.JobModalNameBlockID),
				field(blockkit.JobModalCommandBlockID),
				field(blockkit.JobModalScheduleBlockID),
				field(blockkit.JobModalTimeoutBlockID))
		}))

	// The delete confirmation coming back. No fields to read: the job's name is
	// the whole payload, and it arrives in private_metadata the same way the
	// editor's does.
	router.Handle(blockkit.JobDeleteModalCallbackID, slack.ViewSubmitIntent,
		daemon.HandlerFunc(func(ctx context.Context, in slack.Interaction) error {
			return home.DeleteJob(ctx, in.UserID, in.Item)
		}))
}

// jobName strips the namespace a job row's block_id carries.
func jobName(blockID string) string {
	return strings.TrimPrefix(blockID, blockkit.HomeJobBlockPrefix)
}

// restartViaSupervisor asks this machine's init to restart the daemon, so it
// comes back running the binary that was just installed.
//
// It was launchd-only, which was defensible while the daemon was a macOS
// convenience and is not now that it carries the schedule: a Linux admin
// pressing Restart on the Home tab would have got an error naming a supervisor
// their machine does not have.
//
// Options carries only what the restart needs. The unit is not rewritten here —
// that is `riggs service install`'s job — so the binary path, config path and
// profile baked into it are left exactly as they were, which is the point: the
// supervisor restarts onto the same path, and the file at that path is new.
func restartViaSupervisor(ctx context.Context) error {
	manager, err := service.New(nil, service.Options{})
	if err != nil {
		return err
	}
	return manager.Restart(ctx)
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
			//
			// The failure thread is the digest's, because the card this posts
			// goes somewhere the clicker cannot see — so a silent failure and a
			// delivered request look the same from where they are standing.
			_, err = asker.
				WithFailureThread(in.Channel, in.MessageTS).
				Ask(ctx, in.Item, in.UserID, a.targetFor(creds, in))
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
	// `sme-assistance`, never from the pull-request queue's `review-request`.
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
