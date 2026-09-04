package apphome

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/schedule"
)

// The Jobs half of the Home tab: what is scheduled, how it went, and the
// controls that change it.
//
// This is the surface that replaces going and reading another tool's database
// to find out what Riggs is running. A schedule you cannot see is one you
// assume is working.

// JobStore is the ledger's job table, narrowed to what this surface needs.
type JobStore interface {
	Jobs(ctx context.Context) ([]notify.Job, error)
	Job(ctx context.Context, name string) (notify.Job, bool, error)
	SaveJob(ctx context.Context, job notify.Job) error
	SetJobEnabled(ctx context.Context, name string, enabled bool, at time.Time) (bool, error)
	DeleteJob(ctx context.Context, name string) (bool, error)
}

// JobRunner is the scheduler, narrowed to what this surface needs: what is
// happening now, what happens next, and the ability to say "now".
type JobRunner interface {
	RunNow(ctx context.Context, job notify.Job, at time.Time) (schedule.Result, error)
	IsRunning(name string) bool
	NextRun(name string) (time.Time, bool)
}

// jobRows renders the Jobs section for the admin.
func (p *Publisher) jobRows(ctx context.Context, admin bool) []blockkit.HomeJob {
	if !admin || p.deps.Jobs == nil {
		return nil
	}
	jobs, err := p.deps.Jobs.Jobs(ctx)
	if err != nil {
		// Logged, not surfaced. A ledger blip should cost the admin the Jobs
		// section for one publish, not replace their Home tab with an error —
		// the same call the update check makes.
		p.deps.Logger.Error("could not read the schedule for the app home", "error", err)
		return nil
	}
	now := p.now()
	rows := make([]blockkit.HomeJob, 0, len(jobs))
	for _, job := range jobs {
		rows = append(rows, blockkit.HomeJob{
			ID:       job.Name,
			Schedule: job.Spec,
			Command:  schedule.Command(job),
			Status:   p.jobStatus(job, now),
			Enabled:  job.Enabled,
		})
	}
	return rows
}

// jobStatus is the row's third line: what happened, and what happens next.
//
// Rendered here rather than in blockkit because it is arithmetic on a clock,
// and a package that lays out JSON has no business holding one.
func (p *Publisher) jobStatus(job notify.Job, now time.Time) string {
	if p.deps.Runner != nil && p.deps.Runner.IsRunning(job.Name) {
		// Said first, and before the disabled check: a job disabled while it
		// was mid-run is still mid-run, and reporting it as idle would have
		// somebody wondering why the next one is late.
		return blockkit.MarkerRunning + " running now"
	}
	if !job.Enabled {
		// No next-run time, because there is not one. A disabled job showing
		// "next in 40s" is the kind of detail that makes a reader doubt the
		// whole panel.
		return blockkit.MarkerWarning + " disabled"
	}

	var parts []string
	switch {
	case !job.Ran():
		parts = append(parts, "never run")
	case job.LastOK:
		parts = append(parts, fmt.Sprintf("%s ran %s ago in %s", blockkit.MarkerDone,
			since(now, job.LastRun), round(job.LastDuration)))
	default:
		failure := fmt.Sprintf("%s failed %s ago", blockkit.MarkerFailed, since(now, job.LastRun))
		if reason := strings.TrimSpace(job.LastError); reason != "" {
			failure += " — " + reason
		}
		parts = append(parts, failure)
	}
	if next := p.nextRun(job.Name); next != "" {
		parts = append(parts, "next "+next)
	}
	return strings.Join(parts, " · ")
}

// nextRun renders when a job is next due, and empty when nothing knows.
//
// Nothing knows in two ordinary cases: a scheduler that has not ticked since
// the job was created, and a calendar expression parked past the horizon
// because it matches no date that will ever exist.
func (p *Publisher) nextRun(name string) string {
	if p.deps.Runner == nil {
		return ""
	}
	at, known := p.deps.Runner.NextRun(name)
	if !known || at.IsZero() {
		return ""
	}
	d := at.Sub(p.now())
	if d <= 0 {
		return "due now"
	}
	if d > 48*time.Hour {
		// Past a couple of days "in 1704h" stops being a duration anybody can
		// read, and the date is what they wanted anyway.
		return "on " + at.Format("Mon 2 Jan 15:04")
	}
	return "in " + humanDuration(d)
}

