package apphome

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/schedule"
)

func when(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeJobs is the ledger's job table, remembered.
type fakeJobs struct {
	mu   sync.Mutex
	jobs map[string]notify.Job
	err  error
}

func newJobs(jobs ...notify.Job) *fakeJobs {
	f := &fakeJobs{jobs: map[string]notify.Job{}}
	for _, job := range jobs {
		f.jobs[job.Name] = job
	}
	return f
}

func (f *fakeJobs) Jobs(context.Context) ([]notify.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	// Sorted, as the real store returns them.
	var names []string
	for name := range f.jobs {
		names = append(names, name)
	}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	out := make([]notify.Job, 0, len(names))
	for _, name := range names {
		out = append(out, f.jobs[name])
	}
	return out, nil
}

func (f *fakeJobs) Job(_ context.Context, name string) (notify.Job, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[name]
	return job, ok, f.err
}

func (f *fakeJobs) SaveJob(_ context.Context, job notify.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.jobs[job.Name] = job
	return nil
}

func (f *fakeJobs) SetJobEnabled(_ context.Context, name string, enabled bool, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[name]
	if !ok {
		return false, nil
	}
	job.Enabled = enabled
	f.jobs[name] = job
	return true, nil
}

func (f *fakeJobs) DeleteJob(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.jobs[name]
	delete(f.jobs, name)
	return ok, nil
}

// fakeRunner is the scheduler.
type fakeRunner struct {
	mu      sync.Mutex
	running map[string]bool
	next    map[string]time.Time
	ran     []string
	err     error
}

func newRunner() *fakeRunner {
	return &fakeRunner{running: map[string]bool{}, next: map[string]time.Time{}}
}

func (f *fakeRunner) RunNow(_ context.Context, job notify.Job, _ time.Time) (schedule.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, job.Name)
	return schedule.Result{}, f.err
}

func (f *fakeRunner) IsRunning(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[name]
}

func (f *fakeRunner) NextRun(name string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.next[name]
	return at, ok
}

// jobRig assembles a publisher with the schedule wired.
type jobRig struct {
	*Publisher
	views  *fakeViews
	modals *fakeModals
	jobs   *fakeJobs
	runner *fakeRunner
}

func newJobRig(t *testing.T, jobs ...notify.Job) *jobRig {
	t.Helper()
	r := &jobRig{views: &fakeViews{}, modals: &fakeModals{},
		jobs: newJobs(jobs...), runner: newRunner()}
	r.Publisher = New(Deps{
		Version: "v1.0.0", BotToken: "xoxb", AdminUserID: admin,
		Views: r.views, Modals: r.modals, Jobs: r.jobs, Runner: r.runner,
		Now:     func() time.Time { return when("2026-09-01 09:00") },
		Restart: func(context.Context) error { return nil },
		Logger:  quiet(),
	})
	return r
}

func sampleJob() notify.Job {
	return notify.Job{
		Name: "github-review-queue", Type: string(schedule.KindGitHubReviews),
		Params: map[string]string{schedule.ParamLogin: "miere"},
		Spec:   "3m", Enabled: true,
	}
}

// sampleJiraJob is the other kind, for the tests that need two rows or the
// editor that is not the singleton's.
func sampleJiraJob(name, jql string) notify.Job {
	return notify.Job{
		Name: name, Type: string(schedule.KindJiraTickets),
		Params: map[string]string{schedule.ParamJQL: jql},
		Spec:   "3m", Enabled: true,
	}
}

// jobRow finds one job's rendered row in the last published view.
func (p publishedView) jobRow(id string) (map[string]any, bool) {
	blocks, _ := p.view["blocks"].([]any)
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if block["block_id"] == blockkit.HomeJobBlockPrefix+id {
			return block, true
		}
	}
	return nil, false
}

func jobRowText(t *testing.T, r *jobRig, id string) string {
	t.Helper()
	row, ok := r.views.last().jobRow(id)
	if !ok {
		t.Fatalf("no row for %s", id)
	}
	return row["text"].(map[string]any)["text"].(string)
}

