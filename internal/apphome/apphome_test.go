package apphome

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/updates"
)

const admin = "U0ADMIN"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- fakes ------------------------------------------------------------------

type fakeViews struct {
	mu        sync.Mutex
	published []publishedView
}

type publishedView struct {
	user string
	view map[string]any
}

func (f *fakeViews) PublishHome(_ context.Context, _, userID string, view any) error {
	raw, err := json.Marshal(view)
	if err != nil {
		return err
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, publishedView{user: userID, view: decoded})
	return nil
}

func (f *fakeViews) last() publishedView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published[len(f.published)-1]
}

func (f *fakeViews) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

// hasControlsMenu reports whether the version line carries the overflow menu.
func (p publishedView) hasControlsMenu() bool {
	blocks, _ := p.view["blocks"].([]any)
	if len(blocks) < 2 {
		return false
	}
	_, found := blocks[1].(map[string]any)["accessory"]
	return found
}

// blockTypes reports the rendered surface's block types, which is the only
// thing these tests need from the view.
func (p publishedView) blockTypes() []string {
	blocks, _ := p.view["blocks"].([]any)
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.(map[string]any)["type"].(string))
	}
	return out
}

type fakeChecker struct {
	rel         updates.Release
	err         error
	calls       int
	invalidated int
}

func (f *fakeChecker) Check(context.Context) (updates.Release, error) {
	f.calls++
	return f.rel, f.err
}
func (f *fakeChecker) Invalidate() { f.invalidated++ }

type fakeInstaller struct {
	result updates.Result
	err    error
	tags   []string
}

func (f *fakeInstaller) Install(_ context.Context, tag string) (updates.Result, error) {
	f.tags = append(f.tags, tag)
	return f.result, f.err
}

type fakePoster struct{ posted []string }

func (f *fakePoster) Post(_ context.Context, _ slack.Target, msg slack.Message) (slack.Ref, error) {
	f.posted = append(f.posted, msg.Text)
	return slack.Ref{}, nil
}
func (f *fakePoster) Update(context.Context, slack.Target, slack.Ref, slack.Message) error {
	return nil
}
func (f *fakePoster) Delete(context.Context, slack.Target, slack.Ref) error { return nil }

// --- helpers ----------------------------------------------------------------

func publisher(t *testing.T, mutate func(*Deps)) (*Publisher, *fakeViews, *fakeChecker, *fakeInstaller, *fakePoster) {
	t.Helper()
	views, checker := &fakeViews{}, &fakeChecker{
		rel: updates.Release{Current: "v0.1.0", Tag: "v0.2.0", Notes: "## Fixes\n- a thing", Available: true},
	}
	installer, poster := &fakeInstaller{result: updates.Result{Tag: "v0.2.0", Path: "/usr/local/bin/riggs"}}, &fakePoster{}
	deps := Deps{
		Version: "v0.1.0", BotToken: "xoxb-test", AdminUserID: admin,
		Views: views, Notify: poster, Checker: checker, Installer: installer,
		Restart: func(context.Context) error { return nil },
		Logger:  quiet(),
	}
	if mutate != nil {
		mutate(&deps)
	}
	return New(deps), views, checker, installer, poster
}

// --- the audience split -----------------------------------------------------

func TestTheAdminSeesPastTheDivider(t *testing.T) {
	p, views, _, _, _ := publisher(t, nil)
	if _, err := p.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := strings.Join(views.last().blockTypes(), ",")
	if got != "image,section,divider,header,section" {
		t.Fatalf("admin view = %s", got)
	}
}

