package launchd

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRunner records launchctl invocations and returns scripted results.
type fakeRunner struct {
	calls [][]string
	out   map[string][]byte
	err   map[string]error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{out: map[string][]byte{}, err: map[string]error{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if len(args) == 0 {
		return nil, nil
	}
	return f.out[args[0]], f.err[args[0]]
}

func (f *fakeRunner) verbs() []string {
	var out []string
	for _, c := range f.calls {
		if len(c) > 1 {
			out = append(out, c[1])
		}
	}
	return out
}

func newManager(t *testing.T, runner Runner, opts Options) *Manager {
	t.Helper()
	opts.HomeDir = t.TempDir()
	opts.UID = 501
	if opts.BinaryPath == "" {
		opts.BinaryPath = "/usr/local/bin/riggs"
	}
	return New(runner, opts)
}

func skipUnlessDarwin(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("launchd is macOS-only")
	}
}

// --- the plist ------------------------------------------------------------

func TestPlistIsWellFormedXML(t *testing.T) {
	m := newManager(t, newFakeRunner(), Options{
		ConfigPath: "/Users/x/.config/riggs/config.yaml", Profile: "riggs",
	})
	plist, err := m.Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}

	// A plist launchd cannot parse fails with a message only `launchctl print`
	// will show you, so this is checked here rather than discovered there.
	decoder := xml.NewDecoder(strings.NewReader(string(plist)))
	decoder.Strict = true
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist is not well-formed: %v\n%s", err, plist)
		}
	}
}

func TestPlistRunsTheDaemonWithItsConfigAndProfile(t *testing.T) {
	m := newManager(t, newFakeRunner(), Options{
		BinaryPath: "/opt/riggs",
		ConfigPath: "/etc/riggs/config.yaml",
		Profile:    "riggs",
	})
	plist, _ := m.Plist()
	got := string(plist)

	for _, want := range []string{
		"<string>/opt/riggs</string>",
		"<string>daemon</string>",
		"<string>--config-file</string>",
		"<string>/etc/riggs/config.yaml</string>",
		"<string>--slack-profile</string>",
		"<string>riggs</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist is missing %s\n%s", want, got)
		}
	}
}

// The config path must be explicit: the agent inherits none of the shell's
// environment, so $RIGGS_CONFIG would not reach it.
func TestPlistAlwaysNamesAConfigPath(t *testing.T) {
	m := newManager(t, newFakeRunner(), Options{ConfigPath: "/etc/riggs/config.yaml"})
	if !strings.Contains(string(mustPlist(t, m)), "--config-file") {
		t.Fatal("plist does not name a config file")
	}
}

// The daemon exits cleanly when its socket closes, so SuccessfulExit=false
// would leave a disconnected daemon down until someone noticed.
func TestPlistAlwaysRestarts(t *testing.T) {
	got := string(mustPlist(t, newManager(t, newFakeRunner(), Options{})))

	if !strings.Contains(got, "<key>KeepAlive</key>\n\t<true/>") {
		t.Errorf("plist does not keep the agent alive unconditionally:\n%s", got)
	}
	if strings.Contains(got, "SuccessfulExit") {
		t.Errorf("plist restarts only on failure:\n%s", got)
	}
	// Without a throttle, an agent that cannot start respawns as fast as
	// launchd can fork it.
	if !strings.Contains(got, "<key>ThrottleInterval</key>") {
		t.Errorf("plist has no throttle:\n%s", got)
	}
}

func TestPlistEscapesPaths(t *testing.T) {
	m := newManager(t, newFakeRunner(), Options{
		BinaryPath: "/Users/a&b/riggs",
		ConfigPath: "/Users/a&b/config.yaml",
	})
	got := string(mustPlist(t, m))

	if strings.Contains(got, "a&b") {
		t.Errorf("ampersand was not escaped:\n%s", got)
	}
	if !strings.Contains(got, "a&amp;b") {
		t.Errorf("escaping did not happen:\n%s", got)
	}
}

func mustPlist(t *testing.T, m *Manager) []byte {
	t.Helper()
	plist, err := m.Plist()
	if err != nil {
		t.Fatalf("Plist: %v", err)
	}
	return plist
}

// --- install / uninstall ---------------------------------------------------

func TestInstallWritesThePlistAndLoadsIt(t *testing.T) {
	skipUnlessDarwin(t)
	runner := newFakeRunner()
	m := newManager(t, runner, Options{ConfigPath: "/etc/riggs/config.yaml", Profile: "riggs"})

	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(m.PlistPath()); err != nil {
		t.Fatalf("plist was not written: %v", err)
	}
	// launchd will not create the log directory, and a missing one makes the
	// agent fail to spawn.
	if _, err := os.Stat(m.LogDir()); err != nil {
		t.Fatalf("log directory was not created: %v", err)
	}

	verbs := runner.verbs()
	want := []string{"bootout", "bootstrap", "kickstart"}
	if len(verbs) != len(want) {
		t.Fatalf("launchctl calls = %v, want %v", verbs, want)
	}
	for i := range want {
		if verbs[i] != want[i] {
			t.Fatalf("launchctl calls = %v, want %v", verbs, want)
		}
	}
}

