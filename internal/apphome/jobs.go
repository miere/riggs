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

// defaultSchedule is what a new job's Frequency box is pre-filled with.
//
// Three minutes, which is what both jobs Riggs took over from Murtaugh use — so
// the common case is one field of typing and the uncommon one is a field the
// admin was going to change anyway.
const defaultSchedule = "3m"

// JQLChecker proves a query before a job is built out of it.
//
// A seam rather than a Jira client, because this package has no business
// holding credentials: the composition root has one already and every other
// dependency here arrives the same way. Nil skips the check, which is what a
// machine with no Jira configured looks like — the digest cannot run there
// either, and refusing to save the job would be a second complaint about the
// same missing setting.
type JQLChecker interface {
	// CheckJQL reports whether Jira will run this query. An error is the reason
	// it will not, in Jira's own words where possible: "the field 'labls' does
	// not exist" is worth more than "invalid JQL".
	CheckJQL(ctx context.Context, jql string) error
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
			Kind:     kindLabel(job),
			Schedule: job.Spec,
			Command:  schedule.Command(job),
			Status:   p.jobStatus(job, now),
			Enabled:  job.Enabled,
		})
	}
	return rows
}

// kindLabel is what a job's kind is called on its row, and empty for a kind
// this build does not know — which the row then says in its own words.
func kindLabel(job notify.Job) string {
	spec, ok := schedule.LookupKind(schedule.KindOf(job))
	if !ok {
		return ""
	}
	return spec.Label
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

// ConfigureGitHubJob opens the pull-request digest's editor.
//
// One modal for the one job, whether or not it exists yet: the form's checkbox
// is what decides which. That is why this is "Configure GitHub Jobs" rather
// than "New GitHub job" — there is one review queue, and the question is always
// whether Riggs is watching it, never which of several to add.
//
// It opens the modal and does nothing else first — a trigger id lives about
// three seconds (§7e) — with one exception it cannot avoid: the form has to be
// pre-filled with the job that exists, and that is a ledger read. It is one
// indexed row from a local SQLite file, which is microseconds; the alternative
// is an empty form that silently forgets the admin's login every time they open
// it to change the cadence.
func (p *Publisher) ConfigureGitHubJob(ctx context.Context, userID, triggerID string) error {
	if err := p.mayOperateJobs(userID, "configure"); err != nil {
		return err
	}
	existing, found, err := p.githubJob(ctx)
	if err != nil {
		return err
	}
	modal := blockkit.GitHubJobModal{
		// Sensible starting points rather than an empty form. Three minutes is
		// what the job Riggs took over from Murtaugh actually used, so the
		// common case is one field of typing.
		Schedule: defaultSchedule,
		Enabled:  found,
	}
	if found {
		modal.Name = existing.Name
		modal.Login = schedule.Param(existing, schedule.ParamLogin)
		modal.Schedule = existing.Spec
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, modal.View())
}

// NewJiraJob opens an empty ticket-digest editor.
//
// It opens the modal and does nothing else first: a trigger id lives about
// three seconds (§7e), and unlike the GitHub form there is nothing to pre-fill
// — a new query is a new question.
func (p *Publisher) NewJiraJob(ctx context.Context, userID, triggerID string) error {
	if err := p.mayOperateJobs(userID, "create"); err != nil {
		return err
	}
	return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, blockkit.JiraJobModal{
		Schedule: defaultSchedule,
	}.View())
}

// EditJob opens the editor for an existing job — whichever editor that is.
//
// The row's Edit option is one control over two forms, dispatched on the job's
// kind. It has to be: the row is where somebody looks when they want to change
// something, and asking them to remember whether this one is configured from
// the GitHub option or the Jira one is asking them to hold the implementation
// in their head.
//
// A job whose kind this build does not know opens NOTHING, and says so. There
// is no form for it, and the two alternatives — guessing at a form, or opening
// an empty one — both end with a save that rewrites a job into something it was
// not.
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
	switch schedule.KindOf(job) {
	case schedule.KindGitHubReviews:
		return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, blockkit.GitHubJobModal{
			Name:     job.Name,
			Login:    schedule.Param(job, schedule.ParamLogin),
			Schedule: job.Spec,
			Enabled:  true,
		}.View())
	case schedule.KindJiraTickets:
		return p.deps.Modals.OpenView(ctx, p.deps.BotToken, triggerID, blockkit.JiraJobModal{
			Name:     job.Name,
			JQL:      schedule.Param(job, schedule.ParamJQL),
			Schedule: job.Spec,
		}.View())
	}
	return fmt.Errorf("%s is a %q job, which this build of Riggs cannot edit — update Riggs, or delete it",
		job.Name, job.Type)
}

