package main

import (
	"context"
	"fmt"
	"os"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/launchd"
)

// launchdUsage is printed for a missing or unknown subcommand.
const launchdUsage = "usage: riggs launchd <install|uninstall|status> [--slack-profile <name>]"

// runLaunchd supervises the daemon as a macOS launch agent.
func runLaunchd(ctx context.Context, args []string, configPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", launchdUsage)
	}
	action, rest := args[0], args[1:]

	profile, err := launchdProfile(rest)
	if err != nil {
		return err
	}

	// The plist has to name a config path explicitly. The agent inherits none
	// of this shell's environment, so $RIGGS_CONFIG would not reach it and the
	// precedence chain would resolve somewhere else entirely.
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary (needed as the agent's program): %w", err)
	}

	manager := launchd.New(nil, launchd.Options{
		BinaryPath: self,
		ConfigPath: configPath,
		Profile:    profile,
	})

	switch action {
	case "install":
		if err := manager.Install(ctx); err != nil {
			return err
		}
		fmt.Printf("Installed %s\n", manager.PlistPath())
		fmt.Printf("Logs: %s\n", manager.LogDir())
		return nil

	case "uninstall":
		if err := manager.Uninstall(ctx); err != nil {
			return err
		}
		fmt.Printf("Removed %s\n", manager.PlistPath())
		return nil

	case "status":
		status, err := manager.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Println(status)
		return nil

	default:
		return fmt.Errorf("unknown launchd action %q; %s", action, launchdUsage)
	}
}

// launchdProfile reads --slack-profile, which is baked into the plist so the
// agent listens as the same app the digest is posted by.
func launchdProfile(args []string) (string, error) {
	const flag = "--slack-profile"
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag:
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a profile name", flag)
			}
			return args[i+1], nil
		case len(args[i]) > len(flag)+1 && args[i][:len(flag)+1] == flag+"=":
			return args[i][len(flag)+1:], nil
		default:
			return "", fmt.Errorf("unexpected argument %q; %s", args[i], launchdUsage)
		}
	}
	return "", nil
}