// Installing twice must pick up a changed profile or an upgraded binary rather
// than leaving the old agent running.
func TestInstallIsIdempotent(t *testing.T) {
	skipUnlessDarwin(t)
	runner := newFakeRunner()
	m := newManager(t, runner, Options{Profile: "riggs"})

	for i := 0; i < 2; i++ {
		if err := m.Install(context.Background()); err != nil {
			t.Fatalf("Install %d: %v", i, err)
		}
	}
	if got := len(runner.calls); got != 6 {
		t.Fatalf("made %d launchctl calls over two installs, want 6", got)
	}
}

func TestUninstallRemovesThePlist(t *testing.T) {
	skipUnlessDarwin(t)
	runner := newFakeRunner()
	m := newManager(t, runner, Options{})
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(m.PlistPath()); !os.IsNotExist(err) {
		t.Fatalf("plist still present: %v", err)
	}
}

// A missing plist is success: the intent was for the agent not to be there.
func TestUninstallOfNothingSucceeds(t *testing.T) {
	skipUnlessDarwin(t)
	m := newManager(t, newFakeRunner(), Options{})
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
}

func TestInstallNeedsABinaryPath(t *testing.T) {
	skipUnlessDarwin(t)
	m := New(newFakeRunner(), Options{HomeDir: t.TempDir(), UID: 501})
	if err := m.Install(context.Background()); err == nil {
		t.Fatal("Install succeeded with no binary to run")
	}
}

// --- status ----------------------------------------------------------------

func TestStatusReportsNotInstalled(t *testing.T) {
	skipUnlessDarwin(t)
	m := newManager(t, newFakeRunner(), Options{})

	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Installed {
		t.Fatal("reported installed with no plist on disk")
	}
	if !strings.Contains(status.String(), "not installed") {
		t.Fatalf("status = %q", status.String())
	}
}

func TestStatusSummarisesLaunchctlOutput(t *testing.T) {
	skipUnlessDarwin(t)
	runner := newFakeRunner()
	runner.out["print"] = []byte(`
	domain = gui/501
	state = running
	pid = 4242
	last exit code = 0
	an enormous amount of other domain state = ignored
`)
	m := newManager(t, runner, Options{})
	if err := m.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Installed {
		t.Fatal("reported not installed after Install")
	}
	for _, want := range []string{"state = running", "pid = 4242"} {
		if !strings.Contains(status.Detail, want) {
			t.Errorf("detail %q is missing %q", status.Detail, want)
		}
	}
	if strings.Contains(status.Detail, "domain state") {
		t.Errorf("detail kept the noise: %q", status.Detail)
	}
}

func TestPathsLiveUnderTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	m := New(newFakeRunner(), Options{HomeDir: home, UID: 501, BinaryPath: "/x"})

	if got, want := m.PlistPath(), filepath.Join(home, "Library", "LaunchAgents", Label+".plist"); got != want {
		t.Errorf("PlistPath = %q, want %q", got, want)
	}
	if got, want := m.LogDir(), filepath.Join(home, "Library", "Logs", "riggs"); got != want {
		t.Errorf("LogDir = %q, want %q", got, want)
	}
}

// A launch agent inherits nothing, and launchd's default PATH has no Homebrew
// and no ~/.local/bin. Riggs shells out to `gh` for its GitHub token, so
// without this the daemon connects perfectly and then fails on the first click
// with "executable file not found in $PATH" — which is exactly what happened.
func TestPlistCarriesAPath(t *testing.T) {
	m := newManager(t, newFakeRunner(), Options{Path: "/opt/homebrew/bin:/usr/bin"})
	got := string(mustPlist(t, m))

	if !strings.Contains(got, "<key>EnvironmentVariables</key>") {
		t.Fatalf("plist sets no environment:\n%s", got)
	}
	if !strings.Contains(got, "<string>/opt/homebrew/bin:/usr/bin</string>") {
		t.Errorf("plist does not carry the PATH:\n%s", got)
	}
}

func TestMissingToolsReportsWhatIsUnreachable(t *testing.T) {
	dir := t.TempDir()
	// A `gh` the agent can reach, and no `claude` anywhere.
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := newManager(t, newFakeRunner(), Options{Path: dir})

	missing := m.MissingTools()
	if len(missing) != 1 || missing[0] != "claude" {
		t.Fatalf("MissingTools = %v, want [claude]", missing)
	}
}

// A non-executable file of the right name is not a usable tool.
func TestMissingToolsIgnoresNonExecutables(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("not executable"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := newManager(t, newFakeRunner(), Options{Path: dir})

	found := false
	for _, name := range m.MissingTools() {
		if name == "gh" {
			found = true
		}
	}
	if !found {
		t.Error("a non-executable gh was treated as usable")
	}
}