// The rule this package exists to enforce: everyone can look at Riggs, nobody
// but the admin is offered the machinery. Not a disabled button — an absent
// one.
func TestANonAdminNeverSeesTheUpdateSection(t *testing.T) {
	p, views, checker, _, _ := publisher(t, nil)
	if _, err := p.Publish(context.Background(), "U0SOMEONE"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := strings.Join(views.last().blockTypes(), ","); got != "image,section" {
		t.Fatalf("non-admin view = %s", got)
	}
	// And they cost nothing: no GitHub lookup runs for a section they will
	// never be shown.
	if checker.calls != 0 {
		t.Fatalf("checked GitHub %d times for a non-admin", checker.calls)
	}
}

// An unset admin must match nobody. The alternative reading of an empty setting
// hands a binary swap to the whole workspace.
func TestAnUnsetAdminMatchesNobody(t *testing.T) {
	p, views, _, _, _ := publisher(t, func(d *Deps) { d.AdminUserID = "" })
	if p.IsAdmin("") || p.IsAdmin(admin) {
		t.Fatal("an unset admin matched someone")
	}
	p.Publish(context.Background(), admin)
	if got := strings.Join(views.last().blockTypes(), ","); got != "image,section" {
		t.Fatalf("view = %s, want no update section", got)
	}
}

func TestNoUpdateAvailableStopsAtTheVersion(t *testing.T) {
	p, views, _, _, _ := publisher(t, nil)
	p.deps.Checker.(*fakeChecker).rel = updates.Release{Current: "v0.2.0", Tag: "v0.2.0"}

	p.Publish(context.Background(), admin)
	if got := strings.Join(views.last().blockTypes(), ","); got != "image,section" {
		t.Fatalf("view = %s", got)
	}
}

// A GitHub blip costs the admin the update section, not their Home tab.
func TestAFailedCheckStillPublishesTheVersion(t *testing.T) {
	p, views, checker, _, _ := publisher(t, nil)
	checker.rel, checker.err = updates.Release{Current: "v0.1.0"}, errors.New("no network")

	if _, err := p.Publish(context.Background(), admin); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := strings.Join(views.last().blockTypes(), ","); got != "image,section" {
		t.Fatalf("view = %s", got)
	}
}

// app_home_opened fires on every glance. An unchanged view is not worth a Slack
// call.
func TestAnUnchangedViewIsNotRepublished(t *testing.T) {
	p, views, _, _, _ := publisher(t, nil)
	for range 4 {
		p.Publish(context.Background(), admin)
	}
	if views.count() != 1 {
		t.Fatalf("published %d times, want 1", views.count())
	}
}

func TestAChangedViewIsRepublished(t *testing.T) {
	p, views, checker, _, _ := publisher(t, nil)
	p.Publish(context.Background(), admin)
	checker.rel.Tag = "v0.3.0"
	p.Publish(context.Background(), admin)

	if views.count() != 2 {
		t.Fatalf("published %d times, want 2", views.count())
	}
}

// --- the controls menu ------------------------------------------------------

func TestOnlyTheAdminGetsTheControlsMenu(t *testing.T) {
	p, views, _, _, _ := publisher(t, nil)

	p.Publish(context.Background(), admin)
	if !views.last().hasControlsMenu() {
		t.Error("the admin was shown no controls menu")
	}
	p.Publish(context.Background(), "U0SOMEONE")
	if views.last().hasControlsMenu() {
		t.Error("a non-admin was shown the controls menu")
	}
}

// Drawing a Restart option on a Riggs that has no way to restart itself is the
// same mistake as drawing a disabled button: it invites a click and then
// explains why it will not work.
func TestWithNoSupervisorTheMenuIsNotDrawn(t *testing.T) {
	p, views, _, _, _ := publisher(t, func(d *Deps) { d.Restart = nil })

	p.Publish(context.Background(), admin)
	if views.last().hasControlsMenu() {
		t.Error("the menu was drawn with no supervisor to restart through")
	}
}

// Restarting is not about a release. It is offered whether or not there is
// anything to install.
func TestTheMenuSurvivesWithNothingToInstall(t *testing.T) {
	p, views, checker, _, _ := publisher(t, nil)
	checker.rel = updates.Release{Current: "v0.2.0", Tag: "v0.2.0"}

	p.Publish(context.Background(), admin)
	if !views.last().hasControlsMenu() {
		t.Error("the menu vanished when Riggs was up to date")
	}
}

func TestRestartAsksTheSupervisor(t *testing.T) {
	restarted := 0
	p, _, _, installer, poster := publisher(t, func(d *Deps) {
		d.Restart = func(context.Context) error { restarted++; return nil }
	})

	if err := p.Restart(context.Background(), admin); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if restarted != 1 {
		t.Fatalf("restarted %d times, want 1", restarted)
	}
	// Nothing is installed on the way past: this is a restart, not an update.
	if len(installer.tags) != 0 {
		t.Fatalf("the installer ran: %v", installer.tags)
	}
	// And the admin is told BEFORE the process goes, since afterwards there is
	// nobody left to say it.
	if len(poster.posted) != 1 || !strings.Contains(poster.posted[0], "Restarting") {
		t.Fatalf("the restart was not announced: %v", poster.posted)
	}
}

// The menu is only ever rendered for the admin, but an action_id and a value
// are just strings in a payload — and this one takes Riggs off Slack.
func TestRestartRefusesANonAdmin(t *testing.T) {
	restarted := 0
	p, _, _, _, _ := publisher(t, func(d *Deps) {
		d.Restart = func(context.Context) error { restarted++; return nil }
	})

	if err := p.Restart(context.Background(), "U0SOMEONE"); err == nil {
		t.Fatal("a non-admin restarted Riggs")
	}
	if restarted != 0 {
		t.Fatal("the supervisor was asked anyway")
	}
}

// launchd declining means the daemon is NOT coming back on its own. Saying so
// is the difference between a slow restart and a dead Riggs nobody noticed.
func TestARefusedRestartIsReported(t *testing.T) {
	p, _, _, _, poster := publisher(t, func(d *Deps) {
		d.Restart = func(context.Context) error { return errors.New("launchd does not know this agent") }
	})

	if err := p.Restart(context.Background(), admin); err == nil {
		t.Fatal("Restart swallowed the supervisor's refusal")
	}
	joined := strings.Join(poster.posted, "\n")
	if !strings.Contains(joined, "still running the old process") {
		t.Fatalf("the failure was not explained: %v", poster.posted)
	}
}

// --- release notes ----------------------------------------------------------

// The Home tab carries GitHub's release body, and GitHub's Markdown is not
// Slack's. The conversion is slackmd's, reused rather than re-implemented; what
// this asserts is that it is applied at all, and that the footnote list comes
// with it.
func TestReleaseNotesAreConvertedToSlackMarkdown(t *testing.T) {
	got := ReleaseNotes(updates.Release{
		Tag: "v0.2.0",
		Notes: "## What's new\n" +
			"- **bulk cards**, see [the PR](https://github.com/miere/riggs/pull/7)\n" +
			"- a fix in `main`\n\n" +
			"```kotlin\nval x = 1\n```",
		URL: "https://github.com/miere/riggs/releases/tag/v0.2.0",
	})

	// A heading has no equivalent in Slack, so it becomes a bold line.
	if !strings.Contains(got, "*What's new*") || strings.Contains(got, "##") {
		t.Errorf("heading was not flattened to bold:\n%s", got)
	}
	// GitHub's ** is Slack's *. Copying them across turns bold into italic.
	if !strings.Contains(got, "*bulk cards*") || strings.Contains(got, "**") {
		t.Errorf("bold was not translated:\n%s", got)
	}
	// A link becomes `text [N]`, and the list that resolves N comes with it.
	if !strings.Contains(got, "the PR [1]") || !strings.Contains(got, "[1] https://github.com/miere/riggs/pull/7") {
		t.Errorf("link was not footnoted:\n%s", got)
	}
	// Slack prints a fence's language tag as a literal word.
	if strings.Contains(got, "```kotlin") {
		t.Errorf("code fence kept its language tag:\n%s", got)
	}
	// The one link a reader always wants is never in the body.
	if !strings.Contains(got, "https://github.com/miere/riggs/releases/tag/v0.2.0") {
		t.Errorf("the release's own page is missing:\n%s", got)
	}
}

// --- the update ------------------------------------------------------------

func TestUpdateInstallsAndRestarts(t *testing.T) {
	restarted := 0
	p, _, checker, installer, poster := publisher(t, func(d *Deps) {
		d.Restart = func(context.Context) error { restarted++; return nil }
	})

	if err := p.Update(context.Background(), admin); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// The tag is resolved at click time, not carried on the button: a Home tab
	// published days ago must not pin an install to a stale release.
	if len(installer.tags) != 1 || installer.tags[0] != "" {
		t.Fatalf("installed tags = %v, want one request for the latest", installer.tags)
	}
	if restarted != 1 {
		t.Fatalf("restarted %d times, want 1", restarted)
	}
	// The cache still says "v0.2.0 is available", and it now IS the running
	// version. Left alone, the restarted daemon would offer it again.
	if checker.invalidated != 1 {
		t.Fatalf("invalidated %d times, want 1", checker.invalidated)
	}
	if len(poster.posted) != 1 || !strings.Contains(poster.posted[0], "v0.2.0") {
		t.Fatalf("outcome not reported: %v", poster.posted)
	}
}

// The button is only ever drawn for the admin, but an action_id and a value are
// just strings in a payload — and this one replaces a binary.
func TestUpdateRefusesANonAdmin(t *testing.T) {
	p, _, _, installer, _ := publisher(t, nil)
	if err := p.Update(context.Background(), "U0SOMEONE"); err == nil {
		t.Fatal("a non-admin was allowed to update the binary")
	}
	if len(installer.tags) != 0 {
		t.Fatalf("the installer ran anyway: %v", installer.tags)
	}
}

// A failed install must not restart onto a binary that was never replaced, and
// must say so where the admin will see it.
func TestAFailedInstallDoesNotRestart(t *testing.T) {
	restarted := 0
	p, _, _, installer, poster := publisher(t, func(d *Deps) {
		d.Restart = func(context.Context) error { restarted++; return nil }
	})
	installer.err = errors.New("verify failed")

	if err := p.Update(context.Background(), admin); err == nil {
		t.Fatal("Update reported success after a failed install")
	}
	if restarted != 0 {
		t.Fatalf("restarted after a failed install")
	}
	if len(poster.posted) != 1 || !strings.Contains(poster.posted[0], blockkit.MarkerFailed) {
		t.Fatalf("failure not reported: %v", poster.posted)
	}
}

// The Home tab says its own failures, so the daemon must not say them again.
// An admin who clicked Update once should be told once.
func TestHomeTabFailuresAreMarkedAsAlreadyReported(t *testing.T) {
	t.Run("a failed install", func(t *testing.T) {
		p, _, _, installer, _ := publisher(t, nil)
		installer.err = errors.New("no asset for darwin/arm64")
		err := p.Update(context.Background(), admin)
		if err == nil {
			t.Fatal("Update reported success after a failed install")
		}
		if !slack.WasReported(err) {
			t.Error("the daemon would report this a second time")
		}
	})

	t.Run("a failed restart", func(t *testing.T) {
		p, _, _, _, _ := publisher(t, func(d *Deps) {
			d.Restart = func(context.Context) error { return errors.New("launchd does not know this agent") }
		})
		err := p.Restart(context.Background(), admin)
		if err == nil {
			t.Fatal("Restart reported success after a failed restart")
		}
		if !slack.WasReported(err) {
			t.Error("the daemon would report this a second time")
		}
	})

	// A refusal says nothing to the user, so it must NOT claim to have been
	// reported — this is the case the daemon's catch-all exists for.
	t.Run("a non-admin refusal", func(t *testing.T) {
		p, _, _, _, poster := publisher(t, nil)
		err := p.Update(context.Background(), "U-stranger")
		if err == nil {
			t.Fatal("Update accepted a non-admin")
		}
		if len(poster.posted) != 0 {
			t.Fatalf("a refusal said something: %v", poster.posted)
		}
		if slack.WasReported(err) {
			t.Error("a silent refusal claimed to have been reported")
		}
	})
}

// Installed but not restarted is a real state — a Riggs started by hand has no
// supervisor to bring it back — and the admin has to be told, or they are left
// looking at a version line that will not change.
func TestAFailedRestartIsReported(t *testing.T) {
	p, _, _, _, poster := publisher(t, func(d *Deps) {
		d.Restart = func(context.Context) error { return errors.New("launchd does not know this agent") }
	})

	if err := p.Update(context.Background(), admin); err == nil {
		t.Fatal("Update swallowed a failed restart")
	}
	joined := strings.Join(poster.posted, "\n")
	if !strings.Contains(joined, "Restart Riggs to run it") {
		t.Fatalf("the admin was not told to restart it: %v", poster.posted)
	}
}

func TestWithNoRestartConfiguredTheAdminIsToldToDoIt(t *testing.T) {
	p, _, _, _, poster := publisher(t, func(d *Deps) { d.Restart = nil })

	if err := p.Update(context.Background(), admin); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(poster.posted) != 1 || !strings.Contains(poster.posted[0], "Restart Riggs") {
		t.Fatalf("outcome = %v", poster.posted)
	}
}

// The skip has to be visible to the caller. Without it the daemon logged "app
// home published" on every glance at the app — including the ones it had
// deliberately skipped — which is a log line that lies about a network call.
func TestPublishReportsWhetherItActuallyPublished(t *testing.T) {
	p, _, _, _, _ := publisher(t, nil)

	published, err := p.Publish(context.Background(), admin)
	if err != nil || !published {
		t.Fatalf("first publish = %v, %v; want a real publish", published, err)
	}
	published, err = p.Publish(context.Background(), admin)
	if err != nil || published {
		t.Fatalf("second publish = %v, %v; want a skip", published, err)
	}
}
