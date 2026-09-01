package blockkit

import (
	"encoding/json"
	"strings"
	"testing"
)

// viewer is anything that renders a modal payload. Both modals do.
type viewer interface{ View() any }

func modalOf(t *testing.T, m viewer) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m.View())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// The id rides in private_metadata, not in the callback_id: the router matches
// the callback_id exactly, and a table cannot match a value that varies per
// prompt.
func TestPromptModalCarriesTheIDInPrivateMetadata(t *testing.T) {
	got := modalOf(t, PromptModal{ID: "ai_review", Label: "AI code review", Value: "review {ref}"})

	if got["type"] != "modal" {
		t.Fatalf("type = %v", got["type"])
	}
	if got["callback_id"] != PromptModalCallbackID {
		t.Fatalf("callback_id = %v, want the constant the router matches", got["callback_id"])
	}
	if got["private_metadata"] != "ai_review" {
		t.Fatalf("private_metadata = %v", got["private_metadata"])
	}
}

// An edit starts from what is actually running, not from an empty box: nobody
// retypes a paragraph to change one word of it.
func TestPromptModalPreFillsTheWordingInForce(t *testing.T) {
	got := modalOf(t, PromptModal{ID: "ai_review", Label: "AI code review",
		Hint: "{ref} becomes the pull request", Value: "review {ref}"})

	blocks := got["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want the one input", len(blocks))
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "input" || block["block_id"] != PromptModalBlockID {
		t.Fatalf("block = %v", block)
	}
	element := block["element"].(map[string]any)
	if element["action_id"] != PromptModalActionID {
		t.Fatalf("action_id = %v", element["action_id"])
	}
	// Multiline: a prompt is a paragraph, and a single-line box invites one
	// that has had its structure flattened out of it.
	if element["multiline"] != true {
		t.Fatalf("the input is not multiline: %v", element)
	}
	if element["initial_value"] != "review {ref}" {
		t.Fatalf("initial_value = %v", element["initial_value"])
	}
	if hint := block["hint"].(map[string]any); !strings.Contains(hint["text"].(string), "{ref}") {
		t.Fatalf("hint = %v", hint)
	}
}

// A title past Slack's cap is rejected wholesale — the modal simply does not
// open — so it is cut rather than gambled on.
func TestPromptModalTitleFitsSlacksCap(t *testing.T) {
	got := modalOf(t, PromptModal{ID: "x", Label: strings.Repeat("long ", 20)})
	title := got["title"].(map[string]any)["text"].(string)
	if len([]rune(title)) > modalTitleLimit {
		t.Fatalf("title is %d runes: %q", len([]rune(title)), title)
	}

	// Every real label already fits, so the cut above is a guard rather than a
	// thing anybody sees.
	for _, label := range []string{
		"Code review request", "SME assistance request", "AI code review", "AI ticket assistance",
	} {
		if len([]rune(label)) > modalTitleLimit {
			t.Errorf("%q is %d runes, past the modal title cap", label, len([]rune(label)))
		}
	}
}

// A modal for a prompt this build does not know about still has to render: an
// empty text object is rejected by Slack, and the handler's own error is a
// better message than "invalid_blocks".
func TestPromptModalWithoutALabelStillRenders(t *testing.T) {
	got := modalOf(t, PromptModal{ID: "x"})
	if title := got["title"].(map[string]any)["text"].(string); title == "" {
		t.Fatal("the title is empty, which Slack rejects")
	}
}
