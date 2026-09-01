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

	if got := strings.Join(blockTypes(blocks), ","); got != "image,section" {
		t.Fatalf("blocks = %s, want image,section", got)
	}
	if text := blocks[1]["text"].(map[string]any); text["text"] != "Version: v0.1.0" {
		t.Fatalf("version line = %v", text)
	}
	if blocks[0]["image_url"] != HomePortraitURL || blocks[0]["alt_text"] != HomePortraitAlt {
		t.Fatalf("portrait = %v", blocks[0])
	}
}

// The controls menu is admin-only, like everything else that operates Riggs.
// A non-admin gets no menu at all rather than one whose only option refuses
// them.
func TestTheControlsMenuIsAdminOnly(t *testing.T) {
	if acc, found := homeBlocks(t, Home{Version: "v0.1.0"})[1]["accessory"]; found {
		t.Fatalf("a non-admin was shown the controls menu: %v", acc)
	}

	version := homeBlocks(t, Home{Version: "v0.1.0", Admin: true})[1]
	menu, ok := version["accessory"].(map[string]any)
	if !ok {
		t.Fatalf("the admin was shown no controls menu: %v", version)
	}
	if menu["type"] != "overflow" || menu["action_id"] != HomeMenuActionID {
		t.Fatalf("menu identity = %v", menu)
	}

	options := menu["options"].([]any)
	if len(options) != 1 {
		t.Fatalf("menu options = %v", options)
	}
	restart := options[0].(map[string]any)
	if restart["value"] != HomeRestartIntent {
		t.Fatalf("restart value = %v, want the bare token the router matches", restart["value"])
	}
	label := restart["text"].(map[string]any)
	if label["type"] != "plain_text" || label["text"] != "Restart" {
		t.Fatalf("restart label = %v", label)
	}
	// emoji interpretation off, per every other menu label Riggs renders.
	if label["emoji"] != false {
		t.Fatalf("restart label has emoji parsing on: %v", label)
	}
}

// Restarting has nothing to do with a release: there is something to restart
// whether or not anything is out of date.
func TestTheControlsMenuIsThereWithNoUpdate(t *testing.T) {
	withUpdate := homeBlocks(t, Home{
		Version: "v0.1.0", Admin: true, Update: &HomeUpdate{Tag: "v0.2.0", Notes: "x"},
	})
	if _, found := withUpdate[1]["accessory"]; !found {
		t.Fatal("the menu vanished when an update was available")
	}
	without := homeBlocks(t, Home{Version: "v0.1.0", Admin: true})
	if _, found := without[1]["accessory"]; !found {
		t.Fatal("the menu vanished when nothing was available to install")
	}
}

func TestHomeWithAnUpdateAppendsTheAdminSection(t *testing.T) {
	blocks := homeBlocks(t, Home{
		Version: "v0.1.0",
		Update:  &HomeUpdate{Tag: "v0.2.0", Notes: "*Fixes*\n• a thing"},
	})

	if got := strings.Join(blockTypes(blocks), ","); got != "image,section,divider,header,section" {
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
	text := homeBlocks(t, Home{})[1]["text"].(map[string]any)
	if text["text"] != "Version: unknown" {
		t.Fatalf("version line = %v", text)
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
		"menu":    {Version: "v0.1.0", Admin: true},
		"update":  {Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.2.0", Notes: "x"}},
		"tag":     {Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.3.0", Notes: "x"}},
		"notes":   {Version: "v0.1.0", Update: &HomeUpdate{Tag: "v0.2.0", Notes: "y"}},
	} {
		if other.Fingerprint() == base.Fingerprint() {
			t.Errorf("changing the %s did not change the fingerprint", name)
		}
	}
}

// --- the prompts -----------------------------------------------------------

func promptRows() []HomePrompt {
	return []HomePrompt{
		{ID: "review_request", Label: "Code review request", Text: "Hey {reviewer}, mind to review this?"},
		{ID: "ai_review", Label: "AI code review", Text: "Review {ref}.", Overridden: true},
	}
}

// The prompts sit above the update divider, because they are about how Riggs
// behaves rather than about which release it is.
func TestHomeRendersThePromptRows(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Admin: true, Prompts: promptRows()})

	if got := strings.Join(blockTypes(blocks), ","); got != "image,section,divider,header,section,section" {
		t.Fatalf("blocks = %s", got)
	}
	if header := blocks[3]["text"].(map[string]any); header["text"] != "Prompts" {
		t.Fatalf("header = %v", header)
	}

	row := blocks[4]
	if row["block_id"] != HomePromptBlockPrefix+"review_request" {
		t.Fatalf("block_id = %v, want the prompt's identity", row["block_id"])
	}
	text := row["text"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "*Code review request*") {
		t.Fatalf("row text = %q", text)
	}
	if !strings.Contains(text, "Hey {reviewer}") {
		t.Fatalf("the row does not show the wording in force: %q", text)
	}
}

// A default and an override that happens to match it are indistinguishable
// otherwise — and only one of them follows a later change to the default.
func TestAPromptOnItsDefaultSaysSoAndOffersNoReset(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Admin: true, Prompts: promptRows()})

	def := blocks[4]
	if text := def["text"].(map[string]any)["text"].(string); !strings.Contains(text, "(default)") {
		t.Fatalf("a default prompt does not say so: %q", text)
	}
	if got := optionValues(t, def); len(got) != 1 || got[0] != HomePromptEditIntent {
		t.Fatalf("options = %v, want Edit alone: there is nothing to reset", got)
	}

	overridden := blocks[5]
	if text := overridden["text"].(map[string]any)["text"].(string); strings.Contains(text, "(default)") {
		t.Fatalf("an overridden prompt claims to be the default: %q", text)
	}
	if got := optionValues(t, overridden); len(got) != 2 || got[1] != HomePromptResetIntent {
		t.Fatalf("options = %v, want Edit and Reset", got)
	}
}

