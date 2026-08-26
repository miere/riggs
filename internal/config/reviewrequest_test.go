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

// Asking yourself is a defensible default; silently tagging a stranger is not.
func TestReviewReviewerFallsBackToTheAdmin(t *testing.T) {
	cfg := &Config{Admin: Admin{SlackUserID: "U-admin"}}
	if got := cfg.ReviewReviewer(); got != "U-admin" {
		t.Fatalf("ReviewReviewer = %q, want the admin", got)
	}

	cfg.ReviewRequest.UserID = "U-someone"
	if got := cfg.ReviewReviewer(); got != "U-someone" {
		t.Fatalf("ReviewReviewer = %q", got)
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
