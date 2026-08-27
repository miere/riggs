// Package launchd supervises `riggs daemon` as a macOS launch agent.
//
// The daemon is the first part of Riggs that has to be *running* rather than
// invoked. Every other mode is a one-shot Murtaugh starts and waits for; this
// one has to survive a logout, a crash and a reboot, and nothing in the design
// so far had an opinion about that.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Label is the launch agent's identifier, and the base name of its plist.
const Label = "io.riggs.daemon"

// Runner executes launchctl. It is a seam so the install path can be tested
// without touching the machine's real launchd.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ExecRunner is the live Runner.
type ExecRunner struct{}

// Run executes a command and returns its combined output.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Options describe the agent to install.
type Options struct {
	// BinaryPath is the riggs executable the agent runs. Absolute: launchd has
	// no working directory or PATH worth relying on.
	BinaryPath string
	// ConfigPath is passed as --config-file, so the agent reads the same config
	// (and therefore the same ledger) as the CLI.
	ConfigPath string
	// Profile is passed as --slack-profile. Empty leaves it to the default.
	Profile string
	// Path is the PATH the agent runs with.
	//
	// A launch agent inherits nothing, and launchd's default PATH is
	// /usr/bin:/bin:/usr/sbin:/sbin — which contains no Homebrew and no
	// ~/.local/bin. Riggs shells out to `gh` for its GitHub token, so without
	// this the daemon connects perfectly and then fails on the first click with
	// "executable file not found in $PATH".
	//
	// Empty captures the PATH of whatever ran `riggs launchd install`, which is
	// a shell that could find this binary and can almost certainly find the rest.
	Path string
	// HomeDir overrides the user's home; tests set it.
	HomeDir string
	// UID is the user id the agent is bootstrapped for. Zero takes the current
	// user's.
	UID int
}

// Manager installs, removes and inspects the launch agent.
type Manager struct {
	runner Runner
	opts   Options
}

// New builds a Manager.
func New(runner Runner, opts Options) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	if opts.Path == "" {
		opts.Path = os.Getenv("PATH")
	}
	if opts.HomeDir == "" {
		opts.HomeDir, _ = os.UserHomeDir()
	}
	if opts.UID == 0 {
		opts.UID = os.Getuid()
	}
	return &Manager{runner: runner, opts: opts}
}

// PlistPath is where the agent's plist lives.
func (m *Manager) PlistPath() string {
	return filepath.Join(m.opts.HomeDir, "Library", "LaunchAgents", Label+".plist")
}

// LogDir is where the agent's stdout and stderr are written.
func (m *Manager) LogDir() string {
	return filepath.Join(m.opts.HomeDir, "Library", "Logs", "riggs")
}

// target is the launchd service target for the current GUI session.
func (m *Manager) target() string {
	return fmt.Sprintf("gui/%d/%s", m.opts.UID, Label)
}

// domain is the launchd domain the agent is bootstrapped into.
func (m *Manager) domain() string {
	return fmt.Sprintf("gui/%d", m.opts.UID)
}

