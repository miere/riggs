package schedule

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// migrationLedger is a MigrationStore that keeps its rows in a map.
type migrationLedger struct {
	jobs    map[string]Job
	order   []string
	saveErr error
}

func newMigrationLedger(jobs ...Job) *migrationLedger {
	l := &migrationLedger{jobs: map[string]Job{}}
	for _, job := range jobs {
		l.jobs[job.Name] = job
		l.order = append(l.order, job.Name)
	}
	return l
}

func (l *migrationLedger) Jobs(context.Context) ([]Job, error) {
	out := make([]Job, 0, len(l.order))
	for _, name := range l.order {
		if job, ok := l.jobs[name]; ok {
			out = append(out, job)
		}
	}
	return out, nil
}

func (l *migrationLedger) SaveJob(_ context.Context, job Job) error {
	if l.saveErr != nil {
		return l.saveErr
	}
	l.jobs[job.Name] = job
	return nil
}

func (l *migrationLedger) DeleteJob(_ context.Context, name string) (bool, error) {
	_, ok := l.jobs[name]
	delete(l.jobs, name)
	return ok, nil
}

// The two commands Riggs schedules, adopted from the argv an older build wrote.
func TestMigrateAdoptsTheTwoCommands(t *testing.T) {
	const jql = `project = NYX AND labels = "ai-able" AND status = "Ready"`
	ledger := newMigrationLedger(
		Job{Name: "reviews", LegacyArgs: []string{"git", "pr", "--bulk", "miere"}, Spec: "3m", Enabled: true},
		Job{Name: "tickets", LegacyArgs: []string{"jira", "tickets", "--bulk", jql}, Spec: "5m", Enabled: false},
	)

	report, err := Migrate(context.Background(), ledger, time.Now())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !slices.Equal(report.Adopted, []string{"reviews", "tickets"}) {
		t.Fatalf("adopted = %q", report.Adopted)
	}
	if len(report.Discarded) != 0 {
		t.Fatalf("discarded = %+v", report.Discarded)
	}

	reviews := ledger.jobs["reviews"]
	if KindOf(reviews) != KindGitHubReviews || Param(reviews, ParamLogin) != "miere" {
		t.Fatalf("reviews = %+v", reviews)
	}
	tickets := ledger.jobs["tickets"]
	if KindOf(tickets) != KindJiraTickets || Param(tickets, ParamJQL) != jql {
		t.Fatalf("tickets = %+v", tickets)
	}
	// A disabled job stays disabled. The migration changes the shape of a
	// definition, not the admin's decision about whether it runs.
	if tickets.Enabled {
		t.Fatal("the migration re-enabled a paused job")
	}
	// And the legacy column is cleared, so the next start does not try to adopt
	// an already-adopted row.
	if len(tickets.LegacyArgs) != 0 {
		t.Fatalf("legacy args survived: %q", tickets.LegacyArgs)
	}
}

