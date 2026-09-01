package app

import (
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/apphome"
	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/daemon"
	"github.com/miere/riggs-mcp/internal/pullrequest"
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
		// The ticket digest rows' menu. "Assign to Me" is absent because it is
		// not rendered: the verb exists, the option deliberately does not.
		ticket.BulkActionID + "/" + ticket.IntentAskAssist,
		// The ask-review card's Approve, which leaves no comment.
		pullrequest.AskActionID + "/" + pullrequest.IntentApprove,
		// The pull-request digest rows' menu.
		pullrequest.BulkActionID + "/" + pullrequest.IntentApproveMerge,
		pullrequest.BulkActionID + "/" + pullrequest.IntentAskReview,
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("routes = %v, want %v", got, want)
		}
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
		blockkit.HomeMenuActionID + "/" + blockkit.HomeRestartIntent,
		blockkit.HomePromptActionID + "/" + blockkit.HomePromptEditIntent,
		blockkit.HomePromptActionID + "/" + blockkit.HomePromptResetIntent,
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
		blockkit.HomeMenuActionID + "/" + blockkit.HomeNewJobIntent,
		blockkit.JobModalCallbackID + "/" + slack.ViewSubmitIntent,
	}
	assertRoutes(t, router.Routes(), want)
}

// `New job…` and Restart share the controls menu, so they must not collide.
func TestTheControlsMenuRoutesBothOfItsOptions(t *testing.T) {
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
	if menu != 2 {
		t.Fatalf("controls-menu routes = %d, want Restart and New job: %v", menu, got)
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
