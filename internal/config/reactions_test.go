package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadConfig puts a config on disk and loads it, so a test asserts against the
// same path the daemon takes rather than a struct literal nobody constructs.
func loadConfig(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// An unconfigured install reacts with the built-ins. Nothing has to be written
// down for the feature to work.
func TestTheDefaultEmojisApply(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")
	for _, tc := range []struct {
		id   ReactionID
		want string
	}{
		{ReactionAcknowledgement, DefaultAcknowledgementEmoji},
		{ReactionDisregard, DefaultDisregardEmoji},
		{ReactionSuccess, DefaultSuccessEmoji},
		{ReactionWarning, DefaultWarningEmoji},
	} {
		if got := cfg.ReactionEmoji(tc.id); got != tc.want {
			t.Errorf("ReactionEmoji(%s) = %q, want %q", tc.id, got, tc.want)
		}
		if cfg.ReactionOverridden(tc.id) {
			t.Errorf("%s reads as overridden on a config that never mentioned it", tc.id)
		}
	}
}

// The colons are absorbed wherever they arrive: from the modal, and from a
// hand-edited file. `reactions.add` takes the bare name and nothing else.
func TestAConfiguredEmojiIsNormalised(t *testing.T) {
	cfg := loadConfig(t, "reactions:\n  success: \":tada:\"\n")
	if got := cfg.ReactionEmoji(ReactionSuccess); got != "tada" {
		t.Fatalf("ReactionEmoji = %q, want the bare name", got)
	}
	if !cfg.ReactionOverridden(ReactionSuccess) {
		t.Error("a configured emoji does not read as overridden")
	}
}

// The whole set is what the state machine needs: applying one state means
// adding its emoji and removing the others.
func TestReactionEmojisReportsTheWholeSet(t *testing.T) {
	cfg := loadConfig(t, "reactions:\n  success: tada\n")
	got := cfg.ReactionEmojis()
	want := []string{DefaultAcknowledgementEmoji, DefaultDisregardEmoji, "tada", DefaultWarningEmoji}
	if len(got) != len(want) {
		t.Fatalf("ReactionEmojis = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReactionEmojis = %v, want %v", got, want)
		}
	}
}

// A hand-edited file with a bad name is refused at LOAD.
//
// The symptom otherwise is one `invalid_name` per click in a log nobody is
// reading, while the button itself appears to work — the failure mode this
// whole validation exists for.
func TestABadEmojiInTheFileIsRefusedAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("reactions:\n  success: \"two words\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a config with a bad emoji name loaded")
	}
	if !strings.Contains(err.Error(), "reactions.success") {
		t.Fatalf("err = %v, want it to name the setting", err)
	}
}

// The two mistakes somebody actually makes at the modal, both of which would
// otherwise fail hours later on somebody else's approval.
func TestValidateEmojiNameCatchesTheRealMistakes(t *testing.T) {
	// U+1F389 PARTY POPPER, written as a codepoint rather than a literal: the
	// source scan in blockkit rejects an emoji-presentation rune in any string
	// literal under internal/, and it is right to — this test is the one place
	// that needs the character itself, which is precisely the confusion the
	// validator is here to catch.
	pastedEmoji := string(rune(0x1F389))
	for _, bad := range []string{
		pastedEmoji,      // the emoji itself, not its name
		"tada party",     // more than one word
		"White_Check",    // Slack shortcodes are lower case
		"tada, confetti", // a list
	} {
		if err := ValidateEmojiName(bad); err == nil {
			t.Errorf("ValidateEmojiName(%q) = nil, want a refusal", bad)
		}
	}
	for _, good := range []string{
		"", // empty is a reset
		"tada",
		"white_check_mark",
		"+1",
		"+1::skin-tone-3",
		"custom-emoji",
		":tada:", // the colons are absorbed, not refused
	} {
		if err := ValidateEmojiName(good); err != nil {
			t.Errorf("ValidateEmojiName(%q) = %v, want it accepted", good, err)
		}
	}
}

