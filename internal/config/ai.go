package config

import (
	"fmt"
	"strings"
	"time"
)

// AI configures the local harness Riggs runs for "Run Code Review" and "Run AI
// Assistance" — the two actions that actually do the work, as opposed to the
// two that ask a person to.
//
// It holds no credential. The harness carries its own auth, exactly as `gh`
// does for GitHub, which keeps Riggs out of the business of storing another key
// and means a machine that can already run Claude Code needs nothing extra.
//
// One command serves both domains, because it is one harness; the two prompts
// are separate, because reviewing code that exists and scoping work that does
// not are different instructions. That split mirrors review-request against
// sme-assistance one level up, and for the same reason: a shared setting would
// mean changing one silently moved the other.
type AI struct {
	// Command is the harness, with any fixed arguments. Empty disables both
	// actions, and they are not rendered at all.
	//
	// A command whose program is a known harness gets that harness's prompt
	// flag; anything else is handed the prompt as its first argument. The list
	// and the rule live in internal/ai, which is what actually runs it.
	//
	// Split on whitespace, with no quote handling. A harness invocation that
	// needs more than that wants a wrapper script, which is one line and is
	// legible from the outside — unlike a quoting dialect invented here.
	Command string `yaml:"command"`

	// WorkDir is the directory the harness runs in. Empty inherits Riggs' own,
	// which under launchd is very likely `/`.
	//
	// This is asked for at install time rather than assumed, because it is the
	// setting that decides whether the feature works at all. Claude Code reads
	// the project it is standing in — its CLAUDE.md, its permissions, its git
	// remote — so a harness started in the wrong place is not slightly worse,
	// it is a review of nothing.
	WorkDir string `yaml:"workdir"`

	// ReviewPrompt is what a pull-request run is asked to do. Empty uses
	// DefaultAIReviewPrompt.
	//
	// `{ref}` and `{url}` are replaced with the pull request's reference and
	// browser URL. A prompt that names neither still gets both appended — see
	// internal/ai, which guarantees it for the same reason internal/ask
	// guarantees a mention.
	ReviewPrompt string `yaml:"pull-request-prompt"`

	// AssistPrompt is what a ticket run is asked to do. Empty uses
	// DefaultAIAssistPrompt. `{key}` and `{url}` are the placeholders, on the
	// same guarantee.
	AssistPrompt string `yaml:"ticket-prompt"`

	// Timeout bounds one run, as a Go duration ("20m"). Empty uses
	// DefaultAITimeout.
	//
	// Bounded rather than open-ended: this runs under a daemon nobody is
	// watching, and a harness that has hung is indistinguishable from one that
	// is thinking hard right up until the machine runs out of them.
	Timeout string `yaml:"timeout"`
}

// DefaultAIReviewPrompt is what a pull-request run asks for when the config
// defines none.
//
// It says what to do and where to put it, and it deliberately does not approve
// or merge anything: those are separate options on the same menu, pressed by a
// human who has read the result.
const DefaultAIReviewPrompt = "Review the pull request {ref} ({url}). " +
	"Leave your review as a comment on the pull request. Do not approve it and do not merge it."

// DefaultAIAssistPrompt is what a ticket run asks for when the config defines
// none.
//
// The ticket equivalent of the review prompt, and it asks the same question the
// human ask does — is this actionable as written — so the two are comparable
// when somebody has run both.
const DefaultAIAssistPrompt = "Look at the Jira ticket {key} ({url}) and assess whether it is " +
	"actionable as written. Leave your assessment as a comment on the ticket."

// DefaultAITimeout bounds one harness run.
//
// Fifteen minutes: long enough for a real review of a real pull request, short
// enough that a hung process is reported the same morning rather than found
// next week. The retired `internal/ai` used two minutes, which was right for a
// one-paragraph summary and is nowhere near enough for this.
const DefaultAITimeout = 15 * time.Minute

// AIEnabled reports whether the two "Run …" actions can be offered.
func (c *Config) AIEnabled() bool { return c.AICommand() != "" }

// AICommand is the configured harness invocation, trimmed.
func (c *Config) AICommand() string { return strings.TrimSpace(c.AI.Command) }

// AIWorkDir is the directory the harness runs in, or empty for Riggs' own.
func (c *Config) AIWorkDir() string { return strings.TrimSpace(c.AI.WorkDir) }

// AIReviewPrompt is the configured pull-request prompt, or the default.
func (c *Config) AIReviewPrompt() string { return c.PromptText(PromptAIReview) }

// AIAssistPrompt is the configured ticket prompt, or the default.
func (c *Config) AIAssistPrompt() string { return c.PromptText(PromptAIAssist) }

// AITimeout is the configured bound, or the default.
//
// An unparseable value cannot reach here: validate rejects it at load, which is
// where a duration typo should be caught. This falling back rather than
// returning an error is therefore belt and braces, not a second policy.
func (c *Config) AITimeout() time.Duration {
	raw := strings.TrimSpace(c.AI.Timeout)
	if raw == "" {
		return DefaultAITimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultAITimeout
	}
	return d
}

// validateAI reports a malformed timeout.
//
// A duration is checked at load rather than at click time because the failure
// is silent otherwise: "20" parses as nothing, falls back to fifteen minutes,
// and the operator who wrote it believes runs are capped at twenty.
func (c *Config) validateAI() []string {
	raw := strings.TrimSpace(c.AI.Timeout)
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return []string{fmt.Sprintf("ai.timeout %q is not a duration (e.g. 15m, 1h)", raw)}
	}
	if d <= 0 {
		return []string{fmt.Sprintf("ai.timeout %q must be positive", raw)}
	}
	return nil
}
