package app

import (
	"context"
	"io"
	"time"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/tools/bulkreviews"
)

// ledger opens the notification store. It is opened per invocation rather than
// at startup, so `riggs ping` neither pays for it nor creates the database as
// a side effect.
func ledger(cfg *config.Config) (*notify.Store, error) {
	return notify.Open(cfg.DBPath())
}

// githubClient builds a REST client authenticated from the `gh` CLI and backed
// by the ledger's conditional-request cache — which is what makes a
// per-minute poll nearly free (§8).
func githubClient(ctx context.Context, store *notify.Store) (*github.Client, error) {
	auth, err := github.AuthFromGH(ctx, nil)
	if err != nil {
		return nil, err
	}
	return github.New(auth.Token).WithCache(store, time.Now), nil
}

// bulkEngineFor assembles the digest reconciler for one invocation. It reuses
// engineFor's GitHub reads and ledger — the digest is a second consumer of
// both, in its own ledger stream.
func bulkEngineFor(cfg *config.Config, login string, opts pullrequest.BulkOptions) (*pullrequest.BulkEngine, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	opts.Actions = reviewRowActions(cfg)
	gh, err := githubClient(context.Background(), store)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	notifier := notify.New(store, slack.NewAPI())
	engine := pullrequest.NewEngine(gh, store, notifier, login, cfg.Admin.SlackUserID)
	return pullrequest.NewBulkEngine(engine, store, notifier, opts), store, nil
}

// askerFor assembles the review-request poster for one invocation. It needs
// GitHub (to render the card) and the ledger (for the summary the queue may
// already have written).
func askerFor(cfg *config.Config) (*pullrequest.Asker, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	gh, err := githubClient(context.Background(), store)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	api := slack.NewAPI()
	asker := pullrequest.NewAsker(gh, store, notify.New(store, api), api,
		cfg.ReviewReviewer(), cfg.ReviewRequest.Channel, cfg.ReviewPrompt()).WithResolver(api)
	return asker, store, nil
}

// completerFor assembles the digest completer. It needs no GitHub client: every
// row renders from the ledger (§9b).
func completerFor(cfg *config.Config) (*pullrequest.Completer, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	api := slack.NewAPI()
	// The same actions the digest engine renders with. A rebuild drawn under
	// different rules would change the menu on every row in the message.
	completer := pullrequest.NewCompleter(store, notify.New(store, api), api, reviewRowActions(cfg))

	// The digest renders from the ledger, but an ask card has to be redrawn
	// from the pull request. A GitHub client that cannot be built is not fatal:
	// the digest half still works, and Settle says so if it is reached.
	if gh, err := githubClient(context.Background(), store); err == nil {
		completer = completer.WithDetailer(gh)
	}
	return completer, store, nil
}

// approverFor assembles the approver for one invocation.
func approverFor(cfg *config.Config) (*pullrequest.Approver, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	gh, err := githubClient(context.Background(), store)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return pullrequest.NewApprover(gh, store, slack.NewAPI()), store, nil
}

// reviewRowActions reads which optional verbs a pull-request row may offer.
//
// Resolved in the composition root, like its ticket counterpart, and off by
// default: "Ask for Code Review" needs somebody to ask and "Run Code Review"
// needs a harness to run, and neither is rendered without one.
func reviewRowActions(cfg *config.Config) pullrequest.RowActions {
	return pullrequest.RowActions{
		AskReview: cfg.ReviewEnabled(),
		RunReview: cfg.AIEnabled(),
	}
}

// reviewDigest builds the pull-request digest command.
//
// No default login. Whose reviews a pass fetches is named on the command that
// runs it, never resolved from the config at run time.
func reviewDigest(cfg *config.Config) *bulkreviews.Tool {
	return bulkreviews.New(slack.NewResolver(cfg),
		func(_ context.Context, login string, opts pullrequest.BulkOptions) (bulkreviews.Engine, io.Closer, error) {
			return bulkEngineFor(cfg, login, opts)
		})
}
