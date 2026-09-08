package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Adopting the jobs an older Riggs wrote.
//
// Before §9d a job was a name and an argument list, and the ledger on a machine
// that has been running Riggs for a while is full of them. They cannot simply be
// left alone: a job with no kind has no timeout to look up, no modal to edit it
// with, and no way to render the arguments it would run — so on the first start
// after an upgrade it would sit on the Home tab doing nothing, with no
// explanation and no control that worked.
//
// So they are converted, on the way up, best effort. The old argv is the only
// description of what the job did, and for the two commands Riggs actually
// schedules it is a complete one: the login or the query is right there in it.
//
// What cannot be converted is DELETED and reported, rather than kept as a row
// that no longer runs. That is the harsher of the two options and it is the
// honest one: a job that survives the upgrade but never fires again is a job
// somebody is still counting on. Deleting it costs a definition that took
// thirty seconds to type; keeping it costs the assumption that the schedule is
// working, which is the one thing this whole surface exists to protect. The
// definition goes into the report, verbatim, so it can be typed back in.

// Report is what a migration pass did.
type Report struct {
	// Adopted names the jobs that were converted, in the order they were read.
	Adopted []string
	// Discarded are the jobs that could not be, with enough of each to type it
	// back in by hand.
	Discarded []Discarded
}

// Changed reports whether the pass touched anything, which is what decides
// whether the admin hears about it at all.
func (r Report) Changed() bool { return len(r.Adopted) > 0 || len(r.Discarded) > 0 }

// Discarded is one job the migration could not keep.
type Discarded struct {
	// Name is what it was called.
	Name string
	// Command is the argument list it would have run, rendered as a line. It is
	// the whole point of the record: without it the admin knows only that
	// something was deleted.
	Command string
	// Spec is the cadence it ran on, so the replacement can keep it.
	Spec string
	// Reason says why it could not be adopted, in words fit to be read in
	// Slack.
	Reason string
}

// MigrationStore is the ledger, narrowed to what a migration pass needs.
type MigrationStore interface {
	Jobs(ctx context.Context) ([]Job, error)
	SaveJob(ctx context.Context, job Job) error
	DeleteJob(ctx context.Context, name string) (bool, error)
}

// Migrate converts every untyped job in the store, and deletes the ones it
// cannot.
//
// It runs at daemon start, before the scheduler's first pass, so a job is
// either typed or gone by the time anything tries to run it.
//
// A job that already has a kind is left completely alone, including one whose
// kind this build does not know — a ledger shared with a newer Riggs is a
// downgrade, not a corruption, and deleting a job because the running binary is
// out of date would be the worst possible reading of "best effort".
//
// One job's failure does not abandon the rest. The pass is a loop over
// independent rows and the whole reason it reports rather than returns on the
// first problem is that the row it cannot handle is exactly the one the admin
// needs told about.
func Migrate(ctx context.Context, store MigrationStore, now time.Time) (Report, error) {
	var report Report
	if store == nil {
		return report, fmt.Errorf("schedule: no ledger to migrate")
	}
	jobs, err := store.Jobs(ctx)
	if err != nil {
		return report, fmt.Errorf("schedule: reading the schedule to migrate it: %w", err)
	}
	for _, job := range jobs {
		if strings.TrimSpace(job.Type) != "" {
			continue
		}
		adopted, err := Adopt(job)
		if err != nil {
			if _, delErr := store.DeleteJob(ctx, job.Name); delErr != nil {
				return report, fmt.Errorf("schedule: discarding job %s: %w", job.Name, delErr)
			}
			report.Discarded = append(report.Discarded, Discarded{
				Name:    job.Name,
				Command: legacyCommand(job),
				Spec:    job.Spec,
				Reason:  err.Error(),
			})
			continue
		}
		// The history is carried over deliberately: this is the same job, with
		// the same name, on the same schedule. Blanking "last ran four minutes
		// ago, fine" because the definition changed shape would leave the Home
		// tab claiming every job on the machine had never run.
		adopted.UpdatedAt = now
		if err := store.SaveJob(ctx, adopted); err != nil {
			return report, fmt.Errorf("schedule: adopting job %s: %w", job.Name, err)
		}
		report.Adopted = append(report.Adopted, adopted.Name)
	}
	return report, nil
}

// Adopt reads one untyped job's argument list back into a kind and its
// parameters.
//
// Only the two commands Riggs schedules are recognised, and only in the exact
// spelling the CLI resolves. Anything else is refused rather than guessed at:
// the guess would be stored, run every three minutes, and be wrong quietly.
func Adopt(job Job) (Job, error) {
	args := job.LegacyArgs
	if len(args) > 0 && strings.EqualFold(args[0], "riggs") {
		// The binary's name, typed by an operator who was copying the line
		// Murtaugh used to run. `jobs add` forgave it, so the ledger has it.
		args = args[1:]
	}
	if len(args) == 0 {
		return Job{}, fmt.Errorf("it has no command stored, so there is nothing to work out what it did from")
	}
	switch {
	case len(args) == 4 && args[0] == "git" && args[1] == "pr" && args[2] == "--bulk":
		return carryOver(job, KindGitHubReviews, map[string]string{ParamLogin: args[3]})

	case len(args) >= 4 && args[0] == "jira" && args[1] == "tickets" && args[2] == "--bulk":
		// Joined rather than taken as args[3], because both shapes are in the
		// wild and this recovers either. A job stored correctly has the whole
		// query in one argument and the join is a no-op; a job stored before
		// the command was taken verbatim has it split on whitespace into
		// twenty-odd, and joining them back with single spaces reassembles it —
		// the quotes were kept as literal characters when it came apart, so
		// they are still there.
		//
		// What it cannot recover is a query whose own spacing was not single
		// spaces. That is a formatting difference in a language that does not
		// care about it, not a change of meaning.
		return carryOver(job, KindJiraTickets, map[string]string{ParamJQL: strings.Join(args[3:], " ")})
	}
	return Job{}, fmt.Errorf("it runs `%s`, which is not one of the two commands Riggs schedules", legacyCommand(job))
}

// carryOver builds the adopted job, keeping everything about the old row that
// is still true.
//
// The schedule is validated on the way through, because an unparseable one has
// been sitting in this ledger silently not running (the scheduler skips it with
// a log line), and adopting it would carry that forward into a job that looks
// healthy on the tab. It is reported as a discard instead, with the spec in the
// message, which is the first time anybody will have been told.
func carryOver(job Job, kind Kind, params map[string]string) (Job, error) {
	if err := ValidateName(job.Name); err != nil {
		return Job{}, fmt.Errorf("its name cannot be used any more: %w", err)
	}
	if _, err := Parse(job.Spec); err != nil {
		return Job{}, fmt.Errorf("its schedule %q cannot be read: %w", job.Spec, err)
	}
	for name, value := range params {
		if strings.TrimSpace(value) == "" {
			return Job{}, fmt.Errorf("its %s is empty", name)
		}
	}
	adopted := job
	adopted.Type = string(kind)
	adopted.Params = params
	adopted.LegacyArgs = nil
	return adopted, nil
}

// legacyCommand renders an untyped job's stored argv for a human to read.
//
// It cannot go through Command, which renders from a job's KIND — and the whole
// situation here is a job that has not got one.
func legacyCommand(job Job) string {
	quoted := make([]string, len(job.LegacyArgs))
	for i, a := range job.LegacyArgs {
		quoted[i] = QuoteArg(a)
	}
	return strings.Join(quoted, " ")
}