// Install writes the plist and (re)loads the agent.
//
// It is idempotent: an already-loaded agent is booted out first, so running
// this after changing the profile or upgrading the binary picks the change up
// rather than silently leaving the old one running.
func (m *Manager) Install(ctx context.Context) error {
	if err := Supported(); err != nil {
		return err
	}
	if m.opts.BinaryPath == "" {
		return fmt.Errorf("launchd: no riggs binary path to run")
	}

	if err := os.MkdirAll(filepath.Dir(m.PlistPath()), 0o755); err != nil {
		return fmt.Errorf("launchd: creating LaunchAgents: %w", err)
	}
	// launchd will not create these, and a missing log directory makes the
	// agent fail to spawn with a message only `launchctl print` will show you.
	if err := os.MkdirAll(m.LogDir(), 0o755); err != nil {
		return fmt.Errorf("launchd: creating %s: %w", m.LogDir(), err)
	}

	plist, err := m.Plist()
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.PlistPath(), plist, 0o644); err != nil {
		return fmt.Errorf("launchd: writing %s: %w", m.PlistPath(), err)
	}

	// Boot out any previous incarnation. It is expected to fail when nothing is
	// loaded, which is why the error is dropped rather than reported.
	_, _ = m.runner.Run(ctx, "launchctl", "bootout", m.target())

	if out, err := m.runner.Run(ctx, "launchctl", "bootstrap", m.domain(), m.PlistPath()); err != nil {
		return fmt.Errorf("launchd: bootstrap failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := m.runner.Run(ctx, "launchctl", "kickstart", "-k", m.target()); err != nil {
		return fmt.Errorf("launchd: kickstart failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops the agent and removes its plist.
//
// A missing plist is success: the intent was for the agent not to be there.
func (m *Manager) Uninstall(ctx context.Context) error {
	if err := Supported(); err != nil {
		return err
	}
	_, _ = m.runner.Run(ctx, "launchctl", "bootout", m.target())

	if err := os.Remove(m.PlistPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("launchd: removing %s: %w", m.PlistPath(), err)
	}
	return nil
}

// MissingTools reports which of the executables the daemon shells out to are
// not resolvable on the PATH the agent will run with.
//
// It is checked at install time because the alternative is finding out on the
// first click, from a log nobody is watching — which is exactly how the PATH
// gap was discovered.
func (m *Manager) MissingTools() []string {
	var missing []string
	// `gh` alone: the card bodies used to shell out to `claude`, and no longer
	// do (§7d).
	for _, name := range []string{"gh"} {
		if !onPath(name, m.opts.Path) {
			missing = append(missing, name)
		}
	}
	return missing
}

// onPath reports whether name resolves to an executable on the given PATH.
func onPath(name, path string) bool {
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

// Status reports what launchd knows about the agent.
type Status struct {
	Installed bool   `json:"installed"`
	PlistPath string `json:"plist_path"`
	LogDir    string `json:"log_dir"`
	// Detail is launchctl's own output, which is the only honest answer to
	// "why is it not running".
	Detail string `json:"detail,omitempty"`
}

// String renders the status for a human.
func (s Status) String() string {
	if !s.Installed {
		return fmt.Sprintf("not installed (no plist at %s)", s.PlistPath)
	}
	out := fmt.Sprintf("installed at %s\nlogs in %s", s.PlistPath, s.LogDir)
	if s.Detail != "" {
		out += "\n\n" + s.Detail
	}
	return out
}

// Status inspects the agent.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	if err := Supported(); err != nil {
		return Status{}, err
	}
	s := Status{PlistPath: m.PlistPath(), LogDir: m.LogDir()}
	if _, err := os.Stat(s.PlistPath); err != nil {
		return s, nil
	}
	s.Installed = true

	// A failure here is not a failure of the command: "launchd does not know
	// about it" is a legitimate answer to the question being asked.
	out, err := m.runner.Run(ctx, "launchctl", "print", m.target())
	if err != nil {
		s.Detail = "launchd does not have this agent loaded"
		return s, nil
	}
	s.Detail = summarise(string(out))
	return s, nil
}

// summarise keeps the handful of lines from `launchctl print` anyone actually
// reads. Its full output is hundreds of lines of domain state.
func summarise(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, prefix := range []string{"state =", "pid =", "last exit code =", "runs ="} {
			if strings.HasPrefix(trimmed, prefix) {
				kept = append(kept, trimmed)
			}
		}
	}
	return strings.Join(kept, "\n")
}

// Plist renders the launch agent property list.
func (m *Manager) Plist() ([]byte, error) {
	args := []string{m.opts.BinaryPath, "daemon"}
	if m.opts.ConfigPath != "" {
		args = append(args, "--config-file", m.opts.ConfigPath)
	}
	if m.opts.Profile != "" {
		args = append(args, "--slack-profile", m.opts.Profile)
	}

	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writeString(&b, "Label", Label)

	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, a := range args {
		b.WriteString("\t\t<string>" + escape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")

	// Always restart. The daemon exits cleanly when its socket closes, so
	// SuccessfulExit=false would leave a disconnected daemon down until someone
	// noticed. `launchctl bootout` still stops it regardless.
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	// Without a throttle, a daemon that cannot start — bad token, no network —
	// respawns as fast as launchd can fork it.
	b.WriteString("\t<key>ThrottleInterval</key>\n\t<integer>10</integer>\n")
	b.WriteString("\t<key>ProcessType</key>\n\t<string>Background</string>\n")

	if m.opts.Path != "" {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		b.WriteString("\t\t<key>PATH</key>\n\t\t<string>" + escape(m.opts.Path) + "</string>\n")
		b.WriteString("\t</dict>\n")
	}

	writeString(&b, "StandardOutPath", filepath.Join(m.LogDir(), "daemon.log"))
	writeString(&b, "StandardErrorPath", filepath.Join(m.LogDir(), "daemon.err.log"))
	writeString(&b, "WorkingDirectory", m.opts.HomeDir)

	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func writeString(b *bytes.Buffer, key, value string) {
	b.WriteString("\t<key>" + escape(key) + "</key>\n\t<string>" + escape(value) + "</string>\n")
}

// escape XML-escapes a plist value. A path with an ampersand in it is unusual
// and entirely legal, and would otherwise produce a plist launchd silently
// refuses to parse.
func escape(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}

// Supported reports whether launchd is available on this platform.
//
// It is a runtime check rather than a build tag so the command exists
// everywhere and explains itself, instead of vanishing from the usage line on
// Linux.
func Supported() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd is macOS-only; on %s supervise `riggs daemon` with systemd or your init of choice", runtime.GOOS)
	}
	return nil
}

// UIDString renders the uid, for diagnostics.
func (m *Manager) UIDString() string { return strconv.Itoa(m.opts.UID) }
