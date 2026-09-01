package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// The loop. It replaces Murtaugh's cron, and it lives in the daemon because the
// daemon is already the thing that has to keep running (§12b).
//
// What it is NOT is a supervisor. It does not restart a failed job, it does not
// queue a missed one, and it does not run two of the same job at once. A job
// that fails is reported and tried again on its own cadence, which for a
// three-minute digest is the same answer a retry would have given and for a
// nine-o'clock report is the only defensible one.

// Store is the scheduler's half of the ledger.
//
// Narrow on purpose: this reads definitions and writes outcomes, and has no
// business reaching the cards or the digests through a wider handle.
type Store interface {
	Jobs(ctx context.Context) ([]Job, error)
	RecordJobRun(ctx context.Context, name string, at time.Time,
		took time.Duration, runErr error, output string) error
}

// Exec runs one job's argument list and returns its combined output.
//
// The seam that keeps `exec.Command` out of the loop, so the whole scheduler is
// drivable in a test without spawning anything — the rule §11 applies to `gh`
// and to the AI harness.
type Exec func(ctx context.Context, args []string) (output []byte, err error)

// tick is how often the loop looks for work.
//
// Fifteen seconds, against a minimum interval of a minute and a calendar
// resolution of a minute. It bounds how late a job can be, and the cost of it
// is one read of a table with a handful of rows.
const tick = 15 * time.Second

// outputTail bounds what a failed run reports back.
//
// Twelve lines and 1200 characters, the same as a harness run (§7bb) and for
// the same reason: enough to carry the reason, not enough to bury the surface
// showing it. The whole output went to the daemon's log on its way past.
const (
	outputTailLines = 12
	outputTailChars = 1200
)

// Scheduler runs jobs on their cadence.
type Scheduler struct {
	store  Store
	exec   Exec
	logger *slog.Logger
	now    func() time.Time

	// mu guards next and running.
	mu sync.Mutex
	// next is when each job is due, keyed by name. It is held in memory rather
	// than stored, which is what makes a missed run SKIPPED: a daemon that was
	// down over nine o'clock comes back and waits for tomorrow, instead of
	// firing a morning report in the afternoon. The digest jobs lose nothing by
	// it — they are governed by a three-hour cooldown, not by their tick.
	next map[string]time.Time
	// stamp is the UpdatedAt each job's schedule was computed from, so an edit
	// recomputes and an untouched job keeps its place in the cycle.
	stamp map[string]time.Time
	// running is the set of jobs currently executing. A job that overruns its
	// own interval is skipped rather than doubled up: two digest passes at once
	// race each other to write the same ledger rows.
	running map[string]bool
}

// New builds a scheduler over the ledger and a process seam.
func New(store Store, exec Exec, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		store: store, exec: exec, logger: logger, now: time.Now,
		next: map[string]time.Time{}, stamp: map[string]time.Time{},
		running: map[string]bool{},
	}
}

// WithClock overrides the clock; intended for tests.
func (s *Scheduler) WithClock(now func() time.Time) *Scheduler {
	s.now = now
	return s
}

// Run ticks until ctx is cancelled.
//
// It returns nil on cancellation: that is how this process is asked to stop, not
// a failure to report up the stack — the same reading the socket listener gives
// it.
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("scheduler starting", "tick", tick)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	// Once immediately, so a daemon that has just been restarted does not sit
	// idle for the first tick with a due job in front of it.
	s.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopping")
			return nil
		case <-ticker.C:
			s.pass(ctx)
		}
	}
}

// pass reconciles the schedule and starts whatever is due.
//
// A read that fails is logged and the pass abandoned. The ledger is a local
// file; if it cannot be read, the next tick is fifteen seconds away and there
// is nothing useful to do in between.
func (s *Scheduler) pass(ctx context.Context) {
	jobs, err := s.store.Jobs(ctx)
	if err != nil {
		s.logger.Error("could not read the schedule", "error", err)
		return
	}
	now := s.now()
	for _, job := range s.due(jobs, now) {
		s.start(ctx, job, now)
	}
}