// since renders how long ago t was.
func since(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	if d < 0 {
		// A clock that moved backwards, or a row written by another process a
		// moment ahead of this one. "just now" beats "-3s ago".
		return "just now"
	}
	return humanDuration(d)
}

// humanDuration renders a duration at one significant unit.
//
// One unit, not two: this sits at the end of a status line that already carries
// an outcome and a next run, and "2h 14m 6s" spends a line's worth of width
// answering a question nobody asked that precisely.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// round trims a run duration to something readable.
func round(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// now is the publisher's clock, injected so the status lines can be asserted on.
func (p *Publisher) now() time.Time {
	if p.deps.Now != nil {
		return p.deps.Now()
	}
	return time.Now()
}

// --- the controls -----------------------------------------------------------

// NewJob opens an empty job editor.
//
// It opens the modal and does nothing else first: a trigger id lives about
// three seconds (§7e).
func (p *Publisher) NewJob(ctx context.Context, userID, triggerID string) error {
	if err := p.mayOperateJobs(userID, "create"); err != nil {
		return err
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, blockkit.JobModal{
		// Sensible starting points rather than an empty form. Both are what the
		// jobs Riggs took over from Murtaugh actually use, so the common case
		// is one field of typing.
		Schedule: "3m",
		Timeout:  schedule.DefaultTimeout.String(),
	}.View())
}

// EditJob opens the editor for an existing job.
func (p *Publisher) EditJob(ctx context.Context, userID, name, triggerID string) error {
	if err := p.mayOperateJobs(userID, "edit"); err != nil {
		return err
	}
	job, found, err := p.deps.Jobs.Job(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		// A Home tab published before somebody deleted the job.
		return fmt.Errorf("there is no job called %q any more", name)
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, blockkit.JobModal{
		Name:     job.Name,
		Command:  schedule.Command(job),
		Schedule: job.Spec,
		Timeout:  job.Timeout.String(),
	}.View())
}

// SaveJob records a submitted job, creating or updating it.
//
// original is the private_metadata: the job being edited, or empty for a new
// one. A new job's name comes from the form; an existing job's does not, which
// is why "rename" is not an operation here.
func (p *Publisher) SaveJob(ctx context.Context, userID, original, name, command, spec, timeout string) error {
	if err := p.mayOperateJobs(userID, "save"); err != nil {
		return err
	}
	if original != "" {
		name = original
	}
	d, err := parseTimeout(timeout)
	if err != nil {
		return err
	}
	job, err := schedule.NewJob(strings.TrimSpace(name), schedule.SplitArgs(command), spec, d, true)
	if err != nil {
		return err
	}
	if original == "" {
		// Creating. A name already in use would silently replace somebody
		// else's job, and the two would be indistinguishable afterwards.
		if _, exists, err := p.deps.Jobs.Job(ctx, job.Name); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("there is already a job called %q; edit it from its own row", job.Name)
		}
	} else {
		// Editing. Enabled is a menu control, not a form field, so it is
		// carried over rather than reset to on by every save.
		existing, found, err := p.deps.Jobs.Job(ctx, original)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("there is no job called %q any more", original)
		}
		job.Enabled = existing.Enabled
		job.CreatedAt = existing.CreatedAt
	}
	job.UpdatedAt = p.now()
	if err := p.deps.Jobs.SaveJob(ctx, job); err != nil {
		return err
	}
	p.deps.Logger.Info("job saved", "job", job.Name, "spec", job.Spec, "user", userID)
	p.republish(ctx, userID)
	return nil
}

