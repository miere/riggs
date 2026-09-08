package app

import (
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/apphome"
	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/daemon"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/schedule"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/ticket"
)

// Every control Riggs renders that is meant to do something, and nothing else.
// "Open on Browser" is deliberately absent: Slack opens the link itself, and a
// handler that exists only to return nil would be worse than the router's own
// "no handler" log line.
func TestDaemonRegistersTheDigestActions(t *testing.T) {
	a := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	a.registerInteractions(router, slack.Credentials{Profile: "riggs"})

	got := router.Routes()
	// Sorted, as Routes() reports them.
	want := []string{
		// The ticket ask card's link button, declared as deliberately not acted
		// on rather than left out. An unregistered pair now means "Riggs does
		// not understand this" and earns a disregard reaction (§7f); a link
		// Slack opened itself is not that.
		ticket.AskActionID + "/",
		// The ticket digest rows' menu. "Assign to Me" is absent because it is
		// not rendered: the verb exists, the option deliberately does not.
		ticket.BulkActionID + "/" + ticket.IntentAskAssist,
		ticket.BulkActionID + "/" + ticket.IntentOpenBrowser,
		pullrequest.AskOpenActionID + "/",
		// The ask-review card's Approve, which leaves no comment.
		pullrequest.AskActionID + "/" + pullrequest.IntentApprove,
		// The pull-request digest rows' menu.
		pullrequest.BulkActionID + "/" + pullrequest.IntentApproveMerge,
		pullrequest.BulkActionID + "/" + pullrequest.IntentAskReview,
		pullrequest.BulkActionID + "/" + pullrequest.IntentOpenBrowser,
	}
	assertRoutes(t, got, want)
}

// The link buttons are REGISTERED, and registered as ignored. The distinction
// is invisible in Routes() and is the whole point of the entry: an ignored
// control gets no reaction at all, where an unrouted one gets a disregard.
func TestTheLinkButtonsAreIgnoredRatherThanUnrouted(t *testing.T) {
	a := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	a.registerInteractions(router, slack.Credentials{Profile: "riggs"})

	for _, in := range []slack.Interaction{
		{ActionID: pullrequest.BulkActionID, Intent: pullrequest.IntentOpenBrowser},
		{ActionID: ticket.BulkActionID, Intent: ticket.IntentOpenBrowser},
		{ActionID: pullrequest.AskOpenActionID},
		{ActionID: ticket.AskActionID},
	} {
		if got := router.Lookup(in); got != daemon.Ignored {
			t.Fatalf("Lookup(%s/%s) = %v, want Ignored", in.ActionID, in.Intent, got)
		}
	}
	// And something genuinely retired still reads as unrouted, so the two have
	// not been collapsed.
	if got := router.Lookup(slack.Interaction{ActionID: "pr_bulk_overflow", Intent: "retired"}); got != daemon.Unrouted {
		t.Fatalf("Lookup(retired) = %v, want Unrouted", got)
	}
}

func TestTargetForCarriesTheDaemonsCredentials(t *testing.T) {
	a := &Application{cfg: &config.Config{Admin: config.Admin{SlackUserID: "U-admin"}}}
	target := a.targetFor(
		slack.Credentials{Profile: "riggs", BotToken: "xoxb-riggs"},
		slack.Interaction{Channel: "C-digest"},
	)

	if target.BotToken != "xoxb-riggs" || target.Profile != "riggs" {
		t.Fatalf("target = %+v, want the daemon's own app", target)
	}
	if target.Channel != "C-digest" {
		t.Fatalf("target channel = %q, want the click's channel", target.Channel)
	}
	if target.AdminUserID != "U-admin" {
		t.Fatalf("target admin = %q", target.AdminUserID)
	}
}

func TestDaemonProfileParsing(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"absent":   {nil, ""},
		"spaced":   {[]string{"--slack-profile", "riggs"}, "riggs"},
		"appended": {[]string{"--slack-profile=riggs"}, "riggs"},
	}
	for name, tc := range cases {
		got, err := daemonProfile(tc.args)
		if err != nil {
			t.Fatalf("%s: daemonProfile: %v", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: daemonProfile = %q, want %q", name, got, tc.want)
		}
	}
}

