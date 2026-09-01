// Package app is the composition root. It owns the tool registry, picks the
// frontend based on the parsed mode, and starts it. Frontends know nothing
// about each other; the application knows about both but delegates execution
// entirely.
package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/frontends/cli"
	"github.com/miere/riggs-mcp/internal/frontends/mcp"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/tools"
	"github.com/miere/riggs-mcp/internal/tools/capabilities"
	"github.com/miere/riggs-mcp/internal/tools/ping"
	"github.com/miere/riggs-mcp/internal/tools/sendmsg"
)

// Mode selects which frontend Run starts.
type Mode int

const (
	// ModeCLI runs the human-facing CLI frontend.
	ModeCLI Mode = iota
	// ModeMCP runs the MCP stdio server frontend.
	ModeMCP
	// ModeDaemon holds a Socket Mode connection open and reacts to clicks on
	// Riggs' own messages. Unlike the other two it is long-lived.
	ModeDaemon
)

// Application is the composition root for a single riggs invocation.
type Application struct {
	mode     Mode
	args     []string
	registry *tools.Registry
	// cfg is retained for the modes that need more than the registry. The
	// daemon resolves its own credentials from it.
	cfg *config.Config
}

// New constructs an Application configured for the given mode. args is the list
// of positional arguments passed to the selected frontend (the top-level mode
// token is stripped by the caller before this point).
//
// configPath is the --config-file value; empty means the usual precedence
// chain, ending at ~/.config/riggs/config.yaml.
func New(mode Mode, args []string, configPath string) (*Application, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}

	reg := tools.NewRegistry()
	reg.Register(ping.New())
	reg.Register(capabilities.New(cfg))

	// Slack-backed tools are registered only when there is an account to post
	// through. A tool that cannot possibly work is worse than an absent one,
	// and `riggs capabilities` explains the absence (§6).
	if len(cfg.Slack.Profiles) > 0 {
		resolver := slack.NewResolver(cfg)
		reg.Register(sendmsg.New(resolver, slack.NewAPI()))
		registerGitHubTools(reg, cfg, resolver)
		registerJiraTools(reg, cfg, resolver)
	}

	return &Application{mode: mode, args: args, registry: reg, cfg: cfg}, nil
}

// ToolNames reports which tools this binary exposes under the config at path.
// The installer uses it to avoid registering a job whose tool does not exist:
// the answer depends on the config, because Slack-backed tools are gated on a
// configured profile (§6).
func ToolNames(configPath string) (map[string]bool, error) {
	a, err := New(ModeCLI, nil, configPath)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(a.registry.All()))
	for _, t := range a.registry.All() {
		names[t.Name()] = true
	}
	return names, nil
}

// Run starts the selected frontend and blocks until it returns.
func (a *Application) Run(ctx context.Context) error {
	switch a.mode {
	case ModeMCP:
		return mcp.New(a.registry).Serve(ctx)
	case ModeDaemon:
		return a.runDaemon(ctx)
	default:
		return cli.New(a.registry).Run(ctx, a.args)
	}
}

// UsageLine renders a human-readable usage string built from the registered
// tools. Flat tool names (e.g. `ping`) are listed first; namespaced tools are
// grouped by their namespace. A three-part name (`git.pr.approve`) groups under
// its first two segments and renders as a verb flag, which is how it is spelled
// on the command line: `riggs git pr --approve <ref>`.
func (a *Application) UsageLine() string {
	var flat []string
	groups := map[string][]string{}
	var groupOrder []string

	add := func(ns, sub string) {
		if _, seen := groups[ns]; !seen {
			groupOrder = append(groupOrder, ns)
		}
		groups[ns] = append(groups[ns], sub)
	}

	for _, t := range a.registry.All() {
		parts := strings.Split(t.Name(), ".")
		switch len(parts) {
		case 1:
			flat = append(flat, parts[0])
		case 2:
			add(parts[0], parts[1])
		default:
			add(strings.Join(parts[:len(parts)-1], " "), "--"+parts[len(parts)-1])
		}
	}

	parts := append([]string{}, flat...)
	for _, ns := range groupOrder {
		subs := groups[ns]
		sort.Strings(subs)
		parts = append(parts, fmt.Sprintf("%s <%s>", ns, strings.Join(subs, "|")))
	}
	// The modes handled in main.go, outside the registry. `launchd` is absent
	// on purpose: it still works, but it is the old name for `service` and a
	// usage line should name one way to do a thing.
	parts = append(parts, "mcp", "daemon", "service", "jobs", "install")
	return "usage: riggs <command>; commands: " + strings.Join(parts, ", ")
}
