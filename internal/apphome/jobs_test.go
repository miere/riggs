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
		Name: "github-review-queue", Args: []string{"git", "pr", "--bulk", "miere"},
		Spec: "3m", Timeout: 2 * time.Minute, Enabled: true,
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

func TestNewJobOpensAnEmptyEditor(t *testing.T) {
	r := newJobRig(t)
	if err := r.NewJob(context.Background(), admin, "trigger-1"); err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	if r.modals.view["callback_id"] != blockkit.JobModalCallbackID {
		t.Fatalf("callback = %v", r.modals.view["callback_id"])
	}
	// Sensible starting points rather than an empty form: both are what the
	// adopted jobs actually use.
	blocks := r.modals.view["blocks"].([]any)
	var schedule string
	for _, b := range blocks {
		block := b.(map[string]any)
		if block["block_id"] == blockkit.JobModalScheduleBlockID {
			schedule, _ = block["element"].(map[string]any)["initial_value"].(string)
		}
	}
	if schedule != "3m" {
		t.Fatalf("schedule pre-fill = %q", schedule)
	}
}

func TestSaveJobCreatesAndRedraws(t *testing.T) {
	r := newJobRig(t)
	if err := r.SaveJob(context.Background(), admin, "", "nightly", "riggs jira tickets --bulk",
		"0 9 * * 1-5", "5m"); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	job, found, _ := r.jobs.Job(context.Background(), "nightly")
	if !found {
		t.Fatal("the job was not created")
	}
	// The leading `riggs` is dropped: the binary is not the operator's to choose.
	if schedule.Command(job) != "jira tickets --bulk" {
		t.Fatalf("command = %q", schedule.Command(job))
	}
	if job.Spec != "0 9 * * 1-5" || job.Timeout != 5*time.Minute {
		t.Fatalf("job = %+v", job)
	}
	if _, ok := r.views.last().jobRow("nightly"); !ok {
		t.Fatal("the tab was not redrawn with the new job")
	}
}

// A name already in use would silently replace somebody else's job, and the two
// would be indistinguishable afterwards.
func TestCreatingADuplicateIsRefused(t *testing.T) {
	r := newJobRig(t, sampleJob())
	err := r.SaveJob(context.Background(), admin, "", "github-review-queue", "git pr --bulk x", "3m", "")
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
	job := sampleJob()
	job.Enabled = false
	r := newJobRig(t, job)

	if err := r.SaveJob(context.Background(), admin, job.Name, "", "git pr --bulk miere", "5m", ""); err != nil {
		t.Fatalf("SaveJob: %v", err)
	}
	saved, _, _ := r.jobs.Job(context.Background(), job.Name)
	if saved.Enabled {
		t.Fatal("editing a paused job resumed it")
	}
	if saved.Spec != "5m" {
		t.Fatalf("the edit did not take: %q", saved.Spec)
	}
}

// An unreadable schedule must not reach the ledger, where the scheduler would
// log it every fifteen seconds and never run it.
func TestSaveJobRefusesAnUnreadableSchedule(t *testing.T) {
	r := newJobRig(t)
	err := r.SaveJob(context.Background(), admin, "", "nightly", "jira tickets --bulk", "weekly", "")
	if err == nil {
		t.Fatal("an unreadable schedule was saved")
	}
	if _, found, _ := r.jobs.Job(context.Background(), "nightly"); found {
		t.Fatal("the job was created anyway")
	}
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
	if len(job.Args) == 0 {
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
		"new":    func() error { return r.NewJob(ctx, someone, "t") },
		"edit":   func() error { return r.EditJob(ctx, someone, "github-review-queue", "t") },
		"save":   func() error { return r.SaveJob(ctx, someone, "", "x", "ping", "3m", "") },
		"toggle": func() error { return r.ToggleJob(ctx, someone, "github-review-queue") },
		"delete": func() error { return r.DeleteJob(ctx, someone, "github-review-queue") },
		"run":    func() error { return r.RunJob(ctx, someone, "github-review-queue") },
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
