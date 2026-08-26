// Command riggs is the single entry point for the Riggs tool. It runs as a CLI
// (`riggs ping`), as an MCP stdio server (`riggs mcp`), or as a Socket Mode
// daemon (`riggs daemon`).
//
// The first two are one-shots backed by the same tool registry, so Murtaugh can
// invoke either. The third is long-lived and backed by a routing table instead:
// it is how Riggs answers clicks on the messages it posted itself.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/miere/riggs-mcp/internal/app"
	"github.com/miere/riggs-mcp/internal/installer"
	"github.com/miere/riggs-mcp/internal/version"
)

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

	// `riggs install` is interactive, so it lives outside the tool registry
	// and is never exposed over MCP.
	if len(args) > 0 && args[0] == "install" {
		return runInstall(context.Background())
	}

	mode := app.ModeCLI
	rest := args
	if len(args) > 0 {
		switch args[0] {
		case "mcp":
			mode, rest = app.ModeMCP, args[1:]
		case "daemon":
			mode, rest = app.ModeDaemon, args[1:]
		}
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

// runInstall drives the interactive installer.
func runInstall(ctx context.Context) error {
	console := installer.NewConsole()
	if !console.IsTerminal() {
		return fmt.Errorf("install needs a terminal: it reads credentials without echoing them")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary (needed as the job command): %w", err)
	}
	return installer.New(console, installer.Options{
		RiggsPath: self,
		ToolsFor:  app.ToolNames,
	}).Run(ctx)
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
