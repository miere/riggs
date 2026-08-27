package blockkit

import (
	"encoding/json"
	"strings"
	"testing"
)

func homeBlocks(t *testing.T, h Home) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(h.Blocks())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func blockTypes(blocks []map[string]any) []string {
	types := make([]string, 0, len(blocks))
	for _, b := range blocks {
		types = append(types, b["type"].(string))
	}
	return types
}

// Everyone gets the portrait and the version, and nothing else. The divider is
// the boundary: past it is machinery.
func TestHomeWithoutAnUpdateStopsAtTheVersion(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0"})

	if got := strings.Join(blockTypes(blocks), ","); got != "image,context" {
		t.Fatalf("blocks = %s, want image,context", got)
	}
	ctx := blocks[1]["elements"].([]any)[0].(map[string]any)
	if ctx["text"] != "Version: v0.1.0" {
		t.Fatalf("version line = %v", ctx["text"])
	}
	if blocks[0]["image_url"] != HomePortraitURL || blocks[0]["alt_text"] != HomePortraitAlt {
		t.Fatalf("portrait = %v", blocks[0])
	}
}

func TestHomeWithAnUpdateAppendsTheAdminSection(t *testing.T) {
	blocks := homeBlocks(t, Home{
		Version: "v0.1.0",
		Update:  &HomeUpdate{Tag: "v0.2.0", Notes: "*Fixes*\n• a thing"},
	})

	if got := strings.Join(blockTypes(blocks), ","); got != "image,context,divider,header,section" {
		t.Fatalf("blocks = %s", got)
	}

	header := blocks[3]["text"].(map[string]any)
	if header["text"] != "Update Available: v0.2.0" {
		t.Fatalf("header = %v", header["text"])
	}

	notes := blocks[4]
	if notes["block_id"] != HomeReleaseNotesBlockID {
		t.Fatalf("notes block_id = %v, want %q", notes["block_id"], HomeReleaseNotesBlockID)
	}
	if text := notes["text"].(map[string]any); text["type"] != "mrkdwn" || text["text"] != "*Fixes*\n• a thing" {
		t.Fatalf("notes text = %v", text)
	}

	button := notes["accessory"].(map[string]any)
	if button["type"] != "button" || button["style"] != "primary" {
		t.Fatalf("accessory = %v", button)
	}
	// The router matches on (action_id, value) exactly, so both must be the
	// bare tokens it was registered with — not a release tag.
	if button["action_id"] != HomeUpdateActionID || button["value"] != HomeUpdateIntent {
		t.Fatalf("button identity = %v/%v", button["action_id"], button["value"])
	}
}

// A release published with no notes is a real case, and an empty section is a
// payload Slack rejects outright — which would take the whole tab down, not
// just the notes.
func TestHomeSubstitutesForEmptyReleaseNotes(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.2.0"}})
	text := blocks[4]["text"].(map[string]any)["text"].(string)
	if strings.TrimSpace(text) == "" {
		t.Fatal("the notes section is empty, which Slack will reject")
	}
	if !strings.Contains(text, "v0.2.0") {
		t.Fatalf("the stand-in does not name the release: %q", text)
	}
}

// Release notes routinely run past Slack's section limit, and an over-long
// payload is rejected whole — the tab would go blank rather than show a long
// note.
func TestHomeCutsOverlongReleaseNotes(t *testing.T) {
	blocks := homeBlocks(t, Home{
		Version: "v0.1.0",
		Update:  &HomeUpdate{Tag: "v0.2.0", Notes: strings.Repeat("a", 5000)},
	})
	text := blocks[4]["text"].(map[string]any)["text"].(string)
	if len([]rune(text)) > 3000 {
		t.Fatalf("notes are %d runes, past Slack's section limit", len([]rune(text)))
	}
}

func TestHomeReportsAnUnknownVersion(t *testing.T) {
	blocks := homeBlocks(t, Home{})
	ctx := blocks[1]["elements"].([]any)[0].(map[string]any)
	if ctx["text"] != "Version: unknown" {
		t.Fatalf("version line = %v", ctx["text"])
	}
}

func TestHomeViewIsAHomeSurface(t *testing.T) {
	raw, err := json.Marshal(Home{Version: "v0.1.0"}.View())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var view map[string]any
	json.Unmarshal(raw, &view)
	if view["type"] != "home" {
		t.Fatalf("view type = %v", view["type"])
	}
	if len(view["blocks"].([]any)) != 2 {
		t.Fatalf("view blocks = %v", view["blocks"])
	}
}

// The fingerprint is what stops a republish on every glance at the app, so it
// has to be stable across renders and sensitive to everything visible.
func TestHomeFingerprintTracksWhatIsVisible(t *testing.T) {
	base := Home{Version: "v0.1.0"}
	if base.Fingerprint() != (Home{Version: "v0.1.0"}).Fingerprint() {
		t.Fatal("two renders of the same view disagree")
	}
	for name, other := range map[string]Home{
		"version": {Version: "v0.2.0"},
		"update":  {Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.2.0", Notes: "x"}},
		"tag":     {Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.3.0", Notes: "x"}},
		"notes":   {Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.2.0", Notes: "y"}},
	} {
		if other.Fingerprint() == base.Fingerprint() {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}
}
