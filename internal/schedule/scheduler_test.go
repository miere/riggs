package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeStore is the ledger, remembered rather than written.
type fakeStore struct {
	mu   sync.Mutex
	jobs []Job
	runs []recorded
	err  error
}

type recorded struct {
	name   string
	at     time.Time
	took   time.Duration
	err    error
	output string
}

func (f *fakeStore) Jobs(context.Context) ([]Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]Job{}, f.jobs...), nil
}

func (f *fakeStore) RecordJobRun(_ context.Context, name string, at time.Time,
	took time.Duration, runErr error, output string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, recorded{name, at, took, runErr, output})
	return nil
}

func (f *fakeStore) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded{}, f.runs...)
}

// job builds an enabled job with a stable UpdatedAt.
func job(name, spec string, args ...string) Job {
	if len(args) == 0 {
		args = []string{"git", "pr", "--bulk", "miere"}
	}
	return Job{Name: name, Args: args, Spec: spec, Timeout: time.Minute,
		Enabled: true, UpdatedAt: at("2026-09-01 08:00")}
}

// rig assembles a scheduler over a fake store and a recording exec.
type rig struct {
	*Scheduler
	store *fakeStore
	mu    sync.Mutex
	ran   [][]string
	out   []byte
	err   error
	block chan struct{}
}

func newRig(t *testing.T, jobs ...Job) *rig {
	t.Helper()
	r := &rig{store: &fakeStore{jobs: jobs}}
	r.Scheduler = New(r.store, r.exec, quiet())
	return r
}

func (r *rig) exec(ctx context.Context, args []string) ([]byte, error) {
	r.mu.Lock()
	r.ran = append(r.ran, args)
	block, out, err := r.block, r.out, r.err
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
	return out, err
}

func (r *rig) started() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string{}, r.ran...)
}

// passAt reconciles once and releases the claims, which is what the real loop
// does as each job finishes. Tests that care about the claim itself call due()
// directly instead.
func (r *rig) passAt(now time.Time) []string {
	jobs := r.due(r.store.jobs, now)
	for _, j := range jobs {
		r.finish(j.Name)
	}
	return names(jobs)
}