// A mistyped flag must not silently start a daemon listening as the wrong app.
func TestDaemonProfileRejectsBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"missing value": {"--slack-profile"},
		"empty value":   {"--slack-profile="},
		"stray token":   {"riggs"},
		"unknown flag":  {"--slack-channel", "C1"},
	} {
		if got, err := daemonProfile(args); err == nil {
			t.Errorf("%s: daemonProfile returned %q, want an error", name, got)
		}
	}
}

// The two options that RUN are registered only when there is a harness to run.
// A route with no control can only ever answer a click on a digest posted
// before the harness was removed.
func TestTheRunRoutesFollowTheHarness(t *testing.T) {
	off := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	off.registerRunInteractions(router, slack.Credentials{}, quietLogger())
	if got := router.Routes(); len(got) != 0 {
		t.Fatalf("routes = %v, want none with no ai.command", got)
	}

	on := &Application{cfg: &config.Config{AI: config.AI{Command: "claude"}}}
	router = daemon.NewRouter()
	on.registerRunInteractions(router, slack.Credentials{}, quietLogger())

	want := []string{
		ticket.BulkActionID + "/" + ticket.IntentRunAssist,
		pullrequest.BulkActionID + "/" + pullrequest.IntentRunReview,
	}
	assertRoutes(t, router.Routes(), want)
}

// The Home tab's controls, including the prompt editor and the modal coming
// back. A submission is in the same table as a click because it is the same
// kind of thing: the callback_id is the control, the private_metadata the item.
func TestDaemonRegistersTheHomeControls(t *testing.T) {
	a := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	a.registerHomeInteractions(router, apphome.New(apphome.Deps{Logger: quietLogger()}))

	want := []string{
		blockkit.HomeMenuActionID + "/" + blockkit.HomeCustomiseIntent,
		blockkit.HomeMenuActionID + "/" + blockkit.HomeConfigureIntent,
		blockkit.HomeMenuActionID + "/" + blockkit.HomeRestartIntent,
		blockkit.HomePromptActionID + "/" + blockkit.HomePromptEditIntent,
		blockkit.HomePromptActionID + "/" + blockkit.HomePromptResetIntent,
		blockkit.CustomisationModalCallbackID + "/" + slack.ViewSubmitIntent,
		blockkit.ConfigurationModalCallbackID + "/" + slack.ViewSubmitIntent,
		blockkit.HomeUpdateActionID + "/" + blockkit.HomeUpdateIntent,
		blockkit.PromptModalCallbackID + "/" + slack.ViewSubmitIntent,
	}
	assertRoutes(t, router.Routes(), want)
}

// Which prompt a click is about rides in the row's block_id, namespaced so it
// is distinguishable from any other block that might carry an id.
func TestPromptIDStripsTheNamespace(t *testing.T) {
	if got := promptID(blockkit.HomePromptBlockPrefix + "ai_review"); got != "ai_review" {
		t.Fatalf("promptID = %q", got)
	}
	// A modal's private_metadata carries the bare id and must survive untouched.
	if got := promptID("ai_review"); got != "ai_review" {
		t.Fatalf("promptID = %q", got)
	}
}

// assertRoutes compares a router's registered pairs against an expected set.
func assertRoutes(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("routes = %v, want %v", got, want)
		}
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The Jobs section's controls, and the editor coming back. Which job a click is
// about rides in the row's block_id, exactly as a prompt and a pull request do.
func TestDaemonRegistersTheJobControls(t *testing.T) {
	a := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	a.registerJobInteractions(router, apphome.New(apphome.Deps{Logger: quietLogger()}))

	want := []string{
		blockkit.HomeJobActionID + "/" + blockkit.HomeJobDeleteIntent,
		blockkit.HomeJobActionID + "/" + blockkit.HomeJobEditIntent,
		blockkit.HomeJobActionID + "/" + blockkit.HomeJobRunIntent,
		blockkit.HomeJobActionID + "/" + blockkit.HomeJobToggleIntent,
		blockkit.HomeMenuActionID + "/" + blockkit.HomeGitHubJobIntent,
		blockkit.HomeMenuActionID + "/" + blockkit.HomeNewJiraJobIntent,
		// One editor per kind of job, so a submission carrying a checkbox that
		// deletes a digest can never be read as one carrying a query.
		blockkit.GitHubJobModalCallbackID + "/" + slack.ViewSubmitIntent,
		blockkit.JiraJobModalCallbackID + "/" + slack.ViewSubmitIntent,
		// Delete is two routes, not one: the click opens the confirmation and
		// this submission is what actually forgets the job.
		blockkit.JobDeleteModalCallbackID + "/" + slack.ViewSubmitIntent,
	}
	assertRoutes(t, router.Routes(), want)
}

