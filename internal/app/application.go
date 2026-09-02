// Package app is the composition root. It assembles the two digest commands
// and either the CLI or the daemon, and delegates execution entirely.
//
// It owned a tool registry for a long time, because two frontends — the CLI and
// an MCP stdio server — served the same set of commands and neither should have
// had to know what the other exposed. Both halves of that are gone: nothing
// registers Riggs as an MCP server, and the set is down to two.
package app

import (
	"context"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/frontends/cli"
	"github.com/miere/riggs-mcp/internal/tools/bulkreviews"
	"github.com/miere/riggs-mcp/internal/tools/bulktickets"
)

// Mode selects what Run starts.
type Mode int

const (
	// ModeCLI runs the human-facing CLI frontend.
	ModeCLI Mode = iota
	// ModeDaemon holds a Socket Mode connection open, reacts to clicks on
	// Riggs' own messages, and ticks the schedule. Unlike the CLI it is
	// long-lived.
	ModeDaemon
)

// Application is the composition root for a single riggs invocation.
type Application struct {
	mode Mode
	args []string
	// reviews and tickets are the two digests, nil when there is no Slack
	// account to post through.
	reviews *bulkreviews.Tool
	tickets *bulktickets.Tool
	// cfg is what the daemon resolves its own credentials and ledger from.
	cfg *config.Config
}

// New constructs an Application configured for the given mode. args is the
// argument list passed to the selected frontend, with the mode token already
// stripped by the caller.
//
// configPath is the --config-file value; empty means the usual precedence
// chain, ending at ~/.config/riggs/config.yaml.
func New(mode Mode, args []string, configPath string) (*Application, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	a := &Application{mode: mode, args: args, cfg: cfg}

	// Both digests need a Slack account to post through. Without one they are
	// absent rather than broken, and `riggs capabilities` names the setting
	// that would bring them back (§6).
	if len(cfg.Slack.Profiles) > 0 {
		a.reviews = reviewDigest(cfg)
		a.tickets = ticketDigest(cfg)
	}
	return a, nil
}

// Run starts the selected frontend and blocks until it returns.
func (a *Application) Run(ctx context.Context) error {
	if a.mode == ModeDaemon {
		return a.runDaemon(ctx)
	}
	return cli.New(invokerOrNil(a.reviews), invokerOrNil(a.tickets)).Run(ctx, a.args)
}

// UsageLine renders the command list.
//
// It was built from the registry, reconstructing each command's spelling by
// splitting its dotted name. With two commands that machinery was reassembling
// a constant, so this is the constant.
func (a *Application) UsageLine() string {
	return "usage: riggs <command>; commands: " +
		"git pr --bulk <github-login>, jira tickets --bulk <jql>, " +
		"capabilities, daemon, service, jobs, install, version"
}

// invokerOrNil keeps a nil *Tool out of a non-nil interface.
//
// The classic Go trap: a nil pointer in an interface is not a nil interface, so
// the frontend's "is this command configured?" check would pass and the call
// would panic. Both digests are legitimately absent on an unconfigured machine,
// so this is the ordinary path, not an edge case.
func invokerOrNil[T comparable](tool T) cli.Invoker {
	var zero T
	if tool == zero {
		return nil
	}
	return any(tool).(cli.Invoker)
}
