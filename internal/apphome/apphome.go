// Package apphome renders and publishes Riggs' App Home tab, and acts on the
// one control it carries.
//
// The Home tab is the only surface Riggs has that is about Riggs rather than
// about somebody's pull requests. It answers two questions — what is this, and
// what version is answering my clicks — and, for the admin, offers the one
// action that follows from the second.
//
// The audience split is a hard rule, not a nicety. Everyone sees the portrait
// and the version. Everything from the divider onwards — the release notes and
// the Update button — is the admin's alone, and a non-admin is not shown a
// disabled version of it: a control you cannot use is worse than one that was
// never there, because it invites a click and then explains why it will not
// work.
package apphome

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slackmd"
	"github.com/miere/riggs-mcp/internal/updates"
)

// Checker is the release lookup the panel needs. It is an interface so the
// publisher can be tested without a Checker's cache or its network.
type Checker interface {
	Check(ctx context.Context) (updates.Release, error)
	Invalidate()
}

// Installer swaps the running binary for a release.
type Installer interface {
	Install(ctx context.Context, tag string) (updates.Result, error)
}

// Deps is the explicit dependency bundle passed to New.
type Deps struct {
	// Version is the running build, as version.String() reported it.
	Version string
	// BotToken authenticates views.publish. The Home tab belongs to a (user,
	// app) pair and this token names the app.
	BotToken string
	// AdminUserID is the one human who sees past the divider. Empty means
	// nobody does, which is the right answer for a config that never named an
	// admin: the panel degrades to a version line rather than offering a
	// binary swap to the whole workspace.
	AdminUserID string
	// Views publishes the rendered view.
	Views slack.ViewPublisher
	// Notify posts the outcome of an update. Optional; without it an update
	// still runs and is still logged, it is just not narrated.
	Notify slack.Poster
	// Checker reports what is available. Nil renders the version alone.
	Checker Checker
	// Installer performs the swap. Nil renders the panel without a button,
	// because a button that cannot do anything should not be drawn.
	Installer Installer
	// Restart brings the daemon back on the new binary. Nil means the admin is
	// told to restart it themselves.
	Restart func(ctx context.Context) error
	// Logger is where failures go. Nil selects slog.Default.
	Logger *slog.Logger
}

// Publisher renders and publishes the Home tab.
type Publisher struct {
	deps Deps

	// mu guards published, which records the last view each user was actually
	// sent.
	mu        sync.Mutex
	published map[string]string
}

// New constructs a Publisher.
func New(deps Deps) *Publisher {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Publisher{deps: deps, published: map[string]string{}}
}

// IsAdmin reports whether userID is the configured admin.
//
// An unset admin matches nobody. That is deliberately not "matches everybody":
// the alternative reading of an empty setting would hand a binary swap to
// anyone who opened the app.
func (p *Publisher) IsAdmin(userID string) bool {
	admin := strings.TrimSpace(p.deps.AdminUserID)
	return admin != "" && admin == strings.TrimSpace(userID)
}

// Publish renders the Home tab for userID and publishes it, reporting whether
// a Slack call was actually made.
//
// A view identical to the one this user was last sent is skipped.
// app_home_opened fires every time somebody so much as glances at the app, and
// republishing an unchanged view is a Slack call bought for nothing. The
// caller is told which happened so its log can say so — "published" on a call
// that was skipped is worse than no line at all.
func (p *Publisher) Publish(ctx context.Context, userID string) (bool, error) {
	if p.deps.Views == nil {
		return false, fmt.Errorf("apphome: no view publisher configured")
	}
	home := p.render(ctx, userID)
	fingerprint := home.Fingerprint()

	p.mu.Lock()
	unchanged := p.published[userID] == fingerprint
	p.mu.Unlock()
	if unchanged {
		return false, nil
	}

	if err := p.deps.Views.PublishHome(ctx, p.deps.BotToken, userID, home.View()); err != nil {
		return false, err
	}
	p.mu.Lock()
	p.published[userID] = fingerprint
	p.mu.Unlock()
	return true, nil
}

// render builds the view for one viewer.
//
// The release lookup runs for the admin only. It is not merely wasted work for
// everybody else — it is a GitHub request per curious colleague, against a
// 60-an-hour unauthenticated quota, for a section they will never be shown.
func (p *Publisher) render(ctx context.Context, userID string) blockkit.Home {
	admin := p.IsAdmin(userID)
	// The controls menu needs nothing but the gate: there is always something
	// to restart. The update section needs a release AND something able to
	// install it.
	home := blockkit.Home{Version: p.deps.Version, Admin: admin && p.deps.Restart != nil}
	if !admin || p.deps.Checker == nil || p.deps.Installer == nil {
		return home
	}

	rel, err := p.deps.Checker.Check(ctx)
	if err != nil {
		// Logged, not surfaced. A GitHub blip should cost the admin the update
		// section for an hour, not replace their Home tab with an error.
		p.deps.Logger.Debug("app home update check failed", "error", err)
	}
	if !rel.Available {
		return home
	}
	home.Update = &blockkit.HomeUpdate{Tag: rel.Tag, Notes: ReleaseNotes(rel)}
	return home
}

// ReleaseNotes converts a release body from GitHub-flavoured Markdown into what
// Slack will actually render, and appends the numbered list of links the
// conversion had to suppress.
//
// The footnotes are wanted HERE and deliberately not on a digest row: release
// notes are read deliberately, and a note whose every "see #123" has been
// flattened to "see [4]" with no list to resolve it against has lost the thing
// it was pointing at. A two-line card excerpt is read at a glance and would be
// buried by the same list.
//
// The release's own page is appended last, because the one link a reader
// reliably wants is the one to the release itself, and it is never in the body.
func ReleaseNotes(rel updates.Release) string {
	notes := slackmd.Convert(rel.Notes).WithFootnotes()
	if url := strings.TrimSpace(rel.URL); url != "" {
		if notes != "" {
			notes += "\n"
		}
		notes += "\n" + url
	}
	return strings.TrimSpace(notes)
}

