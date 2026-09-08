package blockkit

import (
	"encoding/json"
	"strings"
	"testing"
)

func customisationJSON(t *testing.T, m CustomisationModal) string {
	t.Helper()
	raw, err := json.Marshal(m.View())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func sampleCustomisation() CustomisationModal {
	return CustomisationModal{
		Emojis: []CustomisationEmoji{
			{ID: "acknowledgement", Label: "Acknowledgement", Hint: "Picked up.", Value: "saluting_face"},
			{ID: "success", Label: "Success", Hint: "Finished.", Value: "white_check_mark"},
		},
		ShowBanner: true,
	}
}

// The router matches a submission by callback_id, exactly, like every other
// control (§7b).
func TestTheCustomisationModalCarriesItsCallbackID(t *testing.T) {
	got := customisationJSON(t, sampleCustomisation())
	if !strings.Contains(got, `"callback_id":"`+CustomisationModalCallbackID+`"`) {
		t.Fatalf("callback_id missing:\n%s", got)
	}
	// No private_metadata: the form IS the item, so there is no per-item
	// identity to carry. Every other modal here has one and this deliberately
	// does not.
	if strings.Contains(got, "private_metadata") {
		t.Errorf("the modal carries a private_metadata it has no use for:\n%s", got)
	}
}

// Slack reports a submission's state under (block_id, action_id), so each field
// is addressed by a namespaced block and the shared action id. Without the
// namespace a state's token could collide with the banner's block.
func TestEachEmojiHasItsOwnNamespacedBlock(t *testing.T) {
	got := customisationJSON(t, sampleCustomisation())
	for _, id := range []string{"acknowledgement", "success"} {
		if !strings.Contains(got, `"block_id":"`+CustomisationEmojiBlockPrefix+id+`"`) {
			t.Errorf("no block for %s:\n%s", id, got)
		}
	}
	if !strings.Contains(got, `"block_id":"`+CustomisationBannerBlockID+`"`) {
		t.Errorf("no banner block:\n%s", got)
	}
}

// Every emoji box is OPTIONAL, which is the opposite of the prompt editor and
// deliberate. An empty box here has one sensible reading — "use the built-in" —
// and there is nowhere else on a five-field form to express it.
func TestTheEmojiFieldsAreOptionalAndTheBannerIsNot(t *testing.T) {
	var view struct {
		Blocks []struct {
			BlockID  string `json:"block_id"`
			Optional bool   `json:"optional"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(customisationJSON(t, sampleCustomisation())), &view); err != nil {
		t.Fatal(err)
	}
	for _, b := range view.Blocks {
		optional := b.BlockID != CustomisationBannerBlockID
		if b.Optional != optional {
			t.Errorf("block %s optional = %v, want %v", b.BlockID, b.Optional, optional)
		}
	}
}

// The switch opens on the position it is currently in, or an admin turning the
// banner off would find it reading "Show" the next time they looked.
func TestTheBannerSwitchOpensOnItsCurrentPosition(t *testing.T) {
	m := sampleCustomisation()
	m.ShowBanner = true
	if got := customisationJSON(t, m); !strings.Contains(got,
		`"initial_option":{"text":{"type":"plain_text","text":"Show"`) {
		t.Errorf("a visible banner does not open on Show:\n%s", got)
	}
	m.ShowBanner = false
	if got := customisationJSON(t, m); !strings.Contains(got,
		`"initial_option":{"text":{"type":"plain_text","text":"Hide"`) {
		t.Errorf("a hidden banner does not open on Hide:\n%s", got)
	}
}

// The box takes a NAME and people type the emoji, so the hint says so at the
// field — cheaper than rejecting it after the modal has closed.
func TestEveryEmojiFieldSaysToTypeTheShortcode(t *testing.T) {
	got := customisationJSON(t, sampleCustomisation())
	if n := strings.Count(got, "Type the shortcode, not the emoji."); n != 2 {
		t.Errorf("the shortcode hint appears %d times, want one per emoji field:\n%s", n, got)
	}
}

// A field built from a state this build does not know still renders. An empty
// text object is rejected by Slack, and a rejected view is a click that does
// nothing and says nothing.
func TestAnUnlabelledFieldStillRenders(t *testing.T) {
	got := customisationJSON(t, CustomisationModal{
		Emojis: []CustomisationEmoji{{ID: "mystery"}},
	})
	if strings.Contains(got, `"text":""`) {
		t.Fatalf("an empty text object reached the payload:\n%s", got)
	}
}

// The portrait is what says what this app is, so hiding it is a choice rather
// than the default — and hiding it must not take the version line with it.
func TestHidingTheBannerLeavesTheVersionLine(t *testing.T) {
	shown, err := json.Marshal(Home{Version: "v1.2.3"}.Blocks())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shown), HomePortraitURL) {
		t.Fatalf("the default view has no portrait:\n%s", shown)
	}

	hidden, err := json.Marshal(Home{Version: "v1.2.3", HideBanner: true}.Blocks())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hidden), HomePortraitURL) {
		t.Errorf("HideBanner left the portrait in:\n%s", hidden)
	}
	if !strings.Contains(string(hidden), "v1.2.3") {
		t.Errorf("hiding the banner took the version line with it:\n%s", hidden)
	}
}

// A control that cannot act is not drawn. The option only appears when there is
// a settings store behind it — the same rule the Update button and the Restart
// option follow.
func TestTheCustomisationOptionIsGated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		home       Home
		wantOption bool
	}{
		{"admin with customisation", Home{Admin: true, ShowCustomisation: true}, true},
		{"admin without", Home{Admin: true}, false},
		{"non-admin", Home{ShowCustomisation: true}, false},
	} {
		raw, err := json.Marshal(tc.home.Blocks())
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Contains(string(raw), HomeCustomiseIntent)
		if got != tc.wantOption {
			t.Errorf("%s: option drawn = %v, want %v:\n%s", tc.name, got, tc.wantOption, raw)
		}
	}
}

// --- Configuration ----------------------------------------------------------

// The form IS the item, like Customisation's: no private_metadata, every field
// read back by (block_id, action_id).
func TestConfigurationModalFields(t *testing.T) {
	got := modalOf(t, ConfigurationModal{Timeouts: []ConfigurationTimeout{
		{ID: "github-reviews", Label: "Pull requests timeout", Hint: "How long one pass may take.", Value: "2m0s"},
		{ID: "jira-tickets", Label: "Jira tickets timeout", Hint: "How long one pass may take.", Value: "10m0s"},
	}})

	if got["callback_id"] != ConfigurationModalCallbackID {
		t.Fatalf("callback_id = %v", got["callback_id"])
	}
	if meta, present := got["private_metadata"]; present && meta != "" {
		t.Fatalf("private_metadata = %v, want none: the form is the item", meta)
	}
	blocks := got["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want one per kind", len(blocks))
	}
	first := blocks[0].(map[string]any)
	if first["block_id"] != ConfigurationTimeoutBlockPrefix+"github-reviews" {
		t.Fatalf("block_id = %v, want the kind's own token namespaced", first["block_id"])
	}
	// Pre-filled with what is IN FORCE, so an admin giving a digest more room
	// starts from what it has now.
	element := first["element"].(map[string]any)
	if element["initial_value"] != "2m0s" || element["action_id"] != ConfigurationActionID {
		t.Fatalf("element = %v", element)
	}
}

// Every field is optional, like Customisation's: an empty box here has exactly
// one reading — "use the built-in bound" — and there is nowhere else on a modal
// that edits several settings at once to express a reset.
func TestEveryConfigurationFieldIsOptional(t *testing.T) {
	blocks := modalOf(t, ConfigurationModal{Timeouts: []ConfigurationTimeout{
		{ID: "jira-tickets", Label: "Jira tickets timeout", Hint: "How long."},
	}})["blocks"].([]any)

	block := blocks[0].(map[string]any)
	if optional, _ := block["optional"].(bool); !optional {
		t.Fatal("a timeout is required, so it could never be reset")
	}
	if hint := text(block["hint"]); !strings.Contains(hint, "Empty uses the default") {
		t.Fatalf("hint = %q, want it to say what an empty box means", hint)
	}
}
