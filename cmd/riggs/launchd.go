package main

import (
	"context"
	"fmt"
	"runtime"
)

// `riggs launchd` is the old name for `riggs service`.
//
// Kept because it is in muscle memory and in at least one README, and because
// removing a command is a poor way to announce that it moved. It forwards
// verbatim: the subcommands and flags are identical, and on macOS the manager
// behind them is the very same launchd code it always was.
//
// It says so once, on stderr rather than stdout, so a script parsing the output
// of `riggs launchd status` is unaffected.
func runLaunchdAlias(ctx context.Context, args []string, configPath string) error {
	fmt.Fprintf(stderr, "riggs: `launchd` is now `service`, which also supervises systemd on Linux. Forwarding.\n")
	if runtime.GOOS != "darwin" {
		fmt.Fprintf(stderr, "riggs: this machine is %s, so `service` will use its own init rather than launchd.\n",
			runtime.GOOS)
	}
	return runService(ctx, args, configPath)
}
