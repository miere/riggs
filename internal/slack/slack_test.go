package slack

import (
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
)

// cfg builds a Config in memory, bypassing the file loader.
func cfg(admin string, profiles map[string]config.Profile) *config.Config {
	return &config.Config{
		Path:  "<test>",
		Admin: config.Admin{SlackUserID: admin},
		Slack: config.Slack{Profiles: profiles},
	}
}

var ready = map[string]config.Profile{
	"default": {BotToken: "xoxb-default"},
	"nc":      {BotToken: "xoxb-nc", UserToken: "xoxp-nc"},
}

func TestResolveNamedProfileAndChannel(t *testing.T) {
	got, err := NewResolver(cfg("U1", ready)).Resolve("nc", "C123")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Profile != "nc" || got.BotToken != "xoxb-nc" || got.UserToken != "xoxp-nc" {
		t.Errorf("target = %+v, want the nc profile's tokens", got)
	}
	if got.Channel != "C123" || got.IsDM() {
		t.Errorf("target = %+v, want the named channel", got)
	}
}

// An unnamed profile falls back to "default".
func TestResolveEmptyProfileUsesDefault(t *testing.T) {
	got, err := NewResolver(cfg("U1", ready)).Resolve("", "C1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Profile != config.DefaultProfile || got.BotToken != "xoxb-default" {
		t.Errorf("target = %+v, want the default profile", got)
	}
}

// "if default is not defined it fails" — silently not notifying is worse than
// a loud failure.
func TestResolveMissingDefaultFails(t *testing.T) {
	_, err := NewResolver(cfg("U1", map[string]config.Profile{"nc": {BotToken: "x"}})).Resolve("", "C1")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "configured: nc") {
		t.Errorf("error %q does not list the profiles that do exist", err)
	}
}

func TestResolveUnknownProfileFails(t *testing.T) {
	_, err := NewResolver(cfg("U1", ready)).Resolve("typo", "C1")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), `"typo"`) {
		t.Errorf("error %q does not name the profile that was asked for", err)
	}
}

// A profile whose ${ENV} reference expanded to nothing is not deliverable, and
// must say so rather than posting with an empty token.
func TestResolveEmptyBotTokenFails(t *testing.T) {
	_, err := NewResolver(cfg("U1", map[string]config.Profile{"default": {BotToken: ""}})).Resolve("", "C1")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "bot-token") {
		t.Errorf("error %q does not name the missing token", err)
	}
}

// No channel means DM the admin.
func TestResolveNoChannelIsDMToAdmin(t *testing.T) {
	got, err := NewResolver(cfg("U0B20G0ET9T", ready)).Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.IsDM() || got.AdminUserID != "U0B20G0ET9T" {
		t.Errorf("target = %+v, want a DM to the admin", got)
	}
	if want := "default → DM U0B20G0ET9T"; got.Describe() != want {
		t.Errorf("Describe() = %q, want %q", got.Describe(), want)
	}
}

// ...but only if there is an admin to DM.
func TestResolveNoChannelNoAdminFails(t *testing.T) {
	_, err := NewResolver(cfg("", ready)).Resolve("", "")
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "admin.slack-user-id") {
		t.Errorf("error %q does not name the setting that would fix it", err)
	}
}

func TestResolveTrimsChannel(t *testing.T) {
	got, err := NewResolver(cfg("U1", ready)).Resolve("", "  C7  ")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Channel != "C7" {
		t.Errorf("Channel = %q, want it trimmed", got.Channel)
	}
}
