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
		Args:    []string{"git", "pr", "--bulk", "miere"},
		Spec:    "3m",
		Timeout: 2 * time.Minute,
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
	if got.Name != want.Name || got.Spec != want.Spec || got.Timeout != want.Timeout || !got.Enabled {
		t.Fatalf("job = %+v", got)
	}
	// The argument list is the whole point of the row: a job that came back
	// with no arguments would invoke the binary with none and print the usage
	// line every three minutes.
	if len(got.Args) != 4 || got.Args[3] != "miere" {
		t.Fatalf("args = %v", got.Args)
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
	if len(got.Args) != 4 {
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
