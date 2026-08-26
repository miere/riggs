package slack

import (
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
)

func resolverWith(profiles map[string]config.Profile) *Resolver {
	return NewResolver(&config.Config{
		Path:  "/tmp/riggs/config.yaml",
		Slack: config.Slack{Profiles: profiles},
	})
}

func TestCredentialsResolvesBothTokens(t *testing.T) {
	r := resolverWith(map[string]config.Profile{
		"riggs": {BotToken: "xoxb-1", AppToken: "xapp-1"},
	})

	creds, err := r.Credentials("riggs")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.Profile != "riggs" || creds.BotToken != "xoxb-1" || creds.AppToken != "xapp-1" {
		t.Fatalf("Credentials = %+v", creds)
	}
}

func TestCredentialsDefaultsToTheDefaultProfile(t *testing.T) {
	r := resolverWith(map[string]config.Profile{
		config.DefaultProfile: {BotToken: "xoxb-1", AppToken: "xapp-1"},
	})
	creds, err := r.Credentials("")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}
	if creds.Profile != config.DefaultProfile {
		t.Fatalf("Profile = %q", creds.Profile)
	}
}

// A daemon that starts without an app token would sit connected to nothing, so
// the gap is reported by name rather than tolerated.
func TestCredentialsNamesTheMissingToken(t *testing.T) {
	cases := map[string]struct {
		profile config.Profile
		want    string
	}{
		"no app token": {config.Profile{BotToken: "xoxb-1"}, "app-token"},
		"no bot token": {config.Profile{AppToken: "xapp-1"}, "bot-token"},
	}
	for name, tc := range cases {
		r := resolverWith(map[string]config.Profile{"riggs": tc.profile})
		_, err := r.Credentials("riggs")
		if err == nil {
			t.Fatalf("%s: Credentials succeeded", name)
		}
		if !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: error does not wrap ErrNotConfigured: %v", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", name, err, tc.want)
		}
	}
}

func TestCredentialsRejectsAnUndefinedProfile(t *testing.T) {
	r := resolverWith(map[string]config.Profile{"riggs": {BotToken: "b", AppToken: "a"}})
	_, err := r.Credentials("nope")
	if err == nil || !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Credentials error = %v", err)
	}
	// The hint is what turns a typo into a one-step fix.
	if !strings.Contains(err.Error(), "riggs") {
		t.Errorf("error %q does not list the configured profiles", err)
	}
}
