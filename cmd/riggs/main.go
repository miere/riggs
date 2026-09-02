// Command riggs is the single entry point. It runs as a CLI — the two digests
// plus the operational commands — or as a Socket Mode daemon (`riggs daemon`).
//
// There was a third: an MCP stdio server, sharing the CLI's tool registry so
// either could invoke the same commands. Nothing registered Riggs as an MCP
// server, so it went, and the registry went with it.
//
// The daemon is the long-lived one, backed by a routing table rather than a
// command list: it is how Riggs answers clicks on the messages it posted, and
// it now carries the schedule too.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/miere/riggs-mcp/internal/app"
	"github.com/miere/riggs-mcp/internal/capabilities"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/installer"
	"github.com/miere/riggs-mcp/internal/version"
)

// stderr is the diagnostic stream, named so tests can redirect it.
var stderr io.Writer = os.Stderr

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "riggs:", err)
		os.Exit(1)
	}
}

// run parses the top-level command and delegates to the application layer. It
// is separated from main() so it can be exercised by tests.
func run(args []string) error {
	// --config-file is a frontend flag, not a tool parameter, so it is
	// stripped before mode parsing and may appear anywhere on the command
	// line.
	configPath, args, err := extractConfigFlag(args)
	if err != nil {
		return err
	}

	// `version` / `--version` / `-v` print the build version. Handled AFTER
	// --config-file extraction so `riggs --config-file <p> --version` works.
	if (len(args) > 0 && args[0] == "version") || slices.Contains(args, "--version") || slices.Contains(args, "-v") {
		fmt.Println(version.String())
		return nil
	}

	// `riggs install` is interactive, which is why it is a command rather than
	// one of the two digests.
	if len(args) > 0 && args[0] == "install" {
		return runInstall(context.Background())
	}

	// `riggs capabilities` reports what is enabled and names the setting
	// behind anything that is not. It was a registry tool until the registry
	// went; it is a command now, like `version`, because that is what it
	// always was.
	if len(args) > 0 && args[0] == "capabilities" {
		return runCapabilities(context.Background(), configPath, slices.Contains(os.Args, "--json-output"))
	}

	// `riggs service` mutates this machine's init. Like install, it is
	// deliberately not a Tool: it has nothing to do with what Riggs exposes to
	// an MCP client. `launchd` is its former name and still works.
	if len(args) > 0 && args[0] == "service" {
		return runService(context.Background(), args[1:], configPath)
	}
	if len(args) > 0 && args[0] == "launchd" {
		return runLaunchdAlias(context.Background(), args[1:], configPath)
	}

	// `riggs jobs` reads and writes the schedule the daemon runs. It is a CLI
	// rather than a Tool for the same reason: it is about operating this
	// machine, not about what Riggs can do for a caller.
	if len(args) > 0 && args[0] == "jobs" {
		return runJobs(context.Background(), args[1:], configPath)
	}

	mode := app.ModeCLI
	rest := args
	if len(args) > 0 && args[0] == "daemon" {
		mode, rest = app.ModeDaemon, args[1:]
	}

	a, err := app.New(mode, rest, configPath)
	if err != nil {
		return err
	}
	if mode == app.ModeCLI && len(args) == 0 {
		return fmt.Errorf("%s", a.UsageLine())
	}

	ctx := context.Background()
	if mode == app.ModeDaemon {
		// The only long-lived mode, and the only one a supervisor stops with a
		// signal. Cancelling the context lets the socket close and in-flight
		// handlers finish rather than dying mid-approval.
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	return a.Run(ctx)
}

// runCapabilities prints the diagnostic.
func runCapabilities(ctx context.Context, configPath string, asJSON bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	report, err := capabilities.New(cfg).Report(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		encoded, err := json.Marshal(report)
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	}
	fmt.Println(report)
	return nil
}

// runInstall drives the interactive installer.
func runInstall(ctx context.Context) error {
	console := installer.NewConsole()
	if !console.IsTerminal() {
		return fmt.Errorf("install needs a terminal: it reads credentials without echoing them")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}
	return installer.New(console, installer.Options{RiggsPath: self}).Run(ctx)
}

// configFlag selects the configuration file, overriding $RIGGS_CONFIG and the
// conventional locations. The ledger database is derived from it, so this one
// flag moves both.
const configFlag = "--config-file"

// extractConfigFlag removes `--config-file <path>` (or `--config-file=<path>`)
// from args, returning the path and the remaining tokens. The last occurrence
// wins, which is what a reader expects from a repeated flag.
func extractConfigFlag(args []string) (string, []string, error) {
	path := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == configFlag:
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a path", configFlag)
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, configFlag+"="):
			path = strings.TrimPrefix(a, configFlag+"=")
			if path == "" {
				return "", nil, fmt.Errorf("%s requires a path", configFlag)
			}
		default:
			rest = append(rest, a)
		}
	}
	return path, rest, nil
}
