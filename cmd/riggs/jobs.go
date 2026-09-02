package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/schedule"
)

// jobsUsage is printed for a missing or unknown subcommand.
const jobsUsage = `usage: riggs jobs <command>
  list                                  what is scheduled, and how it went
  add <name> <schedule> <command...>    e.g. add nightly "0 9 * * 1-5" jira tickets --bulk
  rm <name>                             forget a job and its history
  enable|disable <name>                 pause or resume without forgetting it
  run <name>                            run one now, whatever its schedule says`

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
	fmt.Fprintln(w, "NAME\tSCHEDULE\tSTATE\tLAST RUN\tCOMMAND")
	for _, job := range jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			job.Name, job.Spec, jobState(job), lastRun(job), schedule.Command(job))
	}
	return w.Flush()
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

// addJob defines one.
//
// Positional rather than flagged, and deliberately: the command being scheduled
// is itself full of flags, and `riggs jobs add x 3m git pr --bulk miere` would
// have any flag parser worth the name trying to interpret `--bulk`.
func addJob(ctx context.Context, store *notify.Store, args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("usage: riggs jobs add <name> <schedule> <command...>")
	}
	name, spec, command := args[0], args[1], args[2:]
	job, err := schedule.NewJob(name, schedule.SplitArgs(strings.Join(command, " ")),
		spec, schedule.DefaultTimeout, true)
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
	result, runErr := schedule.New(store, exec, logger).RunNow(ctx, job, time.Now())
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
