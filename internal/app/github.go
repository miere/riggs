package app

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/miere/riggs-mcp/internal/ai"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/tools"
	"github.com/miere/riggs-mcp/internal/tools/approve"
	"github.com/miere/riggs-mcp/internal/tools/bulkreviews"
	"github.com/miere/riggs-mcp/internal/tools/fetchreviews"
	"github.com/miere/riggs-mcp/internal/tools/importstate"
	"github.com/miere/riggs-mcp/internal/tools/parity"
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

// summariser picks the card-summary backend. With no `claude` on PATH the
// titles stand in, rather than every card failing.
func summariser() ai.Summariser {
	if _, err := exec.LookPath("claude"); err != nil {
		return ai.Titles{}
	}
	return ai.NewClaude()
}

// engineFor assembles the reconciler for one invocation, returning the closer
// for the ledger it opened.
func engineFor(cfg *config.Config, login string) (*pullrequest.Engine, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	gh, err := githubClient(context.Background(), store)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	notifier := notify.New(store, slack.NewAPI())
	engine := pullrequest.NewEngine(gh, store, notifier, summariser(), login, cfg.Admin.SlackUserID)
	return engine, store, nil
}

// bulkEngineFor assembles the digest reconciler for one invocation. It reuses
// engineFor's GitHub reads and ledger — the digest is a second consumer of
// both, in its own ledger stream.
func bulkEngineFor(cfg *config.Config, login string, opts pullrequest.BulkOptions) (*pullrequest.BulkEngine, io.Closer, error) {
	store, err := ledger(cfg)
	if err != nil {
		return nil, nil, err
	}
	gh, err := githubClient(context.Background(), store)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	notifier := notify.New(store, slack.NewAPI())
	engine := pullrequest.NewEngine(gh, store, notifier, summariser(), login, cfg.Admin.SlackUserID)
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
	asker := pullrequest.NewAsker(gh, store, summariser(), api,
		cfg.ReviewReviewer(), cfg.ReviewRequest.Channel, cfg.ReviewPrompt()).WithResolver(api)
	return asker, store, nil
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

// registerGitHubTools wires the pull-request tools, when the prerequisites
// exist. Slack is required because every one of them delivers or is about to;
// `riggs capabilities` explains an absence.
func registerGitHubTools(reg *tools.Registry, cfg *config.Config, resolver *slack.Resolver) {
	// No default login. Whose reviews a pass fetches is named on the command
	// that runs it, never resolved from this file at run time.
	reg.Register(fetchreviews.New(resolver,
		func(_ context.Context, login string) (fetchreviews.Engine, io.Closer, error) {
			return engineFor(cfg, login)
		}))

	reg.Register(bulkreviews.New(resolver,
		func(_ context.Context, login string, opts pullrequest.BulkOptions) (bulkreviews.Engine, io.Closer, error) {
			return bulkEngineFor(cfg, login, opts)
		}))

	reg.Register(parity.New(
		func(_ context.Context, login string) (parity.Resolver, io.Closer, error) {
			return engineFor(cfg, login)
		}))

	approveFactory := func(context.Context) (approve.Approver, io.Closer, error) {
		return approverFor(cfg)
	}
	reg.Register(approve.New(resolver, approveFactory))
	reg.Register(approve.NewMerge(resolver, approveFactory))

	reg.Register(importstate.New(func(context.Context) (importstate.Store, func() error, error) {
		store, err := ledger(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("opening the ledger: %w", err)
		}
		return store, store.Close, nil
	}))
}
