package config

import (
	"os"
	"strings"
	"testing"
)

// The portrait is the one thing on the Home tab that says what this app is, and
// a new workspace member opening it is exactly who needs telling. So it is on
// until somebody turns it off.
func TestTheBannerIsOnByDefault(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")
	if !cfg.ShowBanner() {
		t.Error("the banner is off on a config that never mentioned it")
	}
	if cfg.BannerOverridden() {
		t.Error("an untouched banner reads as configured")
	}
}

// "unset" and "set to false" have to stay distinguishable — which is the whole
// reason the field is a pointer.
func TestAnExplicitFalseIsNotTheDefault(t *testing.T) {
	cfg := loadConfig(t, "home:\n  show-banner: false\n")
	if cfg.ShowBanner() {
		t.Error("show-banner: false did not hide the banner")
	}
	if !cfg.BannerOverridden() {
		t.Error("an explicit false does not read as configured")
	}
}

// The banner is written UNQUOTED. `show-banner: "false"` is a string, and the
// next load would refuse to unmarshal a string into a bool — a save that
// bricks the config file at the following restart, which is the worst possible
// moment to find out.
func TestTheBannerIsWrittenAsABoolean(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")

	if err := cfg.SetShowBanner(false); err != nil {
		t.Fatalf("SetShowBanner: %v", err)
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"false"`) {
		t.Fatalf("the boolean was quoted:\n%s", raw)
	}
	// The proof that matters: it loads again.
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("re-loading after a banner save: %v", err)
	}
	if reloaded.ShowBanner() {
		t.Error("the saved value did not survive a reload")
	}
	if cfg.ShowBanner() {
		t.Error("the in-memory value did not change")
	}
}

// Both directions, because a switch that only latches one way is a switch that
// somebody cannot undo from the surface they set it on.
func TestTheBannerCanBeTurnedBackOn(t *testing.T) {
	cfg := loadConfig(t, "home:\n  show-banner: false\n")
	if err := cfg.SetShowBanner(true); err != nil {
		t.Fatalf("SetShowBanner: %v", err)
	}
	if !cfg.ShowBanner() {
		t.Error("in memory = off, want on")
	}
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("re-loading: %v", err)
	}
	if !reloaded.ShowBanner() {
		t.Error("on disk = off, want on")
	}
}
