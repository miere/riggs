package config

import "testing"

// The registry is what the Home tab draws from. A prompt missing here is a
// prompt nobody can edit, and one whose path is wrong is an edit that goes
// somewhere else.
func TestEveryPromptIsRegisteredAndReadable(t *testing.T) {
	cfg := &Config{}
	seen := map[PromptID]bool{}
	for _, spec := range Prompts() {
		if seen[spec.ID] {
			t.Fatalf("%s is registered twice", spec.ID)
		}
		seen[spec.ID] = true

		if spec.Label == "" || spec.Hint == "" {
			t.Errorf("%s has no label or hint, so its row and its modal render blank", spec.ID)
		}
		if len(spec.Path) != 2 {
			t.Errorf("%s path = %v, want a two-level YAML path", spec.ID, spec.Path)
		}
		if spec.Default == "" {
			t.Errorf("%s has no default, so an unset prompt sends nothing", spec.ID)
		}
		// The effective wording of an unconfigured prompt IS its default.
		if got := cfg.PromptText(spec.ID); got != spec.Default {
			t.Errorf("%s: PromptText = %q, want the default %q", spec.ID, got, spec.Default)
		}
		if cfg.PromptOverridden(spec.ID) {
			t.Errorf("%s reports an override on an empty config", spec.ID)
		}
	}

	for _, want := range []PromptID{
		PromptReviewRequest, PromptSMEAssistance, PromptAIReview, PromptAIAssist,
	} {
		if !seen[want] {
			t.Errorf("%s is not registered", want)
		}
	}
}

// Each spec's getter must reach its own field. A copy-paste that points two of
// them at the same one is invisible until somebody edits the wrong prompt.
func TestEachPromptReadsItsOwnField(t *testing.T) {
	cfg := &Config{}
	cfg.ReviewRequest.Prompt = "review-request"
	cfg.SMEAssistance.Prompt = "sme-assistance"
	cfg.AI.ReviewPrompt = "ai-review"
	cfg.AI.AssistPrompt = "ai-assist"

	for id, want := range map[PromptID]string{
		PromptReviewRequest: "review-request",
		PromptSMEAssistance: "sme-assistance",
		PromptAIReview:      "ai-review",
		PromptAIAssist:      "ai-assist",
	} {
		if got := cfg.PromptText(id); got != want {
			t.Errorf("%s: PromptText = %q, want %q", id, got, want)
		}
		if !cfg.PromptOverridden(id) {
			t.Errorf("%s: a configured prompt does not report as overridden", id)
		}
	}

	// And the wrappers the rest of the codebase calls resolve through the same
	// registry, so an edit reaches the code that sends the message.
	if cfg.ReviewPrompt() != "review-request" || cfg.SMEPrompt() != "sme-assistance" {
		t.Fatalf("the ask wrappers disagree with the registry: %q / %q",
			cfg.ReviewPrompt(), cfg.SMEPrompt())
	}
	if cfg.AIReviewPrompt() != "ai-review" || cfg.AIAssistPrompt() != "ai-assist" {
		t.Fatalf("the run wrappers disagree with the registry: %q / %q",
			cfg.AIReviewPrompt(), cfg.AIAssistPrompt())
	}
}

// A click on a Home tab published by an older build names a prompt this one
// does not have. It must not panic and must not answer for a different prompt.
func TestAnUnknownPromptIsNotAPrompt(t *testing.T) {
	if _, ok := LookupPrompt("nonsense"); ok {
		t.Fatal("LookupPrompt accepted a token that names nothing")
	}
	cfg := &Config{}
	if got := cfg.PromptText("nonsense"); got != "" {
		t.Fatalf("PromptText = %q, want empty", got)
	}
	if cfg.PromptOverridden("nonsense") {
		t.Fatal("an unknown prompt reports as overridden")
	}
}
