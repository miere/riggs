package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/miere/riggs-mcp/internal/app"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/schedule"
)

// jobsUsage is printed for a missing or unknown subcommand.
const jobsUsage = `usage: riggs jobs <command>
  list                                  what is scheduled, and how it went
  add github <name> <schedule> <login>  the pull-request digest for one GitHub user
  add jira <name> <schedule> <jql>      the ticket digest for one query, e.g.
                                        add jira ready 3m \
                                          'project = NYX AND status = "Ready"'
  rm <name>                             forget a job and its history
  enable|disable <name>                 pause or resume without forgetting it
  run <name>                            run one now, whatever its schedule says
  migrate                               adopt jobs written by an older Riggs`

// runJobs is the command-line half of the schedule.
//
// The App Home tab is where these are normally operated, and this exists for
// the two things a Slack modal is bad at: the first-run migration, and looking
// at what is scheduled from a terminal you are already in. It writes the same
// ledger the daemon reads, so an edit here is picked up on the daemon's next
// tick — fifteen seconds, no restart.
func runJobs(ctx context.Context, args []string, configPath string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	store, err := notify.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("opening the ledger: %w", err)
	}
	defer store.Close()

	action, rest := args[0], args[1:]
	switch action {
	case "list":
		return listJobs(ctx, store)
	case "add":
		return addJob(ctx, store, rest)
	case "rm", "remove", "delete":
		return oneNamed(ctx, rest, "rm", func(name string) error {
			found, err := store.DeleteJob(ctx, name)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("there is no job called %q", name)
			}
			fmt.Printf("Removed %s.\n", name)
			return nil
		})
	case "enable":
		return setEnabled(ctx, store, rest, true)
	case "disable":
		return setEnabled(ctx, store, rest, false)
	case "run":
		return oneNamed(ctx, rest, "run", func(name string) error {
			return runJobNow(ctx, cfg, store, name)
		})
	case "migrate":
		return migrateJobs(ctx, store)
	default:
		return fmt.Errorf("unknown jobs command %q\n%s", action, jobsUsage)
	}
}

// listJobs prints the schedule.
func listJobs(ctx context.Context, store *notify.Store) error {
	jobs, err := store.Jobs(ctx)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("Nothing is scheduled. Add one with `riggs jobs add`, or from the App Home tab.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKIND\tSCHEDULE\tSTATE\tLAST RUN\tCOMMAND")
	for _, job := range jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			job.Name, jobKind(job), job.Spec, jobState(job), lastRun(job), schedule.Command(job))
	}
	return w.Flush()
}

// jobKind is the second column: what sort of job this is.
//
// An unknown kind is printed as the raw stored token rather than as a blank or
// a dash. This is the terminal, and the person reading it is the person who
// will have to fix the row — the token is the only thing that tells them
// whether they are looking at a newer Riggs' job or a typo in the ledger.
func jobKind(job notify.Job) string {
	spec, ok := schedule.LookupKind(schedule.KindOf(job))
	if ok {
		return string(spec.Kind)
	}
	if strings.TrimSpace(job.Type) == "" {
		return "UNMIGRATED"
	}
	return job.Type + " (unknown)"
}

// jobState is the middle column: paused, or not.
func jobState(job notify.Job) string {
	if !job.Enabled {
		return "disabled"
	}
	return "enabled"
}

// lastRun renders the outcome column.
//
// The reason is included on a failure and nowhere else. A list is scanned, and
// the one row anybody stops on is the one that says it did not work.
func lastRun(job notify.Job) string {
	if !job.Ran() {
		return "never"
	}
	ago := time.Since(job.LastRun).Round(time.Second)
	if job.LastOK {
		return fmt.Sprintf("ok %s ago (%s)", ago, job.LastDuration.Round(time.Millisecond))
	}
	reason := job.LastError
	if reason == "" {
		reason = "failed"
	}
	return fmt.Sprintf("FAILED %s ago: %s", ago, reason)
}

