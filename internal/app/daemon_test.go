package app

import (
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/daemon"
	"github.com/miere/riggs-mcp/internal/pullrequest"
	"github.com/miere/riggs-mcp/internal/slack"
)

// The digest renders three options; exactly two of them are meant to be
// answered here. "Open on Browser" is Slack's own job — a handler that exists
// only to return nil would be worse than the router's "no handler" log line.
func TestDaemonRegistersTheDigestActions(t *testing.T) {
	a := &Application{cfg: &config.Config{}}
	router := daemon.NewRouter()
	a.registerInteractions(router, slack.Credentials{Profile: "riggs"})

	got := router.Routes()
	want := []string{
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
