package installer

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
)

// argvOf returns the recorded argv for the named job, and whether it ran.
func argvOf(r *rig, jobName string) ([]string, bool) {
	for _, c := range r.cmds {
		for i, a := range c.args {
			if a == "--name" && i+1 < len(c.args) && c.args[i+1] == jobName {
				return c.args, true
			}
		}
	}
	return nil, false
}

// joined renders an argv for substring assertions, with a separator that
// cannot appear inside an argument.
func joined(args []string) string { return strings.Join(args, "\x00") }

func TestRegistersJobThroughMurtaughCLI(t *testing.T) {
	s := happyScript("C0B29C20Z9S")
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.cmds) != 1 {
		t.Fatalf("commands = %+v, want one job registration", r.cmds)
	}
	cmd := r.cmds[0]
	if cmd.name != "/usr/local/bin/murtaugh" {
		t.Errorf("ran %q, want the murtaugh binary", cmd.name)
	}
	// Murtaugh's own CLI, never its database. `jobs define` specifically:
	// `cfg job set` is documented as equivalent but rejects --args, which
	// would leave the job invoking Riggs with no verb.
	if got := strings.Join(cmd.args[:2], " "); got != "jobs define" {
		t.Errorf("argv starts %q, want `jobs define`", got)
	}

	argv := joined(cmd.args)
	for _, want := range []string{
		"--name\x00github-review-queue",
		"--command\x00/usr/local/bin/riggs",
		"--args\x00git\x00--args\x00pr\x00--args\x00--bulk",
		"--args\x00--slack-channel\x00--args\x00C0B29C20Z9S",
		"--every\x003m",
		"--timeout\x002m",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv missing %q:\n%v", strings.ReplaceAll(want, "\x00", " "), cmd.args)
		}
	}
}

// An empty channel means DM the admin, so no channel flag is passed at all.
func TestEmptyChannelOmitsTheFlag(t *testing.T) {
	s := happyScript() // no channel answer -> empty
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv, ok := argvOf(r, "github-review-queue")
	if !ok {
		t.Fatal("job not registered")
	}
	if strings.Contains(joined(argv), "--slack-channel") {
		t.Errorf("a channel flag was passed for the DM default:\n%v", argv)
	}
}

// The job must point Riggs at the config the installer just wrote, but only
// when that is not where Riggs would look anyway.
func TestConfigFileFlagOnlyWhenNonDefault(t *testing.T) {
	s := happyScript()
	s.answers[0] = config.DefaultPath()
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv, _ := argvOf(r, "github-review-queue")
	if strings.Contains(joined(argv), "--config-file") {
		t.Errorf("--config-file passed for the default location:\n%v", argv)
	}

	s2 := happyScript()
	s2.answers[0] = "/opt/riggs/config.yaml"
	r2 := newRig(t, s2, map[string]bool{"git.pr.bulk": true})
	if err := r2.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv2, _ := argvOf(r2, "github-review-queue")
	if !strings.Contains(joined(argv2), "--config-file\x00--args\x00/opt/riggs/config.yaml") &&
		!strings.Contains(joined(argv2), "--args\x00--config-file\x00--args\x00/opt/riggs/config.yaml") {
		t.Errorf("--config-file not passed for a custom location:\n%v", argv2)
	}
}

// A job whose tool this build does not have is skipped, loudly — installing it
// would mean a scheduled failure every minute until the phase lands.
func TestSkipsJobsWhoseToolIsNotBuilt(t *testing.T) {
	s := happyScript()
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := argvOf(r, "quick-coding-tasks-poll"); ok {
		t.Error("registered a job whose tool does not exist")
	}
	transcript := r.prompt.transcript()
	for _, want := range []string{"quick-coding-tasks-poll", "jira.tickets.poll", "not built yet"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("the skip was not explained (%q missing):\n%s", want, transcript)
		}
	}
}

