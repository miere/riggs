package config

import (
	"strings"
	"testing"
	"time"
)

// Both "Run …" options are off until a command is configured. There is no
// default harness: guessing at one would mean a machine quietly shelling out to
// whatever happened to be on its PATH.
func TestAIIsDisabledWithoutACommand(t *testing.T) {
	cfg := &Config{}
	if cfg.AIEnabled() {
		t.Fatal("AIEnabled with no command configured")
	}
	cfg.AI.Command = "   "
	if cfg.AIEnabled() {
		t.Fatal("whitespace counts as a command")
	}
	cfg.AI.Command = "claude"
	if !cfg.AIEnabled() || cfg.AICommand() != "claude" {
		t.Fatalf("AICommand = %q, enabled = %v", cfg.AICommand(), cfg.AIEnabled())
	}
}

func TestAIPromptsFallBackToTheDefaults(t *testing.T) {
	cfg := &Config{}
	if got := cfg.AIReviewPrompt(); got != DefaultAIReviewPrompt {
		t.Fatalf("AIReviewPrompt = %q, want the default", got)
	}
	if got := cfg.AIAssistPrompt(); got != DefaultAIAssistPrompt {
		t.Fatalf("AIAssistPrompt = %q, want the default", got)
	}
	// The two defaults must not be each other's: one reviews code that exists,
	// the other scopes work that does not.
	if DefaultAIReviewPrompt == DefaultAIAssistPrompt {
		t.Fatal("the two default prompts are identical")
	}
	// Each must carry its own placeholder, or the guarantee in internal/ai is
	// the only thing naming the subject and the default reads as an oversight.
	if !strings.Contains(DefaultAIReviewPrompt, "{ref}") {
		t.Fatalf("the review default names no pull request: %q", DefaultAIReviewPrompt)
	}
	if !strings.Contains(DefaultAIAssistPrompt, "{key}") {
		t.Fatalf("the assist default names no ticket: %q", DefaultAIAssistPrompt)
	}

	cfg.AI.ReviewPrompt = "look at {ref}"
	if got := cfg.AIReviewPrompt(); got != "look at {ref}" {
		t.Fatalf("AIReviewPrompt = %q", got)
	}
}

// A duration typo is caught at load. Left to run time it would fall back
// silently, and the operator who wrote `timeout: 20` would believe runs were
// capped at twenty of something.
func TestAITimeoutIsValidatedAtLoad(t *testing.T) {
	if _, err := parse("/tmp/riggs/config.yaml", []byte("ai:\n  command: claude\n  timeout: \"20\"\n")); err == nil {
		t.Fatal("a malformed ai.timeout loaded without complaint")
	}
	if _, err := parse("/tmp/riggs/config.yaml", []byte("ai:\n  command: claude\n  timeout: -5m\n")); err == nil {
		t.Fatal("a negative ai.timeout loaded without complaint")
	}
	cfg, err := parse("/tmp/riggs/config.yaml", []byte("ai:\n  command: claude\n  timeout: 25m\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.AITimeout(); got != 25*time.Minute {
		t.Fatalf("AITimeout = %v", got)
	}
	if got := (&Config{}).AITimeout(); got != DefaultAITimeout {
		t.Fatalf("AITimeout = %v, want the default", got)
	}
}

// KnownFields(true) means an unmodelled key is refused, so the new section has
// to round-trip through the loader.
func TestAILoadsFromYAML(t *testing.T) {
	t.Setenv("RIGGS_TEST_WORKDIR", "/home/m/Development")
	cfg, err := parse("/tmp/riggs/config.yaml", []byte(`
ai:
  command: claude --model opus
  workdir: ${RIGGS_TEST_WORKDIR}
  pull-request-prompt: review {ref}
  ticket-prompt: scope {key}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.AICommand() != "claude --model opus" {
		t.Fatalf("command = %q", cfg.AICommand())
	}
	// The working directory is expanded, so `workdir: ${HOME}/Development`
	// resolves rather than naming a literal directory called "${HOME}".
	if cfg.AIWorkDir() != "/home/m/Development" {
		t.Fatalf("workdir = %q, want it expanded", cfg.AIWorkDir())
	}
	if cfg.AIReviewPrompt() != "review {ref}" || cfg.AIAssistPrompt() != "scope {key}" {
		t.Fatalf("prompts = %q / %q", cfg.AIReviewPrompt(), cfg.AIAssistPrompt())
	}
}

// A prompt is prose. Expanding it would turn a dollar sign somebody typed into
// an empty string, silently.
func TestPromptsAreNotEnvironmentExpanded(t *testing.T) {
	t.Setenv("RIGGS_TEST_SECRET", "leaked")
	cfg, err := parse("/tmp/riggs/config.yaml",
		[]byte("ai:\n  command: claude\n  pull-request-prompt: charge $10, not ${RIGGS_TEST_SECRET}\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if strings.Contains(cfg.AIReviewPrompt(), "leaked") {
		t.Fatalf("the prompt was expanded: %q", cfg.AIReviewPrompt())
	}
}

// The section was called `ai-assistance` when the ticket ask was called "Ask
// for AI Assistance" and reached a person. Refusing to boot over the spelling
// would cost a working install for nothing.
func TestTheRetiredAIAssistanceKeyStillLoads(t *testing.T) {
	cfg, err := parse("/tmp/riggs/config.yaml", []byte(`
ai-assistance:
  channel: C-tickets
  user-id: U-expert
  prompt: have a look at {key}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SMEUser() != "U-expert" || cfg.SMEAssistance.Channel != "C-tickets" {
		t.Fatalf("the legacy section was not adopted: %+v", cfg.SMEAssistance)
	}
	if cfg.SMEPrompt() != "have a look at {key}" {
		t.Fatalf("prompt = %q", cfg.SMEPrompt())
	}
	if !cfg.SMEDeprecated() {
		t.Fatal("the deprecation was not recorded, so capabilities cannot report it")
	}
}

// A half-finished hand edit leaves both spellings in the file. The new one
// wins, field by field: the other reading would let a forgotten old key
// silently override a deliberate new one.
func TestTheNewSMEKeyBeatsTheLegacyOne(t *testing.T) {
	cfg, err := parse("/tmp/riggs/config.yaml", []byte(`
sme-assistance:
  user-id: U-new
ai-assistance:
  user-id: U-old
  channel: C-old
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SMEUser() != "U-new" {
		t.Fatalf("SMEUser = %q, want the new key to win", cfg.SMEUser())
	}
	// A field the new section left blank is still filled from the old one:
	// adopting half a section and dropping the rest would be worse than either.
	if cfg.SMEAssistance.Channel != "C-old" {
		t.Fatalf("channel = %q, want it filled from the legacy section", cfg.SMEAssistance.Channel)
	}
}
