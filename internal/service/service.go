// Package service supervises `riggs daemon` on whichever init the machine
// actually has.
//
// It exists because the schedule moved into the daemon. While Murtaugh owned
// the cron, a Riggs whose daemon was down lost its buttons and nothing else; now
// it loses its schedule too, so "keep this process running" stopped being a
// macOS convenience and became the one piece of setup the whole design rests on.
//
// Two supervisors, one command. `riggs service install` writes a launch agent on
// macOS and a systemd user unit on Linux, and the caller never asks which. That
// is the whole of the platform-specific surface: everything above this package
// is identical on both, because the scheduler is a ticker in a Go process and
// has no opinion about what started it.
//
// What this deliberately does NOT do is supervise individual jobs. One unit per
// job would mean translating a schedule into launchd's StartCalendarInterval
// (which has no ranges and no steps) and into systemd's OnCalendar (which has
// its own syntax again), and a cron expression maps cleanly onto neither. One
// supervised process, scheduling its own work, has no such problem.
package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Label is the supervised unit's name on both platforms, modulo each one's
// conventions: `io.riggs.daemon` for launchd, `riggs-daemon.service` for
// systemd.
const Label = "riggs-daemon"

// Runner executes the supervisor's CLI. A seam, so an install can be tested
// without touching the machine's real init.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the live Runner.
type ExecRunner struct{}

// Run executes a command and returns its combined output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Options describe the daemon to supervise. They are the union of what the two
// supervisors need, which is very nearly the same list.
type Options struct {
	// BinaryPath is the riggs executable to run. Absolute: neither supervisor
	// has a working directory or a PATH worth relying on.
	BinaryPath string
	// ConfigPath is passed as --config-file, so the daemon reads the same
	// config — and therefore the same ledger, and therefore the same jobs — as
	// the CLI that installed it.
	ConfigPath string
	// Profile is passed as --slack-profile. Empty leaves it to the default.
	Profile string
	// Path is the PATH the daemon runs with.
	//
	// Neither supervisor gives a useful one. launchd's default has no Homebrew
	// and no ~/.local/bin; a systemd user unit gets a similarly minimal set.
	// Riggs shells out to `gh` for its GitHub token and to the AI harness for a
	// review, so without this the daemon connects perfectly and fails on the
	// first thing it tries to run. Empty captures the PATH of whatever ran the
	// install, which is a shell that could find this binary.
	Path string
	// HomeDir overrides the user's home; tests set it.
	HomeDir string
	// UID is the user id to install for. Zero takes the current user's.
	UID int
	// User is the login name, used to ask systemd about lingering. Empty takes
	// the current user's.
	User string
}

// Status is what a supervisor reports about the daemon.
type Status struct {
	// Supervisor names what is in charge: "launchd" or "systemd".
	Supervisor string
	// Installed reports whether the unit file is on disk.
	Installed bool
	// UnitPath is where that file lives.
	UnitPath string
	// LogDir is where the daemon's output goes, when the supervisor writes it
	// to files. systemd sends it to the journal, and this is empty there.
	LogDir string
	// Detail is the supervisor's own summary, trimmed to the lines anybody
	// reads.
	Detail string
	// Warning is a problem that does not stop the install but will stop the
	// daemon — today, exactly one: systemd user units do not survive logout
	// without lingering enabled.
	Warning string
}

// Manager installs, removes and inspects the supervised daemon.
type Manager interface {
	// Name is the supervisor this manages.
	Name() string
	// Install writes the unit and starts it. Idempotent.
	Install(ctx context.Context) error
	// Uninstall stops the daemon and removes the unit. A missing unit is
	// success: the intent was for it not to be there.
	Uninstall(ctx context.Context) error
	// Restart stops the daemon and starts it again.
	Restart(ctx context.Context) error
	// Status reports what the supervisor knows.
	Status(ctx context.Context) (Status, error)
	// UnitPath is where the unit file lives.
	UnitPath() string
}

// New builds the Manager for this machine.
//
// The decision is made at RUN TIME on runtime.GOOS, not by build tag, for the
// reason `riggs launchd` already established: a command that vanishes from the
// usage line on the other platform cannot explain itself, and "there is no such
// command" is a worse answer than "this machine has no systemd".
func New(runner Runner, opts Options) (Manager, error) {
	if runner == nil {
		runner = ExecRunner{}
	}
	opts = opts.resolved()
	switch runtime.GOOS {
	case "darwin":
		return newLaunchd(runner, opts), nil
	case "linux":
		return newSystemd(runner, opts)
	default:
		return nil, fmt.Errorf("service: %s is not supported; supervise `riggs daemon` with your init of choice",
			runtime.GOOS)
	}
}

// resolved fills in what the caller left to the machine.
func (o Options) resolved() Options {
	if o.Path == "" {
		o.Path = os.Getenv("PATH")
	}
	if o.HomeDir == "" {
		o.HomeDir, _ = os.UserHomeDir()
	}
	if o.UID == 0 {
		o.UID = os.Getuid()
	}
	if o.User == "" {
		o.User = os.Getenv("USER")
		if o.User == "" {
			o.User = os.Getenv("LOGNAME")
		}
	}
	return o
}

// trimLines keeps the lines of out whose first word is one the reader wants,
// which is how both supervisors' verbose status output is made readable.
func trimLines(out string, prefixes ...string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range prefixes {
			if strings.HasPrefix(trimmed, prefix) {
				kept = append(kept, trimmed)
				break
			}
		}
	}
	return strings.Join(kept, "\n")
}

// MissingTools names the binaries the daemon shells out to that will not be on
// the supervised PATH.
//
// Reported at install time rather than discovered on the first click, from a
// log nobody is watching. It is the same check `riggs launchd install` already
// made, moved here because a systemd user unit has exactly the same problem: it
// inherits a minimal PATH, and `gh` is in neither /usr/bin nor /bin on most
// machines.
//
// harness is the configured AI command, checked alongside `gh` because it is
// now a thing a scheduled job can reach for. Empty skips it — an unconfigured
// harness is not a missing one.
func MissingTools(path, harness string) []string {
	var missing []string
	for _, name := range append([]string{"gh"}, harnessProgram(harness)...) {
		if !onPath(name, path) {
			missing = append(missing, name)
		}
	}
	return missing
}

// harnessProgram is the AI command's program, as a zero-or-one element slice so
// it composes with the fixed list above.
func harnessProgram(command string) []string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return nil
	}
	return fields[:1]
}

// onPath reports whether name resolves to an executable on the given PATH.
//
// An absolute or relative path — which a configured harness may well be — is
// checked directly rather than searched for.
func onPath(name, path string) bool {
	if strings.ContainsRune(name, os.PathSeparator) {
		info, err := os.Stat(name)
		return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}
