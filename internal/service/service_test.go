package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records the supervisor commands instead of running them.
type fakeRunner struct {
	ran  [][]string
	out  map[string]string
	fail map[string]bool
}

func newRunner() *fakeRunner {
	return &fakeRunner{out: map[string]string{}, fail: map[string]bool{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.ran = append(f.ran, append([]string{name}, args...))
	if f.fail[key(args)] {
		return []byte("nope"), os.ErrPermission
	}
	return []byte(f.out[key(args)]), nil
}

// key is the subcommand a fake answer is registered under: `systemctl --user
// show …` is keyed on "show".
func key(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

func (f *fakeRunner) didRun(sub string) bool {
	for _, cmd := range f.ran {
		for _, a := range cmd {
			if a == sub {
				return true
			}
		}
	}
	return false
}

// systemd builds a manager whose unit path is guaranteed to be inside a
// temporary directory.
//
// The pinning is not tidiness. UnitPath honours $XDG_CONFIG_HOME — deliberately,
// because systemd does — so on any machine that sets it, a test that installs
// would write a real unit file into the developer's own config directory and
// leave it there. Setting HomeDir is not enough to prevent that, which is
// exactly what CI caught.
func systemd(t *testing.T, runner Runner, opts Options) *systemdManager {
	t.Helper()
	if opts.HomeDir == "" {
		opts.HomeDir = t.TempDir()
	}
	if opts.BinaryPath == "" {
		opts.BinaryPath = "/usr/local/bin/riggs"
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(opts.HomeDir, ".config"))
	return &systemdManager{runner: runner, opts: opts.resolved()}
}

// The unit is the whole Linux story, so its three load-bearing settings are
// asserted rather than eyeballed.
func TestTheSystemdUnitRestartsAlwaysAndCarriesAPath(t *testing.T) {
	m := systemd(t, newRunner(), Options{
		ConfigPath: "/home/m/.config/riggs/config.yaml",
		Profile:    "riggs",
		Path:       "/opt/homebrew/bin:/usr/bin",
	})
	unit := string(m.unit())

	// Restart=always, not on-failure: the daemon exits CLEANLY when its socket
	// closes, so on-failure would leave a disconnected daemon down.
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("unit does not restart unconditionally:\n%s", unit)
	}
	if !strings.Contains(unit, "RestartSec=") {
		t.Errorf("unit has no restart throttle:\n%s", unit)
	}
	// Without this the daemon starts perfectly and fails on the first thing it
	// shells out to.
	if !strings.Contains(unit, `Environment="PATH=/opt/homebrew/bin:/usr/bin"`) {
		t.Errorf("unit carries no PATH:\n%s", unit)
	}
	// A user unit, so a user target.
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("unit installs into the wrong target:\n%s", unit)
	}
	if !strings.Contains(unit, `ExecStart="/usr/local/bin/riggs" "daemon" "--config-file" "/home/m/.config/riggs/config.yaml" "--slack-profile" "riggs"`) {
		t.Errorf("ExecStart = wrong:\n%s", unit)
	}
}

// A home directory with a space in it is unusual and entirely legal, and
// systemd splits ExecStart on whitespace.
func TestTheUnitQuotesEveryArgument(t *testing.T) {
	m := systemd(t, newRunner(), Options{
		BinaryPath: "/home/Miere Fernandes/bin/riggs",
		ConfigPath: "/home/Miere Fernandes/.config/riggs/config.yaml",
	})
	unit := string(m.unit())
	if !strings.Contains(unit, `"/home/Miere Fernandes/bin/riggs"`) {
		t.Errorf("the binary path was not quoted:\n%s", unit)
	}
}

// systemd honours XDG_CONFIG_HOME, so installing to ~/.config on a machine that
// has moved it writes a file systemd never reads.
func TestTheUnitPathFollowsXDG(t *testing.T) {
	home := t.TempDir()
	m := systemd(t, newRunner(), Options{HomeDir: home})

	// Unset, it falls back to ~/.config. Set explicitly rather than assumed:
	// the variable is populated on plenty of machines, CI's included.
	t.Setenv("XDG_CONFIG_HOME", "")
	if want := filepath.Join(home, ".config", "systemd", "user", unitName); m.UnitPath() != want {
		t.Fatalf("UnitPath = %q, want %q", m.UnitPath(), want)
	}

	// Set, it wins — because systemd itself honours it, and installing to
	// ~/.config on a machine that has moved it writes a file systemd never
	// reads.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if want := filepath.Join(xdg, "systemd", "user", unitName); m.UnitPath() != want {
		t.Fatalf("UnitPath = %q, want %q", m.UnitPath(), want)
	}
}