// A save lands in the file AND in the loaded Config, so a changed tick reaches
// the next approval rather than the next restart.
func TestSetReactionWritesBothTheFileAndTheStruct(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")

	if err := cfg.SetReaction(ReactionSuccess, ":tada:"); err != nil {
		t.Fatalf("SetReaction: %v", err)
	}
	if got := cfg.ReactionEmoji(ReactionSuccess); got != "tada" {
		t.Errorf("in memory = %q, want the new emoji", got)
	}

	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("re-loading: %v", err)
	}
	if got := reloaded.ReactionEmoji(ReactionSuccess); got != "tada" {
		t.Errorf("on disk = %q, want the new emoji", got)
	}
	// Stored WITHOUT the colons, so the file says what the API takes.
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), ":tada:") {
		t.Errorf("the colons reached the file:\n%s", raw)
	}
}

// An empty name deletes the key rather than writing today's default into it, so
// "never customised" stays distinguishable from "customised to whatever the
// default said that day" — and a later change to the default reaches this
// machine.
func TestResettingAReactionDeletesTheKey(t *testing.T) {
	cfg := loadConfig(t, "reactions:\n  success: tada\n")

	if err := cfg.SetReaction(ReactionSuccess, ""); err != nil {
		t.Fatalf("SetReaction: %v", err)
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tada") {
		t.Errorf("the override survived the reset:\n%s", raw)
	}
	if cfg.ReactionOverridden(ReactionSuccess) {
		t.Error("a reset emoji still reads as overridden")
	}
	if got := cfg.ReactionEmoji(ReactionSuccess); got != DefaultSuccessEmoji {
		t.Errorf("ReactionEmoji = %q, want the default back", got)
	}
}

// A bad name is refused BEFORE the file is touched. Half a save is worse than
// none: the daemon would react with something Slack rejects until somebody
// noticed.
func TestSetReactionRefusesABadNameWithoutWriting(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")
	before, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}

	if err := cfg.SetReaction(ReactionSuccess, "two words"); err == nil {
		t.Fatal("a bad name was accepted")
	}
	after, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the file changed on a refused save:\n%s", after)
	}
}

// An id no build knows is a modal opened before an update and submitted after
// it. It is refused by name rather than panicking or writing somewhere random.
func TestAnUnknownReactionIsRefused(t *testing.T) {
	cfg := loadConfig(t, "admin:\n  slack-user-id: U1\n")
	if err := cfg.SetReaction(ReactionID("nonsense"), "tada"); err == nil {
		t.Fatal("an unknown reaction id was accepted")
	}
	if got := cfg.ReactionEmoji(ReactionID("nonsense")); got != "" {
		t.Errorf("ReactionEmoji(nonsense) = %q, want empty", got)
	}
}

// The comments in a config file are the reason it can be fixed at all, and a
// setting edited from Slack must not cost them.
func TestAReactionEditKeepsTheCommentsAndTheLayout(t *testing.T) {
	body := `# Riggs
admin:
  slack-user-id: U1

# how Riggs answers a click
reactions:
  success: tada  # the tick

# unrelated
review-request:
  channel: C1
`
	cfg := loadConfig(t, body)
	if err := cfg.SetReaction(ReactionSuccess, "heavy_check_mark"); err != nil {
		t.Fatalf("SetReaction: %v", err)
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, keep := range []string{"# Riggs", "# how Riggs answers a click", "# the tick", "# unrelated"} {
		if !strings.Contains(got, keep) {
			t.Errorf("comment %q was lost:\n%s", keep, got)
		}
	}
	if !strings.Contains(got, "heavy_check_mark") {
		t.Errorf("the new emoji is not in the file:\n%s", got)
	}
	if strings.Count(got, "\n\n") != strings.Count(body, "\n\n") {
		t.Errorf("the blank lines were reflowed:\n%s", got)
	}
}