// The shredded shape is the one actually in the wild: a JQL typed into
// `jobs add` before the command was taken verbatim went in as one argument and
// was stored as twenty-two. Joining them back with single spaces recovers it —
// the quotes came apart as literal characters, so they are still there.
func TestMigrateReassemblesAShreddedQuery(t *testing.T) {
	const jql = `project = NYX AND labels = "ai-able" AND status = "Ready"`
	shredded := append([]string{"jira", "tickets", "--bulk"}, strings.Fields(jql)...)
	ledger := newMigrationLedger(Job{Name: "tickets", LegacyArgs: shredded, Spec: "3m", Enabled: true})

	if _, err := Migrate(context.Background(), ledger, time.Now()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got := Param(ledger.jobs["tickets"], ParamJQL); got != jql {
		t.Fatalf("jql = %q, want %q", got, jql)
	}
}

// What cannot be adopted is deleted and REPORTED, with enough of it to type
// back in. A job that survives an upgrade but never fires again is a job
// somebody is still counting on.
func TestMigrateDiscardsWhatItCannotAdopt(t *testing.T) {
	for name, tc := range map[string]struct {
		job  Job
		want string
	}{
		"an arbitrary command": {
			Job{Name: "backup", LegacyArgs: []string{"rsync", "-a", "/tmp"}, Spec: "0 3 * * *"},
			"not one of the two commands",
		},
		"a command with no arguments": {
			Job{Name: "empty", LegacyArgs: nil, Spec: "3m"},
			"no command stored",
		},
		"a schedule that never parsed": {
			Job{Name: "odd", LegacyArgs: []string{"git", "pr", "--bulk", "miere"}, Spec: "weekly"},
			"cannot be read",
		},
		"a login that is not there": {
			Job{Name: "blank", LegacyArgs: []string{"git", "pr", "--bulk", "  "}, Spec: "3m"},
			"is empty",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := newMigrationLedger(tc.job)
			report, err := Migrate(context.Background(), ledger, time.Now())
			if err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			if len(report.Discarded) != 1 {
				t.Fatalf("discarded = %+v", report.Discarded)
			}
			gone := report.Discarded[0]
			if !strings.Contains(gone.Reason, tc.want) {
				t.Fatalf("reason = %q, want it to mention %q", gone.Reason, tc.want)
			}
			if gone.Spec != tc.job.Spec {
				t.Fatalf("spec = %q, want the cadence carried into the report", gone.Spec)
			}
			if _, still := ledger.jobs[tc.job.Name]; still {
				t.Fatal("the job is still in the ledger")
			}
		})
	}
}

// The whole definition goes into the report, quoted, because it is the only
// copy left of a job that has just been deleted.
func TestMigrateReportsTheCommandItDiscarded(t *testing.T) {
	ledger := newMigrationLedger(Job{
		Name:       "backup",
		LegacyArgs: []string{"rsync", "-a", "/two words/"},
		Spec:       "0 3 * * *",
	})
	report, err := Migrate(context.Background(), ledger, time.Now())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if want := `rsync -a '/two words/'`; report.Discarded[0].Command != want {
		t.Fatalf("command = %q, want %q", report.Discarded[0].Command, want)
	}
}

// A job that already has a kind is left completely alone — including one this
// build does not know, which is a ledger shared with a NEWER Riggs. Deleting
// that because the running binary is out of date would be the worst possible
// reading of "best effort".
func TestMigrateLeavesTypedJobsAlone(t *testing.T) {
	typed, err := NewGitHubJob("reviews", "miere", "3m")
	if err != nil {
		t.Fatalf("NewGitHubJob: %v", err)
	}
	future := Job{Name: "future", Type: "slack-digest", Spec: "3m", Enabled: true}
	ledger := newMigrationLedger(typed, future)

	report, err := Migrate(context.Background(), ledger, time.Now())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if report.Changed() {
		t.Fatalf("a second pass changed something: %+v", report)
	}
	if _, still := ledger.jobs["future"]; !still {
		t.Fatal("a job from a newer Riggs was deleted")
	}
}

// One job's failure does not abandon the rest — except a write that fails,
// which stops the pass: a ledger that will not take a save is not going to take
// the next one either, and reporting six adoptions that did not happen is worse
// than stopping.
func TestMigrateStopsOnAWriteFailure(t *testing.T) {
	ledger := newMigrationLedger(
		Job{Name: "reviews", LegacyArgs: []string{"git", "pr", "--bulk", "miere"}, Spec: "3m"},
	)
	ledger.saveErr = fmt.Errorf("disk is full")

	if _, err := Migrate(context.Background(), ledger, time.Now()); err == nil {
		t.Fatal("Migrate reported success on a ledger that could not be written")
	}
}

// A leading `riggs` was forgiven by the old `jobs add`, so the ledger has rows
// carrying it.
func TestAdoptForgivesTheBinaryName(t *testing.T) {
	job, err := Adopt(Job{
		Name:       "reviews",
		LegacyArgs: []string{"riggs", "git", "pr", "--bulk", "miere"},
		Spec:       "3m",
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if Param(job, ParamLogin) != "miere" {
		t.Fatalf("job = %+v", job)
	}
}