// The audience split is the design: everything that operates Riggs is the
// admin's alone, and a non-admin is shown nothing rather than a control that
// would refuse them.
func TestThePromptRowsAreAdminOnly(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Prompts: promptRows()})
	if got := strings.Join(blockTypes(blocks), ","); got != "image,section" {
		t.Fatalf("blocks = %s, want the portrait and the version alone", got)
	}
}

// A "Prompts" header with nothing under it says a feature exists and is broken.
func TestNoPromptsMeansNoHeader(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Admin: true})
	if got := strings.Join(blockTypes(blocks), ","); got != "image,section" {
		t.Fatalf("blocks = %s", got)
	}
}

// Four prompts each carrying a paragraph would push the update section off the
// screen. The whole text is in the editor, one click away.
func TestALongPromptIsCutForDisplay(t *testing.T) {
	long := strings.Repeat("a", 500)
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Admin: true,
		Prompts: []HomePrompt{{ID: "ai_review", Label: "AI code review", Text: long}}})

	text := blocks[4]["text"].(map[string]any)["text"].(string)
	if len([]rune(text)) > homePromptLimit+len("*AI code review*  _(default)_\n")+2 {
		t.Fatalf("row text is %d runes", len([]rune(text)))
	}
	if !strings.Contains(text, "…") {
		t.Fatalf("a cut row does not say it was cut: %q", text)
	}
}

// A prompt is prose somebody typed. An `&` in it would re-open the row's own
// bold run and garble everything after it.
func TestAPromptRowEscapesItsText(t *testing.T) {
	blocks := homeBlocks(t, Home{Version: "v0.1.0", Admin: true,
		Prompts: []HomePrompt{{ID: "ai_review", Label: "AI & review", Text: "a < b & c"}}})

	text := blocks[4]["text"].(map[string]any)["text"].(string)
	if strings.Contains(text, "a < b & c") {
		t.Fatalf("the wording was not escaped: %q", text)
	}
	if !strings.Contains(text, "&amp;") || !strings.Contains(text, "&lt;") {
		t.Fatalf("row text = %q", text)
	}
}

// optionValues lists an overflow accessory's option values.
func optionValues(t *testing.T, block map[string]any) []string {
	t.Helper()
	acc, ok := block["accessory"].(map[string]any)
	if !ok {
		t.Fatalf("no accessory on %v", block)
	}
	if acc["action_id"] != HomePromptActionID {
		t.Fatalf("action_id = %v", acc["action_id"])
	}
	raw, ok := acc["options"].([]any)
	if !ok {
		t.Fatalf("no options on %v", acc)
	}
	out := make([]string, 0, len(raw))
	for _, o := range raw {
		out = append(out, o.(map[string]any)["value"].(string))
	}
	return out
}
