package config

import "strings"

// The four prompts Riggs sends on somebody's behalf, named so a surface can
// list and edit them without knowing where in the YAML each one lives.
//
// This registry exists because the App Home tab edits them (§7e). Without it
// the Home tab would need its own table of ids, labels, defaults, getters and
// YAML paths — a second description of the config that has to be kept in step
// with this one, and which nothing would fail to warn about when it drifted.
//
// Two of them are the wording of an ask handed to a PERSON; two are the
// instruction handed to a HARNESS. They are listed together because they are
// the same kind of setting — prose the admin owns, which Riggs sends under
// their name — and separating them here would only mean two registries and one
// arbitrary choice about which surface shows which.

// PromptID names one editable prompt. It is a bare token: it rides in a Slack
// block_id and in a modal's private_metadata, and the router matches it
// exactly (§7b).
type PromptID string

const (
	// PromptReviewRequest is the wording of "Ask for Code Review".
	PromptReviewRequest PromptID = "review_request"
	// PromptSMEAssistance is the wording of "Ask for SME Assistance".
	PromptSMEAssistance PromptID = "sme_assistance"
	// PromptAIReview is the instruction "Run Code Review" hands the harness.
	PromptAIReview PromptID = "ai_review"
	// PromptAIAssist is the instruction "Run AI Assistance" hands the harness.
	PromptAIAssist PromptID = "ai_assist"
)

// PromptSpec describes one editable prompt.
type PromptSpec struct {
	// ID is the token surfaces address it by.
	ID PromptID
	// Label is the human name, shown on the Home tab and in the modal title.
	Label string
	// Hint explains the placeholders, shown under the modal's input.
	Hint string
	// Path is where the value lives in the YAML file, as a sequence of mapping
	// keys. The writer creates whatever is missing along it.
	Path []string
	// Default is what an unset prompt resolves to.
	Default string

	// get reads the RAW configured value — empty when the default is in force.
	// The distinction matters to the writer: resetting a prompt deletes the
	// key rather than writing the default text into the file, so a later change
	// to the default reaches an install that never overrode it.
	get func(*Config) string
	// set writes the value back into the loaded Config, so the running daemon
	// acts on an edit without being restarted.
	set func(*Config, string)
}

// prompts is the registry, in the order surfaces list them: the two human asks
// first, then the two harness instructions. That is the order the actions
// appear on a digest row, which is the order somebody looking for one of them
// will expect.
var prompts = []PromptSpec{
	{
		ID:    PromptReviewRequest,
		Label: "Code review request",
		Hint:  "Sent to a person. {reviewer} and {requester} become mentions; both are added if the wording leaves them out.",
		Path:  []string{"review-request", "prompt"},
		// Default: DefaultReviewPrompt, filled in by init.
		get: func(c *Config) string { return c.ReviewRequest.Prompt },
		set: func(c *Config, v string) { c.ReviewRequest.Prompt = v },
	},
	{
		ID:    PromptSMEAssistance,
		Label: "SME assistance request",
		Hint:  "Sent to a person. {user} and {requester} become mentions; both are added if the wording leaves them out.",
		Path:  []string{"sme-assistance", "prompt"},
		get:   func(c *Config) string { return c.SMEAssistance.Prompt },
		set:   func(c *Config, v string) { c.SMEAssistance.Prompt = v },
	},
	{
		ID:    PromptAIReview,
		Label: "AI code review",
		Hint:  "Handed to the harness. {ref} and {url} become the pull request; both are appended if the wording leaves them out.",
		Path:  []string{"ai", "pull-request-prompt"},
		get:   func(c *Config) string { return c.AI.ReviewPrompt },
		set:   func(c *Config, v string) { c.AI.ReviewPrompt = v },
	},
	{
		ID:    PromptAIAssist,
		Label: "AI ticket assistance",
		Hint:  "Handed to the harness. {key} and {url} become the ticket; both are appended if the wording leaves them out.",
		Path:  []string{"ai", "ticket-prompt"},
		get:   func(c *Config) string { return c.AI.AssistPrompt },
		set:   func(c *Config, v string) { c.AI.AssistPrompt = v },
	},
}

// The defaults are attached here rather than in the literal above so there is
// exactly one definition of each: the const the rest of the package already
// resolves to.
func init() {
	defaults := map[PromptID]string{
		PromptReviewRequest: DefaultReviewPrompt,
		PromptSMEAssistance: DefaultSMEPrompt,
		PromptAIReview:      DefaultAIReviewPrompt,
		PromptAIAssist:      DefaultAIAssistPrompt,
	}
	for i := range prompts {
		prompts[i].Default = defaults[prompts[i].ID]
	}
}

// Prompts lists the editable prompts, in display order.
//
// A copy, because a caller iterating this to draw a menu has no business
// reaching back through the slice header into the registry.
func Prompts() []PromptSpec {
	out := make([]PromptSpec, len(prompts))
	copy(out, prompts)
	return out
}

// LookupPrompt finds one by id. ok is false for a token that names nothing,
// which is what a click on a Home tab published by an older build looks like.
func LookupPrompt(id PromptID) (PromptSpec, bool) {
	for _, s := range prompts {
		if s.ID == id {
			return s, true
		}
	}
	return PromptSpec{}, false
}

// PromptText is the effective wording of one prompt: what is configured, or the
// default. An unknown id yields empty rather than panicking — see LookupPrompt.
func (c *Config) PromptText(id PromptID) string {
	spec, ok := LookupPrompt(id)
	if !ok {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v := strings.TrimSpace(spec.get(c)); v != "" {
		return v
	}
	return spec.Default
}

// PromptOverridden reports whether this prompt is configured rather than
// running on its default. Surfaces say so, and the reset control is only drawn
// when there is something to reset.
func (c *Config) PromptOverridden(id PromptID) bool {
	spec, ok := LookupPrompt(id)
	if !ok {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return strings.TrimSpace(spec.get(c)) != ""
}
