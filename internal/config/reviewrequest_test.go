package config

import "testing"

func TestReviewPromptFallsBackToTheDefault(t *testing.T) {
	if got := (&Config{}).ReviewPrompt(); got != DefaultReviewPrompt {
		t.Fatalf("ReviewPrompt = %q, want the default", got)
	}
	// Whitespace is not a prompt.
	if got := (&Config{ReviewRequest: ReviewRequest{Prompt: "   "}}).ReviewPrompt(); got != DefaultReviewPrompt {
		t.Fatalf("ReviewPrompt = %q, want the default", got)
	}
	cfg := &Config{ReviewRequest: ReviewRequest{Prompt: "have a look"}}
	if got := cfg.ReviewPrompt(); got != "have a look" {
		t.Fatalf("ReviewPrompt = %q", got)
	}
}

// The admin fallback is gone. It was defensible while "Ask for Code Review" was
// the only verb on the row — asking yourself at least reaches somebody — and is
// not beside "Run Code Review", where a menu entry that quietly means "send
// myself a card" reads as a bug next to one that does the work.
func TestReviewReviewerHasNoAdminFallback(t *testing.T) {
	cfg := &Config{Admin: Admin{SlackUserID: "U-admin"}}
	if got := cfg.ReviewReviewer(); got != "" {
		t.Fatalf("ReviewReviewer = %q, want empty: an unanswered setting disables the option", got)
	}
	if cfg.ReviewEnabled() {
		t.Fatal("ReviewEnabled with nobody configured")
	}

	cfg.ReviewRequest.UserID = "U-someone"
	if got := cfg.ReviewReviewer(); got != "U-someone" {
		t.Fatalf("ReviewReviewer = %q", got)
	}
	if !cfg.ReviewEnabled() {
		t.Fatal("ReviewEnabled is false with a reviewer configured")
	}
}

// The same rule on the ticket side, and it is a separate assertion because the
// two settings are deliberately never defaulted from one another.
func TestSMEUserHasNoAdminFallback(t *testing.T) {
	cfg := &Config{Admin: Admin{SlackUserID: "U-admin"}}
	if got := cfg.SMEUser(); got != "" {
		t.Fatalf("SMEUser = %q, want empty", got)
	}
	if cfg.SMEEnabled() {
		t.Fatal("SMEEnabled with nobody configured")
	}
	cfg.SMEAssistance.UserID = "U-expert"
	if !cfg.SMEEnabled() || cfg.SMEUser() != "U-expert" {
		t.Fatalf("SMEUser = %q, enabled = %v", cfg.SMEUser(), cfg.SMEEnabled())
	}
}

// KnownFields(true) means an unmodelled key is refused, so the new block has to
// round-trip through the loader.
func TestReviewRequestLoadsFromYAML(t *testing.T) {
	cfg, err := parse("/tmp/riggs/config.yaml", []byte(`
admin:
  slack-user-id: U-admin
review-request:
  channel: C-reviews
  user-id: U-someone
  prompt: please review
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.ReviewRequest.Channel != "C-reviews" {
		t.Errorf("channel = %q", cfg.ReviewRequest.Channel)
	}
	if cfg.ReviewReviewer() != "U-someone" {
		t.Errorf("reviewer = %q", cfg.ReviewReviewer())
	}
	if cfg.ReviewPrompt() != "please review" {
		t.Errorf("prompt = %q", cfg.ReviewPrompt())
	}
}