// due reconciles in-memory state against the stored definitions and returns the
// jobs to start now.
//
// Every job it returns is CLAIMED: it is marked running before this returns, so
// two passes cannot both start the same one. The caller owns releasing it —
// start() does that in a defer, through a panic if necessary — and a caller
// that forgets will find the job never runs again.
func (s *Scheduler) due(jobs []Job, now time.Time) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()

	live := make(map[string]bool, len(jobs))
	var ready []Job

	for _, job := range jobs {
		live[job.Name] = true
		if !job.Enabled {
			// Forgotten rather than held: re-enabling should start the cycle
			// from the moment it was re-enabled, not resume a countdown that
			// has been running invisibly for a week.
			delete(s.next, job.Name)
			delete(s.stamp, job.Name)
			continue
		}
		sched, err := Parse(job.Spec)
		if err != nil {
			// Stored specs are validated on the way in, so this is a
			// hand-edited row or a downgrade. Said once per pass and skipped:
			// the alternative is a job that silently never runs.
			s.logger.Error("job has an unreadable schedule; it will not run",
				"job", job.Name, "spec", job.Spec, "error", err)
			continue
		}
		if at, known := s.next[job.Name]; !known || !s.stamp[job.Name].Equal(job.UpdatedAt) {
			// New, or edited. First(now) is what makes an interval job run
			// promptly after a restart and a calendar job wait for its time.
			//
			// Deliberately NOT `continue`: falling through to the due check
			// below is what "promptly" means. Skipping the pass that first
			// sees the job would put every restart a tick behind, and every
			// edit a tick behind that.
			s.next[job.Name] = First(sched, now)
			s.stamp[job.Name] = job.UpdatedAt
		} else if now.Before(at) {
			continue
		}
		if now.Before(s.next[job.Name]) {
			continue
		}
		if s.running[job.Name] {
			// Overrunning its own cadence. Skipped, not queued: two passes of
			// the same digest race each other to write the same ledger rows.
			s.logger.Warn("job is still running; skipping this turn", "job", job.Name)
			s.next[job.Name] = advance(sched, s.next[job.Name], now)
			continue
		}
		s.running[job.Name] = true
		s.next[job.Name] = advance(sched, s.next[job.Name], now)
		ready = append(ready, job)
	}

	// A job deleted underneath us leaves nothing behind.
	for name := range s.next {
		if !live[name] {
			delete(s.next, name)
			delete(s.stamp, name)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	return ready
}

// First is when a newly scheduled job is next due.
//
// An interval job is due IMMEDIATELY: `every 3m` after a restart means the
// digest should be current now, not in three minutes. A calendar job waits for
// its next matching time, because "09:00 on weekdays" said when it wants to
// run and a restart at 14:00 is not it.
func First(sched Schedule, now time.Time) time.Time {
	if _, isInterval := sched.(Interval); isInterval {
		return now
	}
	return sched.Next(now)
}

// advance moves a due time forward past now.
//
// The loop matters for a job that took longer than its own interval, or a
// daemon that was suspended: without it a job an hour behind would fire twenty
// times in twenty ticks catching up, which is the stampede this design exists
// to avoid.
func advance(sched Schedule, from, now time.Time) time.Time {
	next := sched.Next(from)
	for !next.IsZero() && !next.After(now) {
		next = sched.Next(next)
	}
	if next.IsZero() {
		// A calendar expression with nothing left in it. Parked far enough out
		// that it is never due again; the row still renders, and the Home tab
		// says the schedule matches nothing.
		return now.Add(cronHorizon)
	}
	return next
}

// start runs one job on its own goroutine.
func (s *Scheduler) start(ctx context.Context, job Job, at time.Time) {
	go func() {
		defer s.finish(job.Name)
		// A panicking job must not take the daemon with it. Everything else in
		// this process is a click that fails on its own; this is the first
		// thing that runs unattended, and there is nobody to notice.
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("job panicked", "job", job.Name, "panic", r)
			}
		}()
		s.RunNow(ctx, job, at)
	}()
}

// IsRunning reports whether a job is executing right now.
//
// The Home tab draws a running job differently — "started 40s ago" rather than
// a next-due time — because a job that takes minutes is otherwise
// indistinguishable from one that is not firing at all.
func (s *Scheduler) IsRunning(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running[name]
}

// NextRun is when a job is next due, and false when the scheduler has no
// opinion — a job it has not seen yet, or one that is disabled.
func (s *Scheduler) NextRun(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.next[name]
	return at, ok
}

// finish releases the in-flight claim.
func (s *Scheduler) finish(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, name)
}

// RunNow executes one job and records the outcome, ignoring its schedule.
//
// It is what a tick calls and what the Home tab's "Run now" calls, so a manual
// run and a scheduled one are the same code — a "Run now" that took a different
// path would be a way to prove the wrong thing works.
func (s *Scheduler) RunNow(ctx context.Context, job Job, at time.Time) (Result, error) {
	timeout := job.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := s.now()
	out, err := s.exec(ctx, job.Args)
	took := s.now().Sub(started)

	result := Result{Output: string(out), Duration: took}
	if ctx.Err() == context.DeadlineExceeded {
		// Told apart from an ordinary failure, because they are different
		// facts: one job went wrong, the other may well still have been going.
		err = fmt.Errorf("timed out after %s", timeout)
		result.TimedOut = true
	}
	result.Err = err

	if err != nil {
		s.logger.Error("job failed", "job", job.Name, "took", took.Round(time.Millisecond),
			"error", err, "output", result.Output)
	} else {
		s.logger.Info("job ran", "job", job.Name, "took", took.Round(time.Millisecond))
	}
	// The record is the Home tab's only source for "is this working?", so a
	// failure to write it is worth a line of its own — but it does not make the
	// run itself a failure, which already happened one way or the other.
	// Output is kept only for a failure. A successful digest pass prints a
	// paragraph every three minutes and nobody has ever read one; the tail of a
	// failure is the whole reason the Home tab can say what went wrong. The
	// decision is here rather than in the store because it is about what is
	// worth knowing, not about how a row is written.
	tail := ""
	if err != nil {
		tail = Tail(result.Output)
	}
	if recErr := s.store.RecordJobRun(ctx, job.Name, at, took, err, tail); recErr != nil {
		s.logger.Error("could not record a job run", "job", job.Name, "error", recErr)
	}
	return result, err
}

// Result is what one run produced.
type Result struct {
	// Output is the combined stdout and stderr, verbatim.
	Output string
	// Duration is how long it took.
	Duration time.Duration
	// TimedOut reports that the bound was reached rather than the job having
	// failed on its own terms.
	TimedOut bool
	// Err is why it failed, nil on success.
	Err error
}

// Tail is the last few lines of a run's output, cut to what a Slack block will
// carry.
//
// The END rather than the beginning: a job that failed says why last, after
// however much progress it narrated first.
func Tail(output string) string {
	return tailOf(output, outputTailLines, outputTailChars)
}