// SaveGitHubJob records the pull-request digest, creating, updating or deleting
// it.
//
// enabled is the checkbox, and it is the control that decides which of the
// three this call is — because the question the form asks is whether Riggs
// watches the review queue at all, and "no" has to be expressible.
//
// It takes no job identity, unlike every other save on this surface. There is
// nothing for the caller to name: the digest is a singleton, and which row that
// is at this moment is a question only the ledger can answer.
//
// Unticked DELETES rather than disables, which is the harsher reading and the
// deliberate one. Disable already exists, on the row, one click away, and it
// keeps the definition and the history; a checkbox that quietly did the same
// thing would leave the admin with two controls that look different and are not.
// The field says so in its own hint, at the moment of ticking.
// The job it acts on is RE-RESOLVED here rather than taken from the form's
// private_metadata. The modal was opened against whatever existed then, and a
// modal can sit on somebody's screen for a long time; a digest created or
// deleted since — from a terminal, or from a second Slack client — would
// otherwise be joined by a second one under Riggs' own name, and the two would
// race to write the same message. This re-read is what makes "there is at most
// one" true rather than hoped for.
func (p *Publisher) SaveGitHubJob(ctx context.Context, userID, login, spec string, enabled bool) error {
	if err := p.mayOperateJobs(userID, "save"); err != nil {
		return err
	}
	existing, found, err := p.githubJob(ctx)
	if err != nil {
		return err
	}

	if !enabled {
		if !found {
			// Unticked a job that is not there. Nothing to do, and no error:
			// the admin's intent and the state of the world agree.
			return nil
		}
		return p.DeleteJob(ctx, userID, existing.Name)
	}

	// A job that already exists keeps its own name, whatever it is: one adopted
	// from an older ledger is called what the operator called it, and renaming a
	// row on an edit would break every log line about it.
	name := schedule.GitHubJobName
	original := ""
	if found {
		name, original = existing.Name, existing.Name
	}
	job, err := schedule.NewGitHubJob(name, login, spec)
	if err != nil {
		return err
	}
	return p.saveJob(ctx, userID, original, job)
}

// SaveJiraJob records a submitted ticket digest, creating or updating it.
//
// original is the private_metadata: the job being edited, or empty for a new
// one. A new job's name comes from the form; an existing job's does not, which
// is why "rename" is not an operation here.
//
// The query is checked against Jira BEFORE it is stored, when there is anything
// to check it with. A JQL that does not parse is not a job that fails once —
// it is a job that fails every three minutes, for good, into a log, and the
// admin who typed it has already closed the modal and moved on. One search
// against the real tenant is the difference between finding out now and finding
// out never.
func (p *Publisher) SaveJiraJob(ctx context.Context, userID, original, name, jql, spec string) error {
	if err := p.mayOperateJobs(userID, "save"); err != nil {
		return err
	}
	if original != "" {
		name = original
	}
	job, err := schedule.NewJiraJob(name, jql, spec)
	if err != nil {
		return err
	}
	if p.deps.JQL != nil {
		if err := p.deps.JQL.CheckJQL(ctx, schedule.Param(job, schedule.ParamJQL)); err != nil {
			p.deps.Logger.Warn("refused a job whose JQL Jira would not run",
				"job", job.Name, "user", userID, "error", err)
			return fmt.Errorf("Jira would not run that query, so the job was not saved: %w", err)
		}
	}
	return p.saveJob(ctx, userID, original, job)
}

// saveJob is the half both editors share: the create-or-update rules, the
// write, and the redraw.
//
// It is shared rather than duplicated because these rules are about a job's
// IDENTITY — a name already in use, a row deleted from another window, the
// enabled flag that is a menu control and not a form field — and none of that
// depends on which kind of job it is. The parts that do differ are already
// decided by the time this is called: the caller built the job.
func (p *Publisher) saveJob(ctx context.Context, userID, original string, job notify.Job) error {
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
	p.deps.Logger.Info("job saved", "job", job.Name, "type", job.Type, "spec", job.Spec, "user", userID)
	p.republish(ctx, userID)
	return nil
}

// githubJob finds the pull-request digest, of which there is at most one.
//
// Found by KIND rather than by name. The name is Riggs' own for a job it
// created, but a ledger that has been through the migration keeps whatever the
// operator called theirs — and looking up schedule.GitHubJobName would then
// find nothing, offer an empty form, and create a SECOND digest racing the
// first to write the same message.
//
// Two of them is not a state this can be in — every door that creates one comes
// through here first — so the first is returned and the rest are logged. A
// hand-edited ledger is not worth an error the admin cannot act on.
func (p *Publisher) githubJob(ctx context.Context) (notify.Job, bool, error) {
	jobs, err := p.deps.Jobs.Jobs(ctx)
	if err != nil {
		return notify.Job{}, false, err
	}
	var found notify.Job
	seen := 0
	for _, job := range jobs {
		if schedule.KindOf(job) != schedule.KindGitHubReviews {
			continue
		}
		seen++
		if seen == 1 {
			found = job
		}
	}
	if seen > 1 {
		p.deps.Logger.Warn("more than one pull-request digest is scheduled; configuring the first",
			"count", seen, "job", found.Name)
	}
	return found, seen > 0, nil
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