// names lists the jobs a pass selected.
func names(jobs []Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

// --- what is due -----------------------------------------------------------

// `every 3m` after a restart means the digest should be current now, not in
// three minutes.
func TestAnIntervalJobIsDueImmediately(t *testing.T) {
	r := newRig(t, job("digest", "3m"))
	now := at("2026-09-01 09:00")

	if got := r.passAt(now); len(got) != 1 || got[0] != "digest" {
		t.Fatalf("first pass = %v, want the job to run at once", got)
	}
	// And not again until its interval has elapsed.
	if got := r.passAt(now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("ran again after a minute: %v", got)
	}
	if got := r.passAt(now.Add(3 * time.Minute)); len(got) != 1 {
		t.Fatalf("did not run after its interval: %v", got)
	}
}

// "09:00 on weekdays" said when it wants to run, and a restart at 14:00 is not
// it.
func TestACalendarJobWaitsForItsTime(t *testing.T) {
	r := newRig(t, job("morning", "0 9 * * *"))
	now := at("2026-09-01 14:00")

	if got := r.passAt(now); len(got) != 0 {
		t.Fatalf("a calendar job fired on startup: %v", got)
	}
	if got := r.passAt(at("2026-09-02 08:59")); len(got) != 0 {
		t.Fatalf("fired early: %v", got)
	}
	if got := r.passAt(at("2026-09-02 09:00")); len(got) != 1 {
		t.Fatalf("did not fire at its time: %v", got)
	}
}

// A daemon that was down over nine o'clock comes back and waits for tomorrow,
// instead of firing a morning report in the afternoon.
func TestAMissedRunIsSkippedNotCaughtUp(t *testing.T) {
	r := newRig(t, job("morning", "0 9 * * *"))
	// First pass at 08:00 schedules 09:00.
	r.passAt(at("2026-09-01 08:00"))
	// The daemon is gone until the afternoon. One run, not five.
	if got := r.passAt(at("2026-09-01 14:00")); len(got) != 1 {
		t.Fatalf("pass = %v, want the one missed run", got)
	}
	// And the next one is tomorrow, not another catch-up.
	if got := r.passAt(at("2026-09-01 14:01")); len(got) != 0 {
		t.Fatalf("stampeded: %v", got)
	}
	if got := r.passAt(at("2026-09-02 09:00")); len(got) != 1 {
		t.Fatalf("tomorrow was missed: %v", got)
	}
}

// Two passes of the same digest race each other to write the same ledger rows.
func TestAnOverrunningJobIsSkipped(t *testing.T) {
	r := newRig(t, job("digest", "1m"))
	now := at("2026-09-01 09:00")
	// The first pass claims it, and nothing has released the claim: this is
	// exactly the state a job that is still running leaves behind.
	r.due(r.store.jobs, now)
	if !r.IsRunning("digest") {
		t.Fatal("the first pass did not claim the job")
	}

	if got := r.due(r.store.jobs, now.Add(2*time.Minute)); len(got) != 0 {
		t.Fatalf("started a second copy: %v", names(got))
	}
	// Once it finishes, the next turn is taken normally.
	r.finish("digest")
	if got := names(r.due(r.store.jobs, now.Add(4*time.Minute))); len(got) != 1 {
		t.Fatalf("did not resume: %v", got)
	}
}

// An edit has to be noticed, or a rescheduled job keeps its old cadence until
// the daemon restarts.
func TestEditingAJobRecomputesItsSchedule(t *testing.T) {
	r := newRig(t, job("morning", "0 9 * * *"))
	r.passAt(at("2026-09-01 08:00"))

	r.store.jobs[0].Spec = "3m"
	r.store.jobs[0].UpdatedAt = at("2026-09-01 08:30")

	// An interval job is due at once, so the edit is visible on the next pass.
	if got := r.passAt(at("2026-09-01 08:31")); len(got) != 1 {
		t.Fatalf("the edit was not picked up: %v", got)
	}
}

// "Off for now" and "deleted" are different intentions, and re-enabling starts
// the cycle from the moment it was re-enabled.
func TestADisabledJobDoesNotRun(t *testing.T) {
	r := newRig(t, job("digest", "3m"))
	r.store.jobs[0].Enabled = false

	if got := r.passAt(at("2026-09-01 09:00")); len(got) != 0 {
		t.Fatalf("a disabled job ran: %v", got)
	}
	r.store.jobs[0].Enabled = true
	if got := r.passAt(at("2026-09-01 09:01")); len(got) != 1 {
		t.Fatalf("re-enabling did not start it: %v", got)
	}
}

func TestADeletedJobIsForgotten(t *testing.T) {
	r := newRig(t, job("digest", "3m"))
	r.passAt(at("2026-09-01 09:00"))
	r.store.jobs = nil

	if got := r.passAt(at("2026-09-01 09:05")); len(got) != 0 {
		t.Fatalf("a deleted job ran: %v", got)
	}
	if _, known := r.NextRun("digest"); known {
		t.Fatal("state was left behind for a deleted job")
	}
}

// A hand-edited row or a downgrade. The alternative to saying so is a job that
// silently never runs.
func TestAnUnreadableSpecIsSkippedNotFatal(t *testing.T) {
	r := newRig(t, job("broken", "every other tuesday"), job("fine", "3m"))
	got := r.passAt(at("2026-09-01 09:00"))
	if len(got) != 1 || got[0] != "fine" {
		t.Fatalf("pass = %v, want the readable job alone", got)
	}
}

// --- running one -----------------------------------------------------------

func TestRunNowExecutesTheArgumentsAndRecordsSuccess(t *testing.T) {
	r := newRig(t)
	r.out = []byte("posted 3 rows")
	now := at("2026-09-01 09:00")

	if _, err := r.RunNow(context.Background(), job("digest", "3m"), now); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	started := r.started()
	if len(started) != 1 || strings.Join(started[0], " ") != "git pr --bulk miere" {
		t.Fatalf("ran %v", started)
	}
	rec := r.store.recorded()
	if len(rec) != 1 || rec[0].name != "digest" || rec[0].err != nil {
		t.Fatalf("recorded %+v", rec)
	}
	// A successful job's chatter is never read, and a digest pass prints a
	// paragraph every three minutes.
	if rec[0].output != "" {
		t.Fatalf("a successful run kept its output: %q", rec[0].output)
	}
}

func TestRunNowRecordsAFailureWithItsOutput(t *testing.T) {
	r := newRig(t)
	r.out, r.err = []byte("fatal: not a git repository"), errors.New("exit status 128")

	_, err := r.RunNow(context.Background(), job("digest", "3m"), at("2026-09-01 09:00"))
	if err == nil {
		t.Fatal("a failing job reported success")
	}
	rec := r.store.recorded()
	if len(rec) != 1 || rec[0].err == nil {
		t.Fatalf("recorded %+v", rec)
	}
	if !strings.Contains(rec[0].output, "not a git repository") {
		t.Fatalf("the output was dropped: %q", rec[0].output)
	}
}

// One job went wrong; the other may well still have been going. They need
// different words.
func TestATimedOutJobSaysSo(t *testing.T) {
	r := newRig(t)
	r.block = make(chan struct{})
	defer close(r.block)

	j := job("slow", "3m")
	j.Timeout = 10 * time.Millisecond
	result, err := r.RunNow(context.Background(), j, at("2026-09-01 09:00"))
	if err == nil {
		t.Fatal("a job past its bound reported success")
	}
	if !result.TimedOut {
		t.Fatalf("TimedOut = false: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

// This is the first thing in the daemon that runs unattended, and there is
// nobody to notice.
func TestAPanickingJobDoesNotTakeTheDaemonDown(t *testing.T) {
	r := newRig(t)
	r.Scheduler.exec = func(context.Context, []string) ([]byte, error) { panic("boom") }

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.start(context.Background(), job("bad", "3m"), at("2026-09-01 09:00"))
	}()
	<-done
	// The claim is released even through a panic, so the job is not wedged
	// "running" for the life of the process.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !r.IsRunning("bad") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the job stayed claimed after panicking")
}

// The ledger is a local file; if it cannot be read the next tick is fifteen
// seconds away and there is nothing useful to do in between.
func TestAnUnreadableLedgerDoesNotStopTheLoop(t *testing.T) {
	r := newRig(t, job("digest", "3m"))
	r.store.err = errors.New("database is locked")
	r.pass(context.Background()) // must not panic
	if got := r.started(); len(got) != 0 {
		t.Fatalf("ran something on a failed read: %v", got)
	}
}

// Cancellation is how this process is asked to stop, not a failure to report.
func TestRunReturnsCleanlyOnCancellation(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want nil on cancellation", err)
	}
}
