package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/service"
)

// serviceUsage is printed for a missing or unknown subcommand.
const serviceUsage = "usage: riggs service <install|uninstall|status|restart> [--slack-profile <name>]"

// runService supervises the daemon on whichever init this machine has.
//
// It replaces `riggs launchd`, which still works and forwards here. The rename
// is not cosmetic: the schedule now lives inside the daemon, so keeping that
// process running stopped being a macOS convenience and became the setup step
// the whole thing rests on — and a command named after one platform's
// supervisor is a poor place to put it.
func runService(ctx context.Context, args []string, configPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", serviceUsage)
	}
	action, rest := args[0], args[1:]

	profile, err := serviceProfile(rest)
	if err != nil {
		return err
	}
	// The unit has to name a config path explicitly. A supervised daemon
	// inherits none of this shell's environment, so $RIGGS_CONFIG would not
	// reach it and the precedence chain would resolve somewhere else entirely —
	// including, now, to a different ledger and therefore a different schedule.
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary (needed as the supervised program): %w", err)
	}

	manager, err := service.New(nil, service.Options{
		BinaryPath: self,
		ConfigPath: configPath,
		Profile:    profile,
	})
	if err != nil {
		return err
	}

	switch action {
	case "install":
		if err := manager.Install(ctx); err != nil {
			return err
		}
		fmt.Printf("Installed %s (%s)\n", manager.UnitPath(), manager.Name())
		reportInstallWarnings(ctx, manager, configPath)
		return nil

	case "uninstall":
		if err := manager.Uninstall(ctx); err != nil {
			return err
		}
		fmt.Printf("Removed %s\n", manager.UnitPath())
		return nil

	case "restart":
		if err := manager.Restart(ctx); err != nil {
			return err
		}
		fmt.Printf("Restarted the %s unit.\n", manager.Name())
		return nil

	case "status":
		status, err := manager.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Print(renderStatus(status))
		return nil

	default:
		return fmt.Errorf("unknown service action %q; %s", action, serviceUsage)
	}
}

// reportInstallWarnings says what will go wrong later, now.
//
// Both of these are silent failures otherwise. A missing `gh` produces a daemon
// that connects perfectly and fails on the first thing it runs; systemd
// lingering produces one that works all afternoon and is gone by morning.
func reportInstallWarnings(ctx context.Context, manager service.Manager, configPath string) {
	harness := ""
	// A config that will not load is not this command's problem to report — the
	// daemon will say so, loudly, on its first start — but it should not cost
	// the PATH check either.
	if cfg, err := config.Load(configPath); err == nil {
		harness = cfg.AICommand()
	}
	if missing := service.MissingTools(os.Getenv("PATH"), harness); len(missing) > 0 {
		fmt.Printf("\nWARNING: not on the supervised PATH: %s\n", strings.Join(missing, ", "))
		fmt.Printf("The daemon will start, then fail on the first job or click that needs them.\n")
		fmt.Printf("Re-run this from a shell where they resolve.\n")
	}
	status, err := manager.Status(ctx)
	if err == nil && status.Warning != "" {
		fmt.Printf("\nWARNING: %s\n", status.Warning)
	}
}

// renderStatus prints what the supervisor knows.
func renderStatus(s service.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "supervisor: %s\n", s.Supervisor)
	fmt.Fprintf(&b, "unit:       %s\n", s.UnitPath)
	if !s.Installed {
		b.WriteString("state:      not installed (run `riggs service install`)\n")
		return b.String()
	}
	if s.LogDir != "" {
		fmt.Fprintf(&b, "logs:       %s\n", s.LogDir)
	} else {
		fmt.Fprintf(&b, "logs:       journalctl --user -u %s\n", service.Label)
	}
	if s.Detail != "" {
		b.WriteString("\n" + s.Detail + "\n")
	}
	if s.Warning != "" {
		fmt.Fprintf(&b, "\nWARNING: %s\n", s.Warning)
	}
	return b.String()
}

// serviceProfile reads --slack-profile, which is baked into the unit so the
// daemon listens as the same app the digest is posted by.
func serviceProfile(args []string) (string, error) {
	const flag = "--slack-profile"
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == flag:
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a profile name", flag)
			}
			return args[i+1], nil
		case strings.HasPrefix(args[i], flag+"="):
			return strings.TrimPrefix(args[i], flag+"="), nil
		default:
			return "", fmt.Errorf("unexpected argument %q; %s", args[i], serviceUsage)
		}
	}
	return "", nil
}
