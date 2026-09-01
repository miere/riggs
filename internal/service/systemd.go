package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The Linux half: a systemd USER unit, not a system one.
//
// A user unit because everything Riggs touches belongs to one person — their
// Slack tokens, their `gh` login, their AI harness, their home directory. A
// system unit would run as root and then have to be told how to become them,
// which is a great deal of ceremony for a single-user tool and one more way to
// end up with a daemon that cannot read its own config.
//
// The cost of that choice is the one thing about systemd that surprises people,
// and it is why this package checks for it: **a user unit stops when the user
// logs out**, unless lingering is enabled. It is the nastiest kind of failure —
// works all afternoon, gone by morning, nothing in the log to say why — so the
// install reports it rather than leaving it to be discovered.

// unitName is the systemd unit, and the file it lives in.
const unitName = Label + ".service"

// systemdManager supervises the daemon through `systemctl --user`.
type systemdManager struct {
	runner Runner
	opts   Options
}

// newSystemd builds the manager, refusing a machine that has no systemd rather
// than half-installing onto one.
//
// Both checks matter and neither is sufficient. `systemctl` on PATH says the
// tools are installed; `/run/systemd/system` says systemd is actually PID 1,
// which it is not inside many containers or under WSL1 — and there, every
// systemctl call fails with a message about D-Bus that explains nothing.
func newSystemd(runner Runner, opts Options) (Manager, error) {
	m := &systemdManager{runner: runner, opts: opts}
	if !systemdIsPID1() {
		return nil, fmt.Errorf(
			"this machine is not running systemd, so there is nothing here to install into.\n"+
				"Run `%s daemon` under whatever supervisor you do have — it needs to stay up, "+
				"restart on exit, and keep the PATH it was started with.", opts.BinaryPath)
	}
	return m, nil
}

// systemdIsPID1 reports whether systemd is actually running this machine.
//
// A package variable so the refusal above can be tested on a developer's laptop,
// which is the one place it will never happen naturally.
var systemdIsPID1 = func() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// Name is the supervisor.
func (m *systemdManager) Name() string { return "systemd" }

// UnitPath is where the user unit lives.
//
// $XDG_CONFIG_HOME is honoured because systemd itself honours it: installing to
// ~/.config on a machine that has moved it would write a file systemd never
// reads, and the only symptom is a unit that does not exist.
func (m *systemdManager) UnitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(m.opts.HomeDir, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitName)
}

