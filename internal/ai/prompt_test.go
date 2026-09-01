package ai

import (
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
)

func TestTextSubstitutesThePlaceholders(t *testing.T) {
	got := Text("Review {ref} at {url}, carefully.", "o/r#7", "https://github.com/o/r/pull/7")
	want := "Review o/r#7 at https://github.com/o/r/pull/7, carefully."
	if got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
}

// A ticket is not called a ref by anybody, so the ticket prompts say {key}.
func TestKeyIsAnAliasForRef(t *testing.T) {
	got := Text("Scope {key} at {url}.", "NYX-1", "https://example.atlassian.net/browse/NYX-1")
	if got != "Scope NYX-1 at https://example.atlassian.net/browse/NYX-1." {
		t.Fatalf("Text = %q", got)
	}
}

// The guarantee. A prompt edited to drop the reference fails SILENTLY: the
// harness starts, runs, and reports success, having reviewed whatever it found
// in the working directory.
func TestTheSubjectSurvivesAWordingThatDroppedIt(t *testing.T) {
	got := Text("Review the pull request and comment on it.", "o/r#7", "https://github.com/o/r/pull/7")
	if !strings.Contains(got, "o/r#7") {
		t.Fatalf("the reference was lost: %q", got)
	}
	if !strings.Contains(got, "https://github.com/o/r/pull/7") {
		t.Fatalf("the URL was lost: %q", got)
	}
	if !strings.HasPrefix(got, "Review the pull request and comment on it.") {
		t.Fatalf("the wording was not kept first: %q", got)
	}
}

// Only what is missing is appended: a prompt that names the reference and not
// the URL should not have the reference repeated at it.
func TestOnlyTheMissingHalfIsAppended(t *testing.T) {
	got := Text("Review {ref}.", "o/r#7", "https://github.com/o/r/pull/7")
	if strings.Count(got, "o/r#7") != 1 {
		t.Fatalf("the reference was repeated: %q", got)
	}
	if !strings.Contains(got, "https://github.com/o/r/pull/7") {
		t.Fatalf("the URL was lost: %q", got)
	}
}

// Both defaults must already place their subject, so the guarantee above never
// fires for an install that changed nothing.
func TestTheDefaultsNeedNoRescue(t *testing.T) {
	review := Text(config.DefaultAIReviewPrompt, "o/r#7", "https://github.com/o/r/pull/7")
	if strings.Contains(review, "Subject:") {
		t.Fatalf("the default review prompt needed rescuing: %q", review)
	}
	assist := Text(config.DefaultAIAssistPrompt, "NYX-1", "https://example.atlassian.net/browse/NYX-1")
	if strings.Contains(assist, "Subject:") {
		t.Fatalf("the default assist prompt needed rescuing: %q", assist)
	}
}

// The Common Rule: nothing Riggs sends anywhere may name Riggs. What the
// harness does next is submitted under the admin's own credentials.
func TestNoPromptNamesTheTool(t *testing.T) {
	for _, prompt := range []string{config.DefaultAIReviewPrompt, config.DefaultAIAssistPrompt} {
		for _, banned := range []string{"riggs", "murtaugh"} {
			if strings.Contains(strings.ToLower(prompt), banned) {
				t.Errorf("the prompt names the tool (%q): %q", banned, prompt)
			}
		}
	}
}
