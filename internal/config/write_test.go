package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig lays a config file down and loads it, returning both.
func loadFrom(t *testing.T, body string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func readBack(t *testing.T, cfg *Config) string {
	t.Helper()
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatalf("reading back %s: %v", cfg.Path, err)
	}
	return string(data)
}

// The whole reason this is textual surgery rather than a marshal. The loaded
// Config holds EXPANDED tokens; re-marshalling it would write a live bot token
// into the file as a side effect of somebody rewording a prompt.
func TestSetPromptNeverWritesTheExpandedSecrets(t *testing.T) {
	t.Setenv("RIGGS_TEST_BOT", "xoxb-a-real-token")
	cfg := loadFrom(t, `version: 1

slack:
  profiles:
    default:
      bot-token: ${RIGGS_TEST_BOT}

review-request:
  user-id: "U-someone"
  prompt: "old wording"
`)
	// The loaded value really is the secret, so the test is about the file
	// rather than about the reference never having been resolved.
	if cfg.Slack.Profiles[DefaultProfile].BotToken != "xoxb-a-real-token" {
		t.Fatal("the token was not expanded, so this proves nothing")
	}

	if err := cfg.SetPrompt(PromptReviewRequest, "new wording"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	got := readBack(t, cfg)
	if strings.Contains(got, "xoxb-a-real-token") {
		t.Fatalf("the expanded token was written to disk:\n%s", got)
	}
	if !strings.Contains(got, "bot-token: ${RIGGS_TEST_BOT}") {
		t.Fatalf("the ${ENV} reference did not survive:\n%s", got)
	}
	if !strings.Contains(got, `prompt: "new wording"`) {
		t.Fatalf("the prompt was not written:\n%s", got)
	}
}

// A config you cannot read is a config you cannot fix, so an edit must leave
// the comments and the layout exactly as they were.
func TestSetPromptKeepsCommentsAndBlankLines(t *testing.T) {
	body := `# Riggs configuration.
version: 1

# Who assists with pull requests.
review-request:
  # Who is tagged.
  user-id: "U-someone"
  prompt: "old wording"

# Atlassian.
jira:
  base-url: https://example.atlassian.net
`
	cfg := loadFrom(t, body)
	if err := cfg.SetPrompt(PromptReviewRequest, "new wording"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	want := strings.Replace(body, `  prompt: "old wording"`, `  prompt: "new wording"`, 1)
	if got := readBack(t, cfg); got != want {
		t.Fatalf("the file changed beyond the one line:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// The key is absent far more often than it is present: a prompt on its default
// is not written at all.
func TestSetPromptInsertsAMissingKey(t *testing.T) {
	cfg := loadFrom(t, "version: 1\n\nreview-request:\n  user-id: \"U-someone\"\n")
	if err := cfg.SetPrompt(PromptReviewRequest, "new wording"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	got := readBack(t, cfg)
	if !strings.Contains(got, `  prompt: "new wording"`) {
		t.Fatalf("the key was not inserted:\n%s", got)
	}
	if !strings.Contains(got, `  user-id: "U-someone"`) {
		t.Fatalf("the sibling key was lost:\n%s", got)
	}
	// It must still load — and still be the prompt it claims to be.
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.ReviewPrompt() != "new wording" {
		t.Fatalf("reloaded prompt = %q", reloaded.ReviewPrompt())
	}
}

// The `ai` section does not exist on a machine that never configured a harness,
// and the Home tab can still be used to write its prompt.
func TestSetPromptCreatesAMissingSection(t *testing.T) {
	cfg := loadFrom(t, "version: 1\n\nadmin:\n  slack-user-id: U-admin\n")
	if err := cfg.SetPrompt(PromptAIReview, "review {ref} carefully"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, readBack(t, cfg))
	}
	if reloaded.AIReviewPrompt() != "review {ref} carefully" {
		t.Fatalf("reloaded prompt = %q\n%s", reloaded.AIReviewPrompt(), readBack(t, cfg))
	}
	if reloaded.Admin.SlackUserID != "U-admin" {
		t.Fatal("the existing section was lost")
	}
}

// A section declared with nothing under it parses as null and has no mapping to
// insert into. Appending a second `ai:` would make the file unloadable, which
// is a worse outcome than the edit failing.
func TestSetPromptFillsAnEmptySection(t *testing.T) {
	cfg := loadFrom(t, "version: 1\n\nai:\n")
	if err := cfg.SetPrompt(PromptAIAssist, "scope {key}"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, readBack(t, cfg))
	}
	if reloaded.AIAssistPrompt() != "scope {key}" {
		t.Fatalf("reloaded prompt = %q\n%s", reloaded.AIAssistPrompt(), readBack(t, cfg))
	}
}

// Reset deletes the key rather than writing the default's own words, so a later
// change to the default reaches a machine that never had an opinion.
func TestSetPromptWithEmptyTextDeletesTheKey(t *testing.T) {
	cfg := loadFrom(t, "version: 1\n\nai:\n  command: claude\n  ticket-prompt: \"mine\"\n")
	if !cfg.PromptOverridden(PromptAIAssist) {
		t.Fatal("the seeded override was not read")
	}
	if err := cfg.SetPrompt(PromptAIAssist, ""); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	got := readBack(t, cfg)
	if strings.Contains(got, "ticket-prompt") {
		t.Fatalf("the key survived a reset:\n%s", got)
	}
	if !strings.Contains(got, "command: claude") {
		t.Fatalf("a sibling key was taken with it:\n%s", got)
	}
	if cfg.PromptOverridden(PromptAIAssist) {
		t.Fatal("the in-memory Config still reports an override")
	}
	if cfg.AIAssistPrompt() != DefaultAIAssistPrompt {
		t.Fatalf("AIAssistPrompt = %q, want the default back", cfg.AIAssistPrompt())
	}
}

// A block scalar spans several lines. Replacing only the first would leave its
// body behind as a stray mapping and break the file.
func TestSetPromptReplacesABlockScalar(t *testing.T) {
	cfg := loadFrom(t, `version: 1

ai:
  command: claude
  pull-request-prompt: |
    review {ref}
    and be thorough
  workdir: /home/m
`)
	if err := cfg.SetPrompt(PromptAIReview, "one line now"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	got := readBack(t, cfg)
	if strings.Contains(got, "be thorough") {
		t.Fatalf("the block scalar's body was left behind:\n%s", got)
	}
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, got)
	}
	if reloaded.AIReviewPrompt() != "one line now" {
		t.Fatalf("reloaded prompt = %q", reloaded.AIReviewPrompt())
	}
	if reloaded.AIWorkDir() != "/home/m" {
		t.Fatalf("the following key was lost: %q", reloaded.AIWorkDir())
	}
}

// A prompt typed into a multiline modal arrives with newlines in it, and prose
// routinely carries a colon or a leading brace. Every one of those changes what
// a plain scalar means, which is why the writer quotes unconditionally.
func TestSetPromptSurvivesAwkwardText(t *testing.T) {
	awkward := "Review {ref}: be careful.\n\"Quote\" it, # comment it, \\ escape it."
	cfg := loadFrom(t, "version: 1\n\nai:\n  command: claude\n")
	if err := cfg.SetPrompt(PromptAIReview, awkward); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	reloaded, err := Load(cfg.Path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, readBack(t, cfg))
	}
	if reloaded.AIReviewPrompt() != awkward {
		t.Fatalf("round trip lost text:\ngot  %q\nwant %q", reloaded.AIReviewPrompt(), awkward)
	}
}

// The file may hold literal tokens. A prompt edit must not be what makes it
// world-readable.
func TestSetPromptKeepsTheFilePrivate(t *testing.T) {
	cfg := loadFrom(t, "version: 1\n\nai:\n  command: claude\n")
	if err := cfg.SetPrompt(PromptAIReview, "new"); err != nil {
		t.Fatalf("SetPrompt: %v", err)
	}
	info, err := os.Stat(cfg.Path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

// There is nothing to edit on a machine that was never provisioned, and saying
// so beats creating a config nobody asked for in a directory nobody chose.
func TestSetPromptRefusesWithoutAConfigFile(t *testing.T) {
	cfg := &Config{Path: NoFilePath}
	if err := cfg.SetPrompt(PromptAIReview, "new"); err == nil {
		t.Fatal("SetPrompt wrote something with no config file")
	}
	if err := (&Config{Path: "/tmp/x.yaml"}).SetPrompt("not_a_prompt", "new"); err == nil {
		t.Fatal("SetPrompt accepted an unknown prompt id")
	}
}