// Update installs the available release and restarts onto it.
//
// It re-checks the admin gate. The button is only ever rendered for the admin,
// but an action_id and a value are just strings in a payload, and this one
// replaces a binary.
//
// The tag is resolved here rather than carried on the button. A Home tab
// published on Tuesday and clicked on Friday would otherwise install Tuesday's
// idea of the latest release — and a value that varied per release could not be
// matched by the daemon's routing table in the first place.
func (p *Publisher) Update(ctx context.Context, userID string) error {
	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied an app home update from a non-admin", "user", userID)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Installer == nil {
		return fmt.Errorf("apphome: no installer configured")
	}

	result, err := p.deps.Installer.Install(ctx, "")
	if err != nil {
		p.deps.Logger.Error("app home update failed", "user", userID, "error", err)
		p.say(ctx, fmt.Sprintf("%s Update failed: %v", blockkit.MarkerFailed, err))
		// The panel is redrawn either way: the button still applies, and a
		// Home tab frozen mid-click reads as a button that did nothing.
		p.republish(ctx, userID)
		// Marked: p.say has already put this in front of the admin, and the
		// daemon would otherwise DM them the same news a second time.
		return slack.Reported(err)
	}
	p.deps.Logger.Info("app home update installed",
		"user", userID, "tag", result.Tag, "path", result.Path, "backup", result.BackupPath)

	// The cache still holds "this release is available", and it is now
	// installed. Dropping it here means the panel the restarted daemon
	// publishes is drawn from a fresh answer.
	if p.deps.Checker != nil {
		p.deps.Checker.Invalidate()
	}

	if p.deps.Restart == nil {
		p.say(ctx, fmt.Sprintf("%s Installed %s. Restart Riggs to run it.", blockkit.MarkerDone, result.Tag))
		p.republish(ctx, userID)
		return nil
	}
	p.say(ctx, fmt.Sprintf("%s Updated to %s — restarting now.", blockkit.MarkerDone, result.Tag))
	if err := p.deps.Restart(ctx); err != nil {
		p.deps.Logger.Error("app home update installed but the restart failed", "tag", result.Tag, "error", err)
		p.say(ctx, fmt.Sprintf("%s %s is installed, but the restart failed: %v. Restart Riggs to run it.",
			blockkit.MarkerWarning, result.Tag, err))
		p.republish(ctx, userID)
		return slack.Reported(err)
	}
	// No republish on the success path: this process is on its way out, and the
	// view it would publish is the OLD version's. The restarted daemon
	// publishes the new one the next time the tab is opened.
	return nil
}

// Restart brings the daemon back on the binary it is already running.
//
// Same admin re-check as Update, for the same reason: the menu is only ever
// rendered for the admin, but an action_id and a value are just strings in a
// payload, and this one takes Riggs off Slack for as long as its supervisor
// needs to bring it back.
//
// Unlike Update there is nothing to install and nothing to verify first, so the
// only failure worth reporting is launchd declining — which means the daemon is
// NOT coming back on its own and the admin has to be told, rather than left
// watching a tab that will never change.
func (p *Publisher) Restart(ctx context.Context, userID string) error {
	if !p.IsAdmin(userID) {
		p.deps.Logger.Warn("denied an app home restart from a non-admin", "user", userID)
		return fmt.Errorf("apphome: %s is not the admin", userID)
	}
	if p.deps.Restart == nil {
		p.deps.Logger.Warn("app home restart requested but none is configured", "user", userID)
		p.say(ctx, fmt.Sprintf("%s Riggs has no supervisor configured, so it cannot restart itself.",
			blockkit.MarkerWarning))
		return slack.Reported(fmt.Errorf("apphome: no restart configured"))
	}

	// Said BEFORE the restart, not after: after, this process is gone.
	p.say(ctx, fmt.Sprintf("%s Restarting Riggs.", blockkit.MarkerRunning))
	p.deps.Logger.Info("app home restart requested", "user", userID)

	if err := p.deps.Restart(ctx); err != nil {
		p.deps.Logger.Error("app home restart failed", "user", userID, "error", err)
		p.say(ctx, fmt.Sprintf("%s Restart failed: %v. Riggs is still running the old process.",
			blockkit.MarkerFailed, err))
		return slack.Reported(err)
	}
	return nil
}

// republish redraws the Home tab, forcing a publish even when the rendered view
// has not changed — after an update the admin is looking at the tab, and "your
// click did nothing" is the wrong impression to leave.
func (p *Publisher) republish(ctx context.Context, userID string) {
	p.mu.Lock()
	delete(p.published, userID)
	p.mu.Unlock()
	if _, err := p.Publish(ctx, userID); err != nil {
		p.deps.Logger.Error("republishing the app home failed", "user", userID, "error", err)
	}
}

// say DMs the admin. An update's outcome cannot be reported on the Home tab
// itself — the interesting half of it lands after this process has gone — so it
// goes somewhere that persists.
func (p *Publisher) say(ctx context.Context, text string) {
	if p.deps.Notify == nil || strings.TrimSpace(p.deps.AdminUserID) == "" {
		return
	}
	target := slack.Target{BotToken: p.deps.BotToken, AdminUserID: p.deps.AdminUserID}
	if _, err := p.deps.Notify.Post(ctx, target, slack.Message{
		Text:   text,
		Blocks: blockkit.ContextBlocks(text),
	}); err != nil {
		p.deps.Logger.Error("could not report the update outcome", "error", err)
	}
}