// addJob defines one, of a named kind.
//
// The kind is the FIRST word, and it is not optional. This used to take a
// command line — `add tickets 3m jira tickets --bulk '<jql>'` — which made the
// operator responsible for the spelling of a command the binary already knows,
// and made a typo in it a job that fails every three minutes rather than a
// usage error at the prompt. A job has a type now (§9d), and there is no way to
// write one down without saying which.
//
// Positional rather than flagged, and deliberately: the value that ends a jira
// line is a JQL query, which is full of things a flag parser would take an
// interest in. The shell has already worked out where it starts and ends —
// that is the one thing in this path that knows — so it is taken as one
// argument, verbatim, and never re-split.
func addJob(ctx context.Context, store *notify.Store, args []string) error {
	if len(args) < 4 {
		return fmt.Errorf("usage: riggs jobs add github|jira <name> <schedule> <login|jql>")
	}
	kind, name, spec, value := args[0], args[1], args[2], args[3]
	if len(args) > 4 {
		// Almost always an unquoted JQL: the shell split it and only the first
		// word arrived. Refused rather than joined back up, because joining is
		// a guess at the operator's spacing and the failure is silent.
		return fmt.Errorf("unexpected argument %q — quote the whole value, e.g. 'project = NYX AND status = \"Ready\"'",
			args[4])
	}

	var (
		job schedule.Job
		err error
	)
	switch kind {
	case "github":
		job, err = schedule.NewGitHubJob(name, value, spec)
	case "jira":
		job, err = schedule.NewJiraJob(name, value, spec)
	default:
		return fmt.Errorf("unknown job kind %q: it is github or jira\n%s", kind, jobsUsage)
	}
	if err != nil {
		return err
	}

	if _, exists, err := store.Job(ctx, job.Name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("there is already a job called %q; remove it first, or edit it from the App Home tab", job.Name)
	}
	job.UpdatedAt = time.Now()
	if err := store.SaveJob(ctx, job); err != nil {
		return err
	}
	fmt.Printf("Added %s: %s, %s\n", job.Name, schedule.Command(job), job.Spec)
	fmt.Println("The daemon picks it up on its next tick.")
	return nil
}

// migrateJobs adopts the jobs an older Riggs wrote, from a terminal.
//
// The daemon does this on every start, which is where it normally happens. This
// exists for the case that matters most on an upgrade: seeing what WOULD be
// discarded, and what it ran, without restarting the daemon to find out — and
// on a machine where the daemon is not running at all.
func migrateJobs(ctx context.Context, store *notify.Store) error {
	report, err := schedule.Migrate(ctx, store, time.Now())
	if err != nil {
		return err
	}
	if !report.Changed() {
		fmt.Println("Every job is already typed; nothing to migrate.")
		return nil
	}
	for _, name := range report.Adopted {
		fmt.Printf("Upgraded %s.\n", name)
	}
	for _, gone := range report.Discarded {
		// To stderr, and with the whole definition: this is the only copy of a
		// job that has just been deleted, and a terminal is where somebody can
		// still copy it back out.
		fmt.Fprintf(os.Stderr, "REMOVED %s — %s\n  it ran: %s\n  on: %s\n",
			gone.Name, gone.Reason, gone.Command, gone.Spec)
	}
	return nil
}

// setEnabled pauses or resumes a job.
func setEnabled(ctx context.Context, store *notify.Store, args []string, enabled bool) error {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	return oneNamed(ctx, args, verb, func(name string) error {
		found, err := store.SetJobEnabled(ctx, name, enabled, time.Now())
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("there is no job called %q", name)
		}
		fmt.Printf("%sd %s.\n", strings.ToUpper(verb[:1])+verb[1:], name)
		return nil
	})
}

// runJobNow runs one job in this process, right now.
//
// The same code the daemon's tick runs, which is the point: a `run` that took a
// different path would be a way to prove the wrong thing works. The outcome is
// recorded, so the Home tab shows a manual run exactly as it shows a scheduled
// one.
func runJobNow(ctx context.Context, cfg *config.Config, store *notify.Store, name string) error {
	job, found, err := store.Job(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("there is no job called %q", name)
	}
	exec, err := schedule.SelfExec(jobConfigFlag(cfg))
	if err != nil {
		return err
	}
	// Output at Info on stderr: this is somebody standing at a terminal waiting
	// for it, not a daemon writing a log nobody reads.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// The same per-kind bounds the daemon applies. A manual run given a
	// different timeout from a scheduled one would be a way to prove the wrong
	// thing works — which is the same reason this goes through RunNow at all.
	result, runErr := schedule.New(store, exec, logger).
		WithTimeouts(app.JobTimeouts(cfg)).
		RunNow(ctx, job, time.Now())
	if out := strings.TrimSpace(result.Output); out != "" {
		fmt.Println(out)
	}
	return runErr
}

// jobConfigFlag mirrors the daemon's rule: pass --config-file only when the
// config is not where Riggs would look anyway.
func jobConfigFlag(cfg *config.Config) string {
	if cfg == nil || cfg.Path == config.NoFilePath || cfg.Path == config.DefaultPath() {
		return ""
	}
	return cfg.Path
}

// oneNamed runs fn against exactly one job name.
func oneNamed(_ context.Context, args []string, verb string, fn func(string) error) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: riggs jobs %s <name>", verb)
	}
	return fn(args[0])
}
