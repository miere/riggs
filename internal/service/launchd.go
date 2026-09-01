package service

import (
	"context"

	"github.com/miere/riggs-mcp/internal/launchd"
)

// The macOS half, which already existed.
//
// `internal/launchd` was written when the daemon was the only thing that had to
// keep running, and it is unchanged: the plist it writes, the bootout-first
// install, the captured PATH and the XML escaping are all still exactly right.
// This is an adapter, not a rewrite — reimplementing a working supervisor to
// fit a new interface is how a working supervisor stops working.

// launchdManager adapts *launchd.Manager to Manager.
type launchdManager struct {
	inner *launchd.Manager
}

// newLaunchd wraps the existing macOS manager.
func newLaunchd(runner Runner, opts Options) Manager {
	return &launchdManager{inner: launchd.New(runner, launchd.Options{
		BinaryPath: opts.BinaryPath,
		ConfigPath: opts.ConfigPath,
		Profile:    opts.Profile,
		Path:       opts.Path,
		HomeDir:    opts.HomeDir,
		UID:        opts.UID,
	})}
}

// Name is the supervisor.
func (m *launchdManager) Name() string { return "launchd" }

// UnitPath is the launch agent's plist.
func (m *launchdManager) UnitPath() string { return m.inner.PlistPath() }

// Install writes the plist and loads the agent.
func (m *launchdManager) Install(ctx context.Context) error { return m.inner.Install(ctx) }

// Uninstall stops the agent and removes its plist.
func (m *launchdManager) Uninstall(ctx context.Context) error { return m.inner.Uninstall(ctx) }

// Restart asks launchd to bring the daemon back.
func (m *launchdManager) Restart(ctx context.Context) error { return m.inner.Restart(ctx) }

// Status reports what launchd knows.
//
// There is no Warning to fill in. launchd agents have no equivalent of systemd's
// lingering: a LaunchAgent in the GUI domain runs whenever the user is logged
// in, and stops when they are not, which is what everyone already expects it to
// do.
func (m *launchdManager) Status(ctx context.Context) (Status, error) {
	inner, err := m.inner.Status(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Supervisor: m.Name(),
		Installed:  inner.Installed,
		UnitPath:   inner.PlistPath,
		LogDir:     inner.LogDir,
		Detail:     inner.Detail,
	}, nil
}