// --- the status line ---------------------------------------------------------

func TestASuccessfulRunReportsWhenAndHowLong(t *testing.T) {
	job := sampleJob()
	job.LastRun, job.LastOK, job.LastDuration = when("2026-09-01 08:58"), true, 1400*time.Millisecond
	r := newJobRig(t, job)
	r.runner.next[job.Name] = when("2026-09-01 09:01")

	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	text := jobRowText(t, r, job.Name)
	for _, want := range []string{blockkit.MarkerDone, "ran 2m ago", "1.4s", "next in 1m"} {
		if !strings.Contains(text, want) {
			t.Errorf("status = %q, want %q", text, want)
		}
	}
}

// The one row anybody stops on is the one that says it did not work, so it
// carries the reason.
func TestAFailedRunReportsWhy(t *testing.T) {
	job := sampleJob()
	job.LastRun, job.LastOK = when("2026-09-01 08:56"), false
	job.LastError = "exit status 128"
	r := newJobRig(t, job)

	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	text := jobRowText(t, r, job.Name)
	if !strings.Contains(text, blockkit.MarkerFailed) || !strings.Contains(text, "exit status 128") {
		t.Fatalf("status = %q", text)
	}
}

// A job that takes minutes is otherwise indistinguishable from one that is not
// firing at all.
func TestARunningJobSaysSo(t *testing.T) {
	job := sampleJob()
	r := newJobRig(t, job)
	r.runner.running[job.Name] = true

	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if text := jobRowText(t, r, job.Name); !strings.Contains(text, "running now") {
		t.Fatalf("status = %q", text)
	}
}

// A disabled job showing "next in 40s" is the kind of detail that makes a
// reader doubt the whole panel.
func TestADisabledJobShowsNoNextRun(t *testing.T) {
	job := sampleJob()
	job.Enabled = false
	r := newJobRig(t, job)
	r.runner.next[job.Name] = when("2026-09-01 09:01")

	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	text := jobRowText(t, r, job.Name)
	if !strings.Contains(text, "disabled") {
		t.Fatalf("status = %q", text)
	}
	if strings.Contains(text, "next") {
		t.Fatalf("a disabled job advertises a next run: %q", text)
	}
}

func TestAJobThatHasNeverRunSaysSo(t *testing.T) {
	r := newJobRig(t, sampleJob())
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if text := jobRowText(t, r, "github-review-queue"); !strings.Contains(text, "never run") {
		t.Fatalf("status = %q", text)
	}
}

// Past a couple of days "in 1704h" stops being a duration anybody can read.
func TestADistantNextRunShowsTheDate(t *testing.T) {
	r := newJobRig(t, sampleJob())
	r.runner.next["github-review-queue"] = when("2026-09-20 09:00")
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if text := jobRowText(t, r, "github-review-queue"); !strings.Contains(text, "Sun 20 Sep") {
		t.Fatalf("status = %q", text)
	}
}

// --- the controls ------------------------------------------------------------

// The GitHub form is opened on whatever singleton exists — which may not be
// called what this build would name one, because a ledger that has been through
// the migration keeps the operator's own name for it.
func TestConfigureGitHubJobPreFillsTheExistingOne(t *testing.T) {
	adopted := sampleJob()
	adopted.Name = "quick-review-poll"
	r := newJobRig(t, adopted)

	if err := r.ConfigureGitHubJob(context.Background(), admin, "trigger-1"); err != nil {
		t.Fatalf("ConfigureGitHubJob: %v", err)
	}
	view := r.modals.view
	if view["callback_id"] != blockkit.GitHubJobModalCallbackID {
		t.Fatalf("callback = %v", view["callback_id"])
	}
	if view["private_metadata"] != "quick-review-poll" {
		t.Fatalf("private_metadata = %v, want the job that actually exists", view["private_metadata"])
	}
	if got := modalField(t, view, blockkit.GitHubJobModalLoginBlockID); got != "miere" {
		t.Fatalf("login pre-fill = %q", got)
	}
	// Ticked, because the job exists. The checkbox is what says so.
	element := modalBlock(t, view, blockkit.GitHubJobModalEnabledBlockID)["element"].(map[string]any)
	if _, ticked := element["initial_options"]; !ticked {
		t.Fatal("the checkbox is unticked for a job that exists")
	}
}