// The nudge keeps its cron, not an interval — the cadences are carried over
// unchanged so a migration does not also change behaviour.
func TestPreservesExistingCadences(t *testing.T) {
	s := happyScript("", "", "")
	r := newRig(t, s, map[string]bool{
		"git.pr.bulk": true, "jira.tickets.poll": true, "jira.tickets.nudge": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, tc := range []struct{ job, flag, value string }{
		{"github-review-queue", "--every", "3m"},
		{"quick-coding-tasks-poll", "--every", "3m"},
		{"quick-coding-tasks-nudge", "--schedule", "0 9,12,14,17 * * 1-5"},
	} {
		argv, ok := argvOf(r, tc.job)
		if !ok {
			t.Errorf("%s was not registered", tc.job)
			continue
		}
		if !strings.Contains(joined(argv), tc.flag+"\x00"+tc.value) {
			t.Errorf("%s: want %s %q:\n%v", tc.job, tc.flag, tc.value, argv)
		}
		// --every and --schedule are mutually exclusive in Murtaugh.
		if strings.Contains(joined(argv), "--every") && strings.Contains(joined(argv), "--schedule") {
			t.Errorf("%s passed both --every and --schedule:\n%v", tc.job, argv)
		}
	}
}

// An empty Murtaugh path means "not installed": nothing is registered, and the
// install still succeeds.
func TestEmptyMurtaughPathSkipsRegistration(t *testing.T) {
	s := happyScript()
	s.answers[4] = "<empty>"
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.cmds) != 0 {
		t.Errorf("ran %+v, want nothing", r.cmds)
	}
	if !strings.Contains(r.prompt.transcript(), "murtaugh cfg job set") {
		t.Error("the skip did not say how to register the jobs later")
	}
}

// A config path that does not exist is a typo, not an install-free machine.
func TestMissingMurtaughConfigIsAnError(t *testing.T) {
	s := happyScript()
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})
	r.Installer.stat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no Murtaugh config") {
		t.Fatalf("err = %v, want a complaint about the missing Murtaugh config", err)
	}
}

func TestMissingMurtaughBinaryIsAnError(t *testing.T) {
	s := happyScript()
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})
	r.Installer.lookPath = func(string) (string, error) { return "", os.ErrNotExist }

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err = %v, want a complaint about the missing murtaugh binary", err)
	}
}

func TestRedact(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "(empty)"},
		{"short", "*****"},
		{"${SLACK_BOT_TOKEN}", "${SLACK_BOT_TOKEN}"},
		{"xoxb-abcdefghijkl", "xoxb*********ijkl"},
	} {
		if got := redact(tc.in); got != tc.want {
			t.Errorf("redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Decommissioning the per-PR card job is done by REUSING its name: redefining
// it replaces the old definition rather than adding a second notifier beside
// it. Two jobs mirroring one review queue is noise, and nothing else would have
// removed the old one.
func TestDigestReplacesTheCardJobRatherThanJoiningIt(t *testing.T) {
	s := happyScript("C0B29C20Z9S")
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true, "git.pr.fetch-reviews": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Exactly one review job, whatever else this build exposes.
	registrations := 0
	for _, cmd := range r.cmds {
		if strings.Contains(joined(cmd.args), "--name\x00github-review-queue") {
			registrations++
		}
	}
	if registrations != 1 {
		t.Fatalf("registered the review job %d times, want once", registrations)
	}

	argv, ok := argvOf(r, "github-review-queue")
	if !ok {
		t.Fatal("review job not registered")
	}
	// The card renderer is retained, but it is no longer on a schedule.
	if strings.Contains(joined(argv), "--fetch-reviews") {
		t.Errorf("the card job is still scheduled:\n%v", argv)
	}
	if !strings.Contains(joined(argv), "--args\x00--bulk") {
		t.Errorf("the review job does not run the digest:\n%v", argv)
	}
}

// A click is delivered to the app that posted the message, so a digest posted
// through the wrong profile produces buttons the daemon never hears about.
func TestDigestJobCarriesTheSlackProfile(t *testing.T) {
	s := happyScript("C0B29C20Z9S", "riggs")
	r := newRig(t, s, map[string]bool{"git.pr.bulk": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	argv, _ := argvOf(r, "github-review-queue")
	if !strings.Contains(joined(argv), "--args\x00--slack-profile\x00--args\x00riggs") {
		t.Errorf("the digest job does not name a Slack profile:\n%v", argv)
	}

	// And the operator is told why it matters.
	if !strings.Contains(r.prompt.transcript()+strings.Join(r.prompt.asked, "\n"), "riggs daemon") {
		t.Error("the profile prompt does not say it must match the daemon")
	}
}

// Only the digest asks; the ticket jobs are posted by whatever profile is
// already the default, and adding a prompt to each would be noise.
func TestOnlyTheDigestAsksForAProfile(t *testing.T) {
	s := happyScript("", "", "")
	r := newRig(t, s, map[string]bool{
		"git.pr.bulk": true, "jira.tickets.poll": true, "jira.tickets.nudge": true})

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, name := range []string{"quick-coding-tasks-poll", "quick-coding-tasks-nudge"} {
		argv, ok := argvOf(r, name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if strings.Contains(joined(argv), "--slack-profile") {
			t.Errorf("%s was given a profile flag:\n%v", name, argv)
		}
	}
}
