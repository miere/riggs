// Command riggs is the single entry point for the Riggs tool. It runs either
// as a CLI (`riggs ping`) or as an MCP stdio server (`riggs mcp`). Both modes
// are backed by the same tool registry, so Murtaugh can invoke it either way.
package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/miere/riggs-mcp/internal/app"
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

	mode := app.ModeCLI
	rest := args
	if len(args) > 0 && args[0] == "mcp" {
		mode = app.ModeMCP
		rest = args[1:]
	}

	a, err := app.New(mode, rest, configPath)
	if err != nil {
		return err
	}
	if mode == app.ModeCLI && len(args) == 0 {
		return fmt.Errorf("%s", a.UsageLine())
	}
	return a.Run(context.Background())
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