// With no job yet the form is empty but not blank: the cadence both adopted
// jobs actually use is offered, so the common case is one field of typing.
func TestConfigureGitHubJobOffersADefaultCadence(t *testing.T) {
	r := newJobRig(t)
	if err := r.ConfigureGitHubJob(context.Background(), admin, "trigger-1"); err != nil {
		t.Fatalf("ConfigureGitHubJob: %v", err)
	}
	if got := modalField(t, r.modals.view, blockkit.GitHubJobModalScheduleBlockID); got != "3m" {
		t.Fatalf("schedule pre-fill = %q", got)
	}
	element := modalBlock(t, r.modals.view, blockkit.GitHubJobModalEnabledBlockID)["element"].(map[string]any)
	if _, ticked := element["initial_options"]; ticked {
		t.Fatal("the checkbox is ticked for a job that does not exist")
	}
}

func TestNewJiraJobOpensAnEmptyEditor(t *testing.T) {
	r := newJobRig(t)
	if err := r.NewJiraJob(context.Background(), admin, "trigger-1"); err != nil {
		t.Fatalf("NewJiraJob: %v", err)
	}
	if r.modals.view["callback_id"] != blockkit.JiraJobModalCallbackID {
		t.Fatalf("callback = %v", r.modals.view["callback_id"])
	}
	if got := modalField(t, r.modals.view, blockkit.JiraJobModalScheduleBlockID); got != "3m" {
		t.Fatalf("schedule pre-fill = %q", got)
	}
}

// Ticking the box on a machine with no digest creates one, and Riggs names it:
// the alternative is a field whose answer never matters and cannot be changed.
func TestSaveGitHubJobCreatesTheSingleton(t *testing.T) {
	r := newJobRig(t)
	ctx := context.Background()

	if err := r.SaveGitHubJob(ctx, admin, "miere", "5m", true); err != nil {
		t.Fatalf("SaveGitHubJob: %v", err)
	}
	job, found, _ := r.jobs.Job(ctx, schedule.GitHubJobName)
	if !found {
		t.Fatal("the job was not created")
	}
	if schedule.Command(job) != "git pr --bulk miere" {
		t.Fatalf("command = %q", schedule.Command(job))
	}
	if job.Spec != "5m" || !job.Enabled {
		t.Fatalf("job = %+v", job)
	}
	if _, ok := r.views.last().jobRow(schedule.GitHubJobName); !ok {
		t.Fatal("the tab was not redrawn with the new job")
	}
}

// Unticking DELETES. That is the harsher of the two readings and the deliberate
// one — Disable already exists on the row, and keeps both the definition and
// the history.
func TestUntickingTheGitHubJobDeletesIt(t *testing.T) {
	r := newJobRig(t, sampleJob())
	ctx := context.Background()

	if err := r.SaveGitHubJob(ctx, admin, "miere", "3m", false); err != nil {
		t.Fatalf("SaveGitHubJob: %v", err)
	}
	if _, found, _ := r.jobs.Job(ctx, "github-review-queue"); found {
		t.Fatal("the job survived being unticked")
	}
	if _, ok := r.views.last().jobRow("github-review-queue"); ok {
		t.Fatal("the tab still shows the deleted job")
	}
}

// Unticking a job that was never there is not an error: the admin's intent and
// the state of the world already agree.
func TestUntickingWhenThereIsNoJobDoesNothing(t *testing.T) {
	r := newJobRig(t)
	if err := r.SaveGitHubJob(context.Background(), admin, "", "", false); err != nil {
		t.Fatalf("SaveGitHubJob: %v", err)
	}
}