// Every option on the controls menu shares one action_id, so they must not
// collide.
func TestTheControlsMenuRoutesEveryOneOfItsOptions(t *testing.T) {
	a := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	home := apphome.New(apphome.Deps{Logger: quietLogger()})
	// Registering the same (action_id, intent) twice panics at wiring time, so
	// this passing IS the assertion that they are distinct.
	a.registerHomeInteractions(router, home)
	a.registerJobInteractions(router, home)

	got := router.Routes()
	var menu int
	for _, route := range got {
		if strings.HasPrefix(route, blockkit.HomeMenuActionID+"/") {
			menu++
		}
	}
	if menu != 5 {
		t.Fatalf("controls-menu routes = %d, want Restart, Customisation, Configuration "+
			"and the two job editors: %v", menu, got)
	}
}

// The daemon passes --config-file to its children only when the config is not
// where Riggs would look anyway: passing it always puts an absolute path in
// every log line, and never sends a daemon started with --config-file to the
// wrong config in its own jobs.
func TestJobConfigFlagOnlyWhenUnusual(t *testing.T) {
	if got := (&Application{cfg: &config.Config{Path: config.DefaultPath()}}).jobConfigFlag(); got != "" {
		t.Fatalf("jobConfigFlag = %q, want empty for the default location", got)
	}
	if got := (&Application{cfg: &config.Config{Path: config.NoFilePath}}).jobConfigFlag(); got != "" {
		t.Fatalf("jobConfigFlag = %q, want empty when there is no config", got)
	}
	if got := (&Application{cfg: &config.Config{Path: "/etc/riggs.yaml"}}).jobConfigFlag(); got != "/etc/riggs.yaml" {
		t.Fatalf("jobConfigFlag = %q", got)
	}
}

// A nil pointer in a non-nil interface would make `Jobs != nil` true and panic
// on the first read.
func TestAnUnavailableLedgerLeavesTheJobsSurfaceUnwired(t *testing.T) {
	if jobStoreOrNil(nil) != nil {
		t.Fatal("a nil store became a non-nil interface")
	}
	if jobRunnerOrNil(nil) != nil {
		t.Fatal("a nil scheduler became a non-nil interface")
	}
}

// A job row's block_id is namespaced so a click on one is distinguishable from
// any other block that carries an id.
func TestJobNameStripsTheNamespace(t *testing.T) {
	if got := jobName(blockkit.HomeJobBlockPrefix + "github-review-queue"); got != "github-review-queue" {
		t.Fatalf("jobName = %q", got)
	}
	// A modal's private_metadata carries the bare name and must survive.
	if got := jobName("github-review-queue"); got != "github-review-queue" {
		t.Fatalf("jobName = %q", got)
	}
}

// The switch that joins two vocabularies. It is a switch rather than a string
// conversion even though both kinds are spelled identically on either side
// today — that agreement is a coincidence of naming, not a contract.
//
// What this actually defends is the failure mode: a kind that fell through
// would silently run on the default forever, which looks exactly like a setting
// nobody had got round to changing.
func TestConfigTimeoutsMapEveryKind(t *testing.T) {
	cfg := &config.Config{Jobs: config.Jobs{GitHubTimeout: "5m", JiraTimeout: "10m"}}
	timeouts := JobTimeouts(cfg)

	if got := timeouts.JobTimeout(schedule.KindGitHubReviews); got != 5*time.Minute {
		t.Errorf("github = %v, want 5m", got)
	}
	if got := timeouts.JobTimeout(schedule.KindJiraTickets); got != 10*time.Minute {
		t.Errorf("jira = %v, want 10m", got)
	}
	// Every kind this build runs has to be answered by name. A new one added to
	// schedule and forgotten here is what this catches.
	for _, spec := range schedule.Kinds() {
		if got := timeouts.JobTimeout(spec.Kind); got == 0 {
			t.Errorf("kind %q has no timeout setting behind it", spec.Kind)
		}
	}
	// An unknown kind falls through to zero, which the scheduler reads as "no
	// opinion" and answers with its own default.
	if got := timeouts.JobTimeout("slack-digest"); got != 0 {
		t.Errorf("an unknown kind = %v, want no opinion", got)
	}
}