// Install writes the unit and starts it.
//
// Idempotent by construction: writing the file, reloading, and `enable --now`
// all do the right thing whether or not something was there before, which is
// the same guarantee the launchd path gives by booting out first.
func (m *systemdManager) Install(ctx context.Context) error {
	path := m.UnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("service: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, m.unit(), 0o644); err != nil {
		return fmt.Errorf("service: writing %s: %w", path, err)
	}
	// Before enable, not after: systemd will not enable a unit it has not read.
	if out, err := m.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("service: daemon-reload failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := m.runner.Run(ctx, "systemctl", "--user", "enable", "--now", unitName); err != nil {
		return fmt.Errorf("service: enabling %s failed: %w: %s", unitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops the daemon and removes the unit.
//
// `disable --now` before the file is removed: disabling a unit systemd can no
// longer read leaves the symlink in .wants behind, and the next daemon-reload
// complains about it forever. A failure is ignored for the same reason the
// launchd path ignores a failed bootout — the unit may not have been loaded,
// and the intent is for it to be gone either way.
func (m *systemdManager) Uninstall(ctx context.Context) error {
	_, _ = m.runner.Run(ctx, "systemctl", "--user", "disable", "--now", unitName)
	if err := os.Remove(m.UnitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("service: removing %s: %w", m.UnitPath(), err)
	}
	if out, err := m.runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("service: daemon-reload failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Restart brings the daemon back, which is how it picks up a binary that has
// just been replaced (§7e).
func (m *systemdManager) Restart(ctx context.Context) error {
	out, err := m.runner.Run(ctx, "systemctl", "--user", "restart", unitName)
	if err != nil {
		return fmt.Errorf("service: restarting %s failed: %w: %s", unitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Status reports what systemd knows, plus the lingering warning.
func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	s := Status{Supervisor: m.Name(), UnitPath: m.UnitPath()}
	if _, err := os.Stat(s.UnitPath); err != nil {
		return s, nil
	}
	s.Installed = true

	// A failure here is not a failure of the command: "systemd does not have
	// this loaded" is a legitimate answer to the question being asked.
	out, err := m.runner.Run(ctx, "systemctl", "--user", "show", unitName,
		"--property=ActiveState,SubState,MainPID,NRestarts,ExecMainStatus")
	if err != nil {
		s.Detail = "systemd does not have this unit loaded"
	} else {
		s.Detail = trimLines(string(out),
			"ActiveState=", "SubState=", "MainPID=", "NRestarts=", "ExecMainStatus=")
	}
	s.Warning = m.lingerWarning(ctx)
	return s, nil
}

// lingerWarning reports the one thing about a user unit that surprises people.
//
// Checked rather than assumed, and reported rather than fixed: enabling
// lingering needs polkit or root, and a tool that silently escalated to change
// a login-manager setting would be doing something nobody asked it to.
func (m *systemdManager) lingerWarning(ctx context.Context) string {
	if m.opts.User == "" {
		return ""
	}
	out, err := m.runner.Run(ctx, "loginctl", "show-user", m.opts.User, "--property=Linger")
	if err != nil {
		// loginctl missing, or a user it does not know. Not worth a warning of
		// its own — the daemon works either way while somebody is logged in.
		return ""
	}
	if strings.Contains(string(out), "Linger=yes") {
		return ""
	}
	return fmt.Sprintf(
		"lingering is off for %s, so this daemon stops when you log out and no job runs until you log back in.\n"+
			"      Enable it with: sudo loginctl enable-linger %s", m.opts.User, m.opts.User)
}

// unit renders the systemd user unit.
//
// The three settings that matter, and why each is what it is:
//
//   - Restart=always, not on-failure. The daemon exits CLEANLY when its socket
//     closes, so restarting only on failure would leave a disconnected daemon
//     down until somebody noticed. Exactly the reasoning behind launchd's
//     unconditional KeepAlive (§12b).
//   - RestartSec, so a daemon that cannot start — bad token, no network — does
//     not respawn as fast as systemd can fork it. launchd's ThrottleInterval.
//   - Environment=PATH, because a user unit inherits a minimal one and `gh`
//     lives in neither /usr/bin nor /bin on most machines.
func (m *systemdManager) unit() []byte {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Riggs: Slack digests, review actions and the job schedule\n")
	b.WriteString("Documentation=https://github.com/miere/riggs\n")
	// Not network-online.target: that is a system target, and a user unit
	// cannot order itself against one. The daemon reconnects on its own anyway,
	// which is what its Socket Mode client is for.
	b.WriteString("After=default.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", quoteArgv(m.argv()))
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=10\n")
	if path := strings.TrimSpace(m.opts.Path); path != "" {
		fmt.Fprintf(&b, "Environment=%s\n", quoteValue("PATH="+path))
	}
	b.WriteString("\n[Install]\n")
	// default.target, not multi-user.target: this is a user unit, and
	// multi-user.target is the system's.
	b.WriteString("WantedBy=default.target\n")
	return []byte(b.String())
}

// argv is the command the unit runs.
func (m *systemdManager) argv() []string {
	argv := []string{m.opts.BinaryPath, "daemon"}
	if m.opts.ConfigPath != "" {
		argv = append(argv, "--config-file", m.opts.ConfigPath)
	}
	if m.opts.Profile != "" {
		argv = append(argv, "--slack-profile", m.opts.Profile)
	}
	return argv
}

// quoteArgv renders an argument list for ExecStart.
//
// Every element is quoted, unconditionally. systemd splits ExecStart on
// whitespace, so a home directory with a space in it — unusual and entirely
// legal — would otherwise become two arguments and a daemon that will not
// start. Quoting always means never having to be right about which one needed
// it, which is the same rule the config writer follows.
func quoteArgv(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		out = append(out, quoteValue(arg))
	}
	return strings.Join(out, " ")
}

// quoteValue renders one systemd-quoted string.
func quoteValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
