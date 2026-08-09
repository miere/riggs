package installer

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/miere/riggs-mcp/internal/config"
)

// defaultTicketJQL is the query the quick-coding-tasks poll has always used,
// lifted from the Python so the job keeps advertising exactly what it did.
const defaultTicketJQL = `project = NYX AND labels = "ai-able" AND assignee IS EMPTY AND status = "Ready"`

// job is one Murtaugh scheduled job Riggs can install.
type job struct {
	// Name is the Murtaugh job key. It matches the existing job so installing
	// replaces the Python definition rather than running alongside it.
	Name string
	// Tool is the Riggs tool the job invokes. The job is skipped when this is
	// not registered, so a not-yet-built phase never installs a job that would
	// fail every minute.
	Tool string
	// Args are the Riggs arguments, before delivery flags are appended.
	Args []string
	// Every and Schedule carry the cadence the Python job already runs at.
	// Exactly one is set.
	Every    string
	Schedule string
	Timeout  string
	// What describes the job in the prompt.
	What string
}

// jobs is the set Riggs takes over. Cadences are the ones already configured
// in Murtaugh; the installer does not ask, because changing a schedule during
// a migration hides which change caused a behaviour difference.
var jobs = []job{
	{
		Name:  "github-review-queue",
		Tool:  "git.pr.fetch-reviews",
		Args:  []string{"git", "pr", "--fetch-reviews"},
		Every: "1m", Timeout: "2m",
		What: "PR review queue",
	},
	{
		Name:  "quick-coding-tasks-poll",
		Tool:  "jira.tickets.poll",
		Args:  []string{"jira", "tickets", "--poll", defaultTicketJQL},
		Every: "3m", Timeout: "2m",
		What: "ai-able ticket poll",
	},
	{
		Name:     "quick-coding-tasks-nudge",
		Tool:     "jira.tickets.nudge",
		Args:     []string{"jira", "tickets", "--nudge"},
		Schedule: "0 9,12,14,17 * * 1-5", Timeout: "3m",
		What: "idle ticket nudge",
	},
}

// wireMurtaugh registers the scheduled jobs, if Murtaugh is installed.
func (i *Installer) wireMurtaugh(ctx context.Context, cfg *config.Config, configPath string) error {
	i.p.Say("")
	i.p.Say("Murtaugh runs the schedule and owns the Slack gateway; Riggs is only ever")
	i.p.Say("invoked by it. Leave this empty to skip job registration.")

	murtaughCfg, err := i.p.Ask("Murtaugh config path", defaultMurtaughConfig())
	if err != nil {
		return err
	}
	murtaughCfg = strings.TrimSpace(expandHome(murtaughCfg))
	if murtaughCfg == "" {
		i.p.Say("Skipped. Register the jobs later with `murtaugh cfg job set`.")
		return nil
	}
	if _, err := i.stat(murtaughCfg); err != nil {
		return fmt.Errorf("no Murtaugh config at %s: %w", murtaughCfg, err)
	}
	bin, err := i.lookPath("murtaugh")
	if err != nil {
		return fmt.Errorf("murtaugh config found at %s but the murtaugh binary is not on PATH", murtaughCfg)
	}

	available := map[string]bool{}
	if i.opts.ToolsFor != nil {
		available, err = i.opts.ToolsFor(configPath)
		if err != nil {
			return fmt.Errorf("determining which tools this build exposes: %w", err)
		}
	}

	var installed, skipped []string
	for _, j := range jobs {
		if !available[j.Tool] {
			skipped = append(skipped, fmt.Sprintf("%s (needs the %s tool, not built yet)", j.Name, j.Tool))
			continue
		}
		channel, err := i.p.Ask(
			fmt.Sprintf("  Channel for the %s (empty = DM the admin)", j.What), "")
		if err != nil {
			return err
		}
		args := i.jobArgs(j, strings.TrimSpace(channel), configPath)
		if err := i.setJob(ctx, bin, j, args); err != nil {
			return err
		}
		installed = append(installed, j.Name)
	}

	i.p.Say("")
	for _, name := range installed {
		i.p.Say("  registered %s", name)
	}
	for _, note := range skipped {
		i.p.Say("  skipped    %s", note)
	}
	if len(installed) > 0 {
		i.p.Say("")
		i.p.Say("Restart the Murtaugh gateway to pick these up.")
	}
	return nil
}

// jobArgs builds the Riggs argument list for a job, appending the delivery
// target and — when the config is not in its default location — the flag that
// points Riggs at it.
func (i *Installer) jobArgs(j job, channel, configPath string) []string {
	args := append([]string{}, j.Args...)
	if channel != "" {
		args = append(args, "--slack-channel", channel)
	}
	if configPath != config.DefaultPath() {
		args = append(args, "--config-file", configPath)
	}
	return args
}

// setJob registers one job through Murtaugh's CLI.
//
// This is deliberately `murtaugh cfg job set` rather than a write to
// Murtaugh's database: that command re-validates the whole assembled config
// and rolls back anything that would leave it invalid.
func (i *Installer) setJob(ctx context.Context, bin string, j job, riggsArgs []string) error {
	argv := []string{"cfg", "job", "set", "--name", j.Name, "--command", i.opts.RiggsPath}
	for _, a := range riggsArgs {
		argv = append(argv, "--args", a)
	}
	argv = append(argv, "--timeout", j.Timeout)
	if j.Every != "" {
		argv = append(argv, "--every", j.Every)
	} else {
		argv = append(argv, "--schedule", j.Schedule)
	}

	out, err := i.runCmd(ctx, bin, argv...)
	if err != nil {
		return fmt.Errorf("registering job %s: %w: %s", j.Name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultMurtaughConfig is where Murtaugh keeps its bootstrap file.
func defaultMurtaughConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.config/murtaugh/config.yaml"
}