func TestSaveJiraJobCreatesAndRedraws(t *testing.T) {
	const jql = `project = NYX AND labels = "ai-able" AND status = "Ready"`
	r := newJobRig(t)

	if err := r.SaveJiraJob(context.Background(), admin, "", "nightly", jql, "0 9 * * 1-5"); err != nil {
		t.Fatalf("SaveJiraJob: %v", err)
	}
	job, found, _ := r.jobs.Job(context.Background(), "nightly")
	if !found {
		t.Fatal("the job was not created")
	}
	// The query arrives as ONE parameter, quoting and all. It never goes near a
	// splitter, which is the whole reason the form asks for it directly.
	if got := schedule.Param(job, schedule.ParamJQL); got != jql {
		t.Fatalf("jql = %q", got)
	}
	if job.Spec != "0 9 * * 1-5" {
		t.Fatalf("job = %+v", job)
	}
	if _, ok := r.views.last().jobRow("nightly"); !ok {
		t.Fatal("the tab was not redrawn with the new job")
	}
}

// A query Jira will not run is a job that fails every three minutes, for good,
// into a log — and the admin who typed it has closed the modal and moved on.
func TestSaveJiraJobRefusesAQueryJiraRejects(t *testing.T) {
	r := newJobRig(t)
	r.deps.JQL = jqlRefuser{err: errors.New("the field 'labls' does not exist")}

	err := r.SaveJiraJob(context.Background(), admin, "", "nightly", "labls = x", "3m")
	if err == nil {
		t.Fatal("a query Jira rejected was saved")
	}
	if !strings.Contains(err.Error(), "labls") {
		t.Fatalf("err = %v, want Jira's own words", err)
	}
	if _, found, _ := r.jobs.Job(context.Background(), "nightly"); found {
		t.Fatal("the job was created anyway")
	}
}

// A machine with no Jira configured cannot check the query, and must not refuse
// to save it on those grounds: the digest cannot run there either, and that is
// one complaint about one missing setting.
func TestSaveJiraJobWithNoCheckerStillSaves(t *testing.T) {
	r := newJobRig(t)
	if r.deps.JQL != nil {
		t.Fatal("the rig has a checker; this test is about not having one")
	}
	if err := r.SaveJiraJob(context.Background(), admin, "", "nightly", "project = NYX", "3m"); err != nil {
		t.Fatalf("SaveJiraJob: %v", err)
	}
}

// A name already in use would silently replace somebody else's job, and the two
// would be indistinguishable afterwards.
func TestCreatingADuplicateIsRefused(t *testing.T) {
	r := newJobRig(t, sampleJiraJob("nightly", "project = NYX"))
	err := r.SaveJiraJob(context.Background(), admin, "", "nightly", "project = PLAT", "3m")
	if err == nil {
		t.Fatal("a duplicate name was accepted")
	}
	if !strings.Contains(err.Error(), "already a job") {
		t.Fatalf("err = %v", err)
	}
}

// Enabled is a menu control, not a form field, so it is carried over rather
// than reset to on by every save.
func TestEditingKeepsWhetherTheJobIsPaused(t *testing.T) {
	job := sampleJiraJob("nightly", "project = NYX")
	job.Enabled = false
	r := newJobRig(t, job)

	if err := r.SaveJiraJob(context.Background(), admin, job.Name, "", "project = PLAT", "5m"); err != nil {
		t.Fatalf("SaveJiraJob: %v", err)
	}
	saved, _, _ := r.jobs.Job(context.Background(), job.Name)
	if saved.Enabled {
		t.Fatal("editing a paused job resumed it")
	}
	if saved.Spec != "5m" || schedule.Param(saved, schedule.ParamJQL) != "project = PLAT" {
		t.Fatalf("the edit did not take: %+v", saved)
	}
}