// daemon-reload before enable: systemd will not enable a unit it has not read.
func TestInstallWritesReloadsThenEnables(t *testing.T) {
	runner := newRunner()
	m := systemd(t, runner, Options{})
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(m.UnitPath()); err != nil {
		t.Fatalf("the unit was not written: %v", err)
	}
	if len(runner.ran) < 2 {
		t.Fatalf("ran %v", runner.ran)
	}
	if key(runner.ran[0][1:]) != "daemon-reload" || key(runner.ran[1][1:]) != "enable" {
		t.Fatalf("order = %v, want daemon-reload then enable", runner.ran)
	}
}

// Disabling a unit systemd can no longer read leaves the symlink behind, and
// the next daemon-reload complains about it forever.
func TestUninstallDisablesBeforeRemoving(t *testing.T) {
	runner := newRunner()
	m := systemd(t, runner, Options{})
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	runner.ran = nil

	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if key(runner.ran[0][1:]) != "disable" {
		t.Fatalf("first command = %v, want disable", runner.ran[0])
	}
	if _, err := os.Stat(m.UnitPath()); !os.IsNotExist(err) {
		t.Fatalf("the unit survived: %v", err)
	}
	// A missing unit is success: the intent was for it not to be there.
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatalf("uninstalling twice: %v", err)
	}
}

// The nastiest failure this feature has: works all afternoon, gone by morning,
// nothing in the log to say why.
func TestStatusWarnsWhenLingeringIsOff(t *testing.T) {
	runner := newRunner()
	runner.out["show-user"] = "Linger=no\n"
	m := systemd(t, runner, Options{User: "miere"})
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(status.Warning, "log out") {
		t.Fatalf("warning = %q, want it to explain the symptom", status.Warning)
	}
	// And the exact command, because it needs root and nobody remembers it.
	if !strings.Contains(status.Warning, "loginctl enable-linger miere") {
		t.Fatalf("warning = %q, want the fix", status.Warning)
	}

	runner.out["show-user"] = "Linger=yes\n"
	status, _ = m.Status(context.Background())
	if status.Warning != "" {
		t.Fatalf("warning with lingering on: %q", status.Warning)
	}
}

// "Not installed" is a legitimate answer to the question being asked.
func TestStatusOnAnUninstalledUnit(t *testing.T) {
	m := systemd(t, newRunner(), Options{})
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Installed {
		t.Fatal("reported installed with no unit file")
	}
	if status.Supervisor != "systemd" {
		t.Fatalf("supervisor = %q", status.Supervisor)
	}
}

// loginctl missing, or a user it does not know: not worth a warning of its own,
// since the daemon works either way while somebody is logged in.
func TestAMissingLoginctlIsNotAWarning(t *testing.T) {
	runner := newRunner()
	runner.fail["show-user"] = true
	m := systemd(t, runner, Options{User: "miere"})
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	status, _ := m.Status(context.Background())
	if status.Warning != "" {
		t.Fatalf("warning = %q, want none", status.Warning)
	}
}

// Reported at install time rather than discovered on the first job, from a log
// nobody is watching.
func TestMissingToolsChecksGhAndTheHarness(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "gh"))

	if missing := MissingTools(dir, ""); len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}
	// The harness joins the check now that a scheduled job can reach for it.
	if missing := MissingTools(dir, "claude --model opus"); len(missing) != 1 || missing[0] != "claude" {
		t.Fatalf("missing = %v, want claude", missing)
	}
	if missing := MissingTools(t.TempDir(), ""); len(missing) != 1 || missing[0] != "gh" {
		t.Fatalf("missing = %v, want gh", missing)
	}
	// An absolute harness path is checked directly rather than searched for.
	harness := filepath.Join(dir, "my-agent")
	writeExecutable(t, harness)
	if missing := MissingTools(dir, harness); len(missing) != 0 {
		t.Fatalf("missing = %v, want none for an absolute path that exists", missing)
	}
	if missing := MissingTools(dir, "/nowhere/my-agent"); len(missing) != 1 {
		t.Fatalf("missing = %v, want the absent absolute path", missing)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// systemd may be installed without being PID 1 — the state inside many
// containers and under WSL1, where every systemctl call fails with a message
// about D-Bus that explains nothing. Refusing beats half-installing.
func TestARefusalWhenSystemdIsNotInCharge(t *testing.T) {
	original := systemdIsPID1
	t.Cleanup(func() { systemdIsPID1 = original })
	systemdIsPID1 = func() bool { return false }

	_, err := newSystemd(newRunner(), Options{BinaryPath: "/usr/local/bin/riggs"}.resolved())
	if err == nil {
		t.Fatal("newSystemd accepted a machine with no systemd")
	}
	// It says what to do instead, rather than only what is wrong.
	if !strings.Contains(err.Error(), "not running systemd") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "/usr/local/bin/riggs daemon") {
		t.Errorf("err = %v, want it to name the command to supervise", err)
	}

	systemdIsPID1 = func() bool { return true }
	if _, err := newSystemd(newRunner(), Options{}.resolved()); err != nil {
		t.Fatalf("newSystemd on a systemd machine: %v", err)
	}
}