// ToggleJob pauses or resumes a job.
func (p *Publisher) ToggleJob(ctx context.Context, userID, name string) error {
	if err := p.mayOperateJobs(userID, "toggle"); err != nil {
		return err
	}
	job, found, err := p.deps.Jobs.Job(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("there is no job called %q any more", name)
	}
	if _, err := p.deps.Jobs.SetJobEnabled(ctx, name, !job.Enabled, p.now()); err != nil {
		return err
	}
	p.deps.Logger.Info("job toggled", "job", name, "enabled", !job.Enabled, "user", userID)
	p.republish(ctx, userID)
	return nil
}

// ConfirmDeleteJob opens the second chance, and deletes nothing.
//
// The Delete option is the only control on the Jobs section that does not act
// on the click. Slack has no per-option confirmation on an overflow (§7e), so
// the question is a modal; DeleteJob is what its submission reaches.
//
// The job is NOT read here. A trigger id lives about three seconds, and a
// ledger read before views.open spends some of them to answer a question the
// submission has to ask again anyway — a job can be deleted from another
// window while the modal is open.
func (p *Publisher) ConfirmDeleteJob(ctx context.Context, userID, name, triggerID string) error {
	if err := p.mayOperateJobs(userID, "delete"); err != nil {
		return err
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID,
		blockkit.JobDeleteModal{Name: name}.View())
}

// DeleteJob forgets a job and its history.
//
// Reached from the confirmation modal's submission, never from a click. The
// authorisation is checked again rather than trusted from whoever opened the
// modal: a view submission is an inbound message like any other, and "it must
// have been the admin, the modal opened" is exactly the assumption worth not
// making.
func (p *Publisher) DeleteJob(ctx context.Context, userID, name string) error {
	if err := p.mayOperateJobs(userID, "delete"); err != nil {
		return err
	}
	found, err := p.deps.Jobs.DeleteJob(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("there is no job called %q any more", name)
	}
	p.deps.Logger.Info("job deleted", "job", name, "user", userID)
	p.republish(ctx, userID)
	return nil
}

// RunJob runs one job now, whatever its schedule says.
//
// It redraws the tab BEFORE running, so the row says "running now" for the
// minutes the run takes — a control that shows nothing for two minutes reads as
// one that did nothing — and again afterwards with the outcome.
func (p *Publisher) RunJob(ctx context.Context, userID, name string) error {
	if err := p.mayOperateJobs(userID, "run"); err != nil {
		return err
	}
	if p.deps.Runner == nil {
		return fmt.Errorf("this build has no scheduler, so nothing can be run")
	}
	job, found, err := p.deps.Jobs.Job(ctx, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("there is no job called %q any more", name)
	}
	if p.deps.Runner.IsRunning(name) {
		return fmt.Errorf("%s is already running", name)
	}

	p.deps.Logger.Info("job run requested", "job", name, "user", userID)
	runErr := func() error {
		defer p.republish(ctx, userID)
		p.republish(ctx, userID)
		_, err := p.deps.Runner.RunNow(ctx, job, p.now())
		return err
	}()
	if runErr != nil {
		// The tab has been redrawn with the failure on the row, which names the
		// job and the reason. Marked, so the daemon does not report the same
		// thing again in a DM.
		return slackReported(fmt.Errorf("%s failed: %w", name, runErr))
	}
	return nil
}

// mayOperateJobs is the gate every job control re-checks.
//
// The rows are only ever rendered for the admin, but an action_id and a
// block_id are just strings in a payload, and these ones write to the ledger
// and start processes.
func (p *Publisher) mayOperateJobs(userID, verb string) error {
	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied a job operation from a non-admin", "user", userID, "verb", verb)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Jobs == nil || p.deps.Modals == nil {
		return fmt.Errorf("apphome: the schedule is not wired up in this build")
	}
	return nil
}

// parseTimeout reads the modal's timeout field. Empty takes the default.
func parseTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (e.g. 2m, 30s)", raw)
	}
	return d, nil
}