// An unreadable schedule must not reach the ledger, where the scheduler would
// log it every fifteen seconds and never run it.
func TestSaveJobRefusesAnUnreadableSchedule(t *testing.T) {
	r := newJobRig(t)
	err := r.SaveJiraJob(context.Background(), admin, "", "nightly", "project = NYX", "weekly")
	if err == nil {
		t.Fatal("an unreadable schedule was saved")
	}
	if _, found, _ := r.jobs.Job(context.Background(), "nightly"); found {
		t.Fatal("the job was created anyway")
	}
}

// The row's Edit is one control over two forms. Asking somebody to remember
// which menu option configured this one is asking them to hold the
// implementation in their head.
func TestEditJobOpensTheRightEditor(t *testing.T) {
	r := newJobRig(t, sampleJob(), sampleJiraJob("nightly", "project = NYX"))
	ctx := context.Background()

	if err := r.EditJob(ctx, admin, "github-review-queue", "t1"); err != nil {
		t.Fatalf("EditJob: %v", err)
	}
	if r.modals.view["callback_id"] != blockkit.GitHubJobModalCallbackID {
		t.Fatalf("callback = %v, want the GitHub editor", r.modals.view["callback_id"])
	}

	if err := r.EditJob(ctx, admin, "nightly", "t2"); err != nil {
		t.Fatalf("EditJob: %v", err)
	}
	if r.modals.view["callback_id"] != blockkit.JiraJobModalCallbackID {
		t.Fatalf("callback = %v, want the Jira editor", r.modals.view["callback_id"])
	}
	if got := modalField(t, r.modals.view, blockkit.JiraJobModalJQLBlockID); got != "project = NYX" {
		t.Fatalf("jql pre-fill = %q", got)
	}
}

// A job whose kind this build does not know opens NOTHING. Guessing at a form,
// or opening an empty one, both end with a save that rewrites the job into
// something it was not.
func TestEditJobRefusesAKindItDoesNotKnow(t *testing.T) {
	future := notify.Job{Name: "future", Type: "slack-digest", Spec: "3m", Enabled: true}
	r := newJobRig(t, future)

	err := r.EditJob(context.Background(), admin, "future", "t1")
	if err == nil {
		t.Fatal("a job of an unknown kind opened an editor")
	}
	if r.modals.view != nil {
		t.Fatalf("a modal was opened anyway: %v", r.modals.view)
	}
	if !strings.Contains(err.Error(), "update Riggs") {
		t.Fatalf("err = %v, want it to say what to do", err)
	}
}

// The row says what kind of job it is, because the parameter no longer does:
// `miere` is a GitHub login only if you already know which job you are reading.
func TestTheRowNamesTheKind(t *testing.T) {
	r := newJobRig(t, sampleJob(), sampleJiraJob("nightly", "project = NYX"))
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if text := jobRowText(t, r, "github-review-queue"); !strings.Contains(text, "Pull Requests") {
		t.Fatalf("row = %q", text)
	}
	if text := jobRowText(t, r, "nightly"); !strings.Contains(text, "Jira tickets") {
		t.Fatalf("row = %q", text)
	}
}

// jqlRefuser is a JQLChecker that always says no.
type jqlRefuser struct{ err error }

func (j jqlRefuser) CheckJQL(context.Context, string) error { return j.err }

// modalBlock finds one input block in an opened modal.
func modalBlock(t *testing.T, view map[string]any, blockID string) map[string]any {
	t.Helper()
	for _, b := range view["blocks"].([]any) {
		block := b.(map[string]any)
		if block["block_id"] == blockID {
			return block
		}
	}
	t.Fatalf("no block %q in %v", blockID, view)
	return nil
}

// modalField reads one input block's pre-filled value.
func modalField(t *testing.T, view map[string]any, blockID string) string {
	t.Helper()
	element, _ := modalBlock(t, view, blockID)["element"].(map[string]any)
	value, _ := element["initial_value"].(string)
	return value
}

