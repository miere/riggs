package notify

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func jobStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sampleJob() Job {
	return Job{
		Name:    "github-review-queue",
		Type:    "github-reviews",
		Params:  map[string]string{"login": "miere"},
		Spec:    "3m",
		Enabled: true,
	}
}

func TestJobRoundTrip(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	want := sampleJob()
	if err := s.SaveJob(ctx, want); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	got, found, err := s.Job(ctx, want.Name)
	if err != nil || !found {
		t.Fatalf("Job: %v found=%v", err, found)
	}
	if got.Name != want.Name || got.Type != want.Type || got.Spec != want.Spec || !got.Enabled {
		t.Fatalf("job = %+v", got)
	}
	// The parameters are the whole point of the row: a ticket digest that came
	// back without its query is not a smaller version of the job, it is a
	// different one.
	if got.Params["login"] != "miere" {
		t.Fatalf("params = %v", got.Params)
	}
	if got.Ran() {
		t.Fatal("a job that has never run reports a last run")
	}
	if _, found, _ := s.Job(ctx, "nothing"); found {
		t.Fatal("found a job that was never saved")
	}
}

// Editing a schedule is not a statement about whether the last run worked, and
// blanking that would take the one piece of evidence anybody has when a job
// starts failing after a change.
func TestSavingAJobKeepsItsHistory(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	job := sampleJob()
	if err := s.SaveJob(ctx, job); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	ran := time.Now().Truncate(time.Second)
	if err := s.RecordJobRun(ctx, job.Name, ran, 1500*time.Millisecond,
		errors.New("exit status 1"), "fatal: boom"); err != nil {
		t.Fatalf("RecordJobRun: %v", err)
	}

	job.Spec = "5m"
	if err := s.SaveJob(ctx, job); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	got, _, err := s.Job(ctx, job.Name)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Spec != "5m" {
		t.Fatalf("the edit did not take: %q", got.Spec)
	}
	if !got.Ran() || got.LastOK {
		t.Fatalf("the history was lost: %+v", got)
	}
	if got.LastError != "exit status 1" || got.LastOutput != "fatal: boom" {
		t.Fatalf("last run = %q / %q", got.LastError, got.LastOutput)
	}
	if got.LastDuration != 1500*time.Millisecond {
		t.Fatalf("duration = %v", got.LastDuration)
	}
	if !got.LastRun.Equal(ran) {
		t.Fatalf("last run at %v, want %v", got.LastRun, ran)
	}
}

// A successful run overwrites a failed one, or the Home tab keeps showing an
// error that has been fixed.
func TestASuccessfulRunClearsTheFailure(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	job := sampleJob()
	if err := s.SaveJob(ctx, job); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	now := time.Now()
	if err := s.RecordJobRun(ctx, job.Name, now, time.Second, errors.New("boom"), "why"); err != nil {
		t.Fatalf("RecordJobRun: %v", err)
	}
	if err := s.RecordJobRun(ctx, job.Name, now.Add(time.Minute), time.Second, nil, ""); err != nil {
		t.Fatalf("RecordJobRun: %v", err)
	}
	got, _, _ := s.Job(ctx, job.Name)
	if !got.LastOK || got.LastError != "" || got.LastOutput != "" {
		t.Fatalf("the fixed job still reports a failure: %+v", got)
	}
}

// "Off for now" and "deleted" are different intentions, and only one of them is
// recoverable.
func TestDisablingKeepsTheDefinition(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	if err := s.SaveJob(ctx, sampleJob()); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	found, err := s.SetJobEnabled(ctx, "github-review-queue", false, time.Now())
	if err != nil || !found {
		t.Fatalf("SetJobEnabled: %v found=%v", err, found)
	}
	got, _, _ := s.Job(ctx, "github-review-queue")
	if got.Enabled {
		t.Fatal("the job is still enabled")
	}
	if got.Type == "" || got.Params["login"] != "miere" {
		t.Fatalf("disabling lost the definition: %+v", got)
	}

	// A name that is not there is told apart from a write that did nothing:
	// that is a Home tab published before somebody deleted the job.
	if found, _ := s.SetJobEnabled(ctx, "gone", false, time.Now()); found {
		t.Fatal("SetJobEnabled reported success for a job that does not exist")
	}
}

func TestDeleteJob(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	if err := s.SaveJob(ctx, sampleJob()); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	found, err := s.DeleteJob(ctx, "github-review-queue")
	if err != nil || !found {
		t.Fatalf("DeleteJob: %v found=%v", err, found)
	}
	if _, found, _ := s.Job(ctx, "github-review-queue"); found {
		t.Fatal("the job survived deletion")
	}
	if found, _ := s.DeleteJob(ctx, "github-review-queue"); found {
		t.Fatal("deleting twice reported success twice")
	}
}

// A list whose order moves under the reader is one they cannot scan.
func TestJobsAreListedByName(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	for _, name := range []string{"zebra", "alpha", "middle"} {
		job := sampleJob()
		job.Name = name
		if err := s.SaveJob(ctx, job); err != nil {
			t.Fatalf("SaveJob: %v", err)
		}
	}
	got, err := s.Jobs(ctx)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "middle" || got[2].Name != "zebra" {
		t.Fatalf("jobs = %v", got)
	}
}

// The ledger predates this table. An existing one has to grow it rather than
// refuse to open.
func TestAnExistingLedgerGainsTheJobsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer second.Close()
	if err := second.SaveJob(context.Background(), sampleJob()); err != nil {
		t.Fatalf("SaveJob on a reopened ledger: %v", err)
	}
}

// A row written by a build before jobs were typed still loads, and brings its
// argument list with it. That is the migration's whole input: if this row
// refused to scan, the daemon would fail to read the schedule at all and every
// job on the machine would stop.
func TestALegacyRowLoadsWithItsArguments(t *testing.T) {
	ctx, s := context.Background(), jobStore(t)
	// Written the way the old SaveJob wrote it: no type, no params, an argv in
	// `args` and a timeout in its own column.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (name, args, spec, timeout_ms, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		"quick-coding-tasks-poll", `["jira","tickets","--bulk","project = NYX"]`, "3m",
		120000, "2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z"); err != nil {
		t.Fatalf("inserting a legacy row: %v", err)
	}

	got, found, err := s.Job(ctx, "quick-coding-tasks-poll")
	if err != nil || !found {
		t.Fatalf("Job: %v found=%v", err, found)
	}
	if got.Type != "" {
		t.Fatalf("type = %q, want a row that has not been migrated yet", got.Type)
	}
	if len(got.LegacyArgs) != 4 || got.LegacyArgs[3] != "project = NYX" {
		t.Fatalf("legacy args = %q", got.LegacyArgs)
	}

	// And once it is saved back as a typed job, the legacy column is cleared —
	// so the next start's migration pass leaves it alone.
	got.Type = "jira-tickets"
	got.Params = map[string]string{"jql": "project = NYX"}
	if err := s.SaveJob(ctx, got); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	again, _, err := s.Job(ctx, got.Name)
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if len(again.LegacyArgs) != 0 {
		t.Fatalf("legacy args survived the save: %q", again.LegacyArgs)
	}
	if again.Params["jql"] != "project = NYX" {
		t.Fatalf("params = %v", again.Params)
	}
}