func TestToggleAndDelete(t *testing.T) {
	r := newJobRig(t, sampleJob())
	ctx := context.Background()

	if err := r.ToggleJob(ctx, admin, "github-review-queue"); err != nil {
		t.Fatalf("ToggleJob: %v", err)
	}
	job, _, _ := r.jobs.Job(ctx, "github-review-queue")
	if job.Enabled {
		t.Fatal("the job was not paused")
	}
	// Disabling keeps the definition: "off for now" and "deleted" are different
	// intentions.
	if job.Type == "" || schedule.Param(job, schedule.ParamLogin) == "" {
		t.Fatal("pausing lost the definition")
	}

	if err := r.DeleteJob(ctx, admin, "github-review-queue"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, found, _ := r.jobs.Job(ctx, "github-review-queue"); found {
		t.Fatal("the job survived deletion")
	}
	// A Home tab published before somebody deleted it.
	if err := r.DeleteJob(ctx, admin, "github-review-queue"); err == nil {
		t.Fatal("deleting twice reported success")
	}
}

// A control that shows nothing for two minutes reads as one that did nothing.
func TestRunJobRedrawsBeforeAndAfter(t *testing.T) {
	r := newJobRig(t, sampleJob())
	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	before := r.views.count()

	if err := r.RunJob(context.Background(), admin, "github-review-queue"); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if len(r.runner.ran) != 1 {
		t.Fatalf("ran %v", r.runner.ran)
	}
	if r.views.count() != before+2 {
		t.Fatalf("published %d times, want one redraw before and one after", r.views.count()-before)
	}
}

// The failure is already on the row, which names the job and the reason.
func TestAFailedRunIsMarkedAsAlreadyReported(t *testing.T) {
	r := newJobRig(t, sampleJob())
	r.runner.err = errors.New("exit status 1")

	err := r.RunJob(context.Background(), admin, "github-review-queue")
	if err == nil {
		t.Fatal("a failing run reported success")
	}
	if !strings.Contains(err.Error(), "github-review-queue") {
		t.Fatalf("err = %v, want it to name the job", err)
	}
}

// An action_id and a block_id are just strings in a payload, and these ones
// write to the ledger and start processes.
func TestEveryJobControlIsAdminOnly(t *testing.T) {
	r := newJobRig(t, sampleJob())
	ctx, someone := context.Background(), "U-someone"

	for name, call := range map[string]func() error{
		"new github":  func() error { return r.ConfigureGitHubJob(ctx, someone, "t") },
		"new jira":    func() error { return r.NewJiraJob(ctx, someone, "t") },
		"edit":        func() error { return r.EditJob(ctx, someone, "github-review-queue", "t") },
		"save github": func() error { return r.SaveGitHubJob(ctx, someone, "miere", "3m", true) },
		"save jira":   func() error { return r.SaveJiraJob(ctx, someone, "", "x", "project = NYX", "3m") },
		"toggle":      func() error { return r.ToggleJob(ctx, someone, "github-review-queue") },
		"delete":      func() error { return r.DeleteJob(ctx, someone, "github-review-queue") },
		"run":         func() error { return r.RunJob(ctx, someone, "github-review-queue") },
	} {
		if err := call(); err == nil {
			t.Errorf("a non-admin could %s", name)
		}
	}
	if job, found, _ := r.jobs.Job(ctx, "github-review-queue"); !found || !job.Enabled {
		t.Fatal("a non-admin changed the schedule")
	}
	if len(r.runner.ran) != 0 {
		t.Fatalf("a non-admin ran %v", r.runner.ran)
	}
}

// A non-admin sees the portrait and the version, and nothing that operates
// Riggs.
func TestANonAdminSeesNoJobs(t *testing.T) {
	r := newJobRig(t, sampleJob())
	if _, err := r.Publish(context.Background(), "U-someone"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, found := r.views.last().jobRow("github-review-queue"); found {
		t.Fatal("a non-admin was shown the schedule")
	}
}

// A ledger blip should cost the admin the Jobs section for one publish, not
// replace their Home tab with an error.
func TestAnUnreadableLedgerDoesNotBreakTheTab(t *testing.T) {
	r := newJobRig(t, sampleJob())
	r.jobs.err = errors.New("database is locked")

	if _, err := r.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, found := r.views.last().jobRow("github-review-queue"); found {
		t.Fatal("a row was drawn from a failed read")
	}
}

// Clicking Delete asks; it does not delete.
//
// The click and the deletion are two round trips now, and the job has to still
// be there after the first one — a "confirmation" that has already acted is
// just a receipt.
func TestClickingDeleteOnlyAsks(t *testing.T) {
	r := newJobRig(t, sampleJob())
	ctx := context.Background()

	if err := r.ConfirmDeleteJob(ctx, admin, "github-review-queue", "trigger-1"); err != nil {
		t.Fatalf("ConfirmDeleteJob: %v", err)
	}
	if _, found, _ := r.jobs.Job(ctx, "github-review-queue"); !found {
		t.Fatal("the click deleted the job instead of asking about it")
	}

	if r.modals.triggerID != "trigger-1" {
		t.Errorf("trigger id = %q, want the one the click carried", r.modals.triggerID)
	}
	if got := r.modals.view["callback_id"]; got != blockkit.JobDeleteModalCallbackID {
		t.Errorf("callback_id = %v, want the delete confirmation", got)
	}
	if got := r.modals.view["private_metadata"]; got != "github-review-queue" {
		t.Errorf("private_metadata = %v, want the job the row was about", got)
	}

	// And the submission that follows is what actually forgets it.
	if err := r.DeleteJob(ctx, admin, "github-review-queue"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	if _, found, _ := r.jobs.Job(ctx, "github-review-queue"); found {
		t.Fatal("the confirmed delete did not take")
	}
}

// The modal is not a permission slip. A submission arrives as its own inbound
// message, so it is authorised on its own terms.
func TestANonAdminCannotOpenOrSubmitTheDeleteModal(t *testing.T) {
	r := newJobRig(t, sampleJob())
	ctx, someone := context.Background(), "U-someone"

	if err := r.ConfirmDeleteJob(ctx, someone, "github-review-queue", "t"); err == nil {
		t.Error("a non-admin opened the delete confirmation")
	}
	if err := r.DeleteJob(ctx, someone, "github-review-queue"); err == nil {
		t.Error("a non-admin submitted a delete")
	}
	if _, found, _ := r.jobs.Job(ctx, "github-review-queue"); !found {
		t.Fatal("a non-admin deleted the job")
	}
}

// The form's private_metadata is not trusted, because a modal can sit on
// somebody's screen for a long time. A digest created from a terminal while the
// form was open would otherwise be joined by a SECOND one under Riggs' own
// name, and the two would race to write the same message.
func TestSaveGitHubJobActsOnWhatExistsNow(t *testing.T) {
	r := newJobRig(t)
	ctx := context.Background()

	// Opened when there was nothing, so the submission carries no identity.
	// Between the two, somebody adds one from the CLI under their own name.
	adopted := sampleJob()
	adopted.Name = "quick-review-poll"
	if err := r.jobs.SaveJob(ctx, adopted); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}

	if err := r.SaveGitHubJob(ctx, admin, "someone-else", "9m", true); err != nil {
		t.Fatalf("SaveGitHubJob: %v", err)
	}
	jobs, _ := r.jobs.Jobs(ctx)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want the one digest updated rather than a second created", jobs)
	}
	if jobs[0].Name != "quick-review-poll" {
		t.Fatalf("name = %q, want the existing job's own name kept", jobs[0].Name)
	}
	if schedule.Param(jobs[0], schedule.ParamLogin) != "someone-else" || jobs[0].Spec != "9m" {
		t.Fatalf("the edit did not take: %+v", jobs[0])
	}

	// And the same for the destructive half: unticking deletes whatever exists
	// now, not whatever the form remembered.
	if err := r.SaveGitHubJob(ctx, admin, "", "", false); err != nil {
		t.Fatalf("SaveGitHubJob: %v", err)
	}
	if jobs, _ := r.jobs.Jobs(ctx); len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want the digest deleted", jobs)
	}
}
