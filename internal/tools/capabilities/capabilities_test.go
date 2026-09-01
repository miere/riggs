package capabilities

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/notify"
)

// probes builds a Tool whose external checks are fully determined by the test.
func probes(cfg *config.Config, present map[string]string, env map[string]string) *Tool {
	return New(cfg).WithProbes(
		func(name string) (string, error) {
			if p, ok := present[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		func(k string) string { return env[k] },
	).WithLedgerProbe(func(path string) Ledger { return Ledger{Path: path} })
}

func invoke(t *testing.T, tool *Tool) Report {
	t.Helper()
	got, err := tool.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r, ok := got.(Report)
	if !ok {
		t.Fatalf("Invoke returned %T, want Report", got)
	}
	return r
}

func TestReportsReadyInstallation(t *testing.T) {
	cfg := &config.Config{
		Path:  "/tmp/config.yaml",
		Admin: config.Admin{SlackUserID: "U1", JiraEmail: "m@x"},
		Slack: config.Slack{Profiles: map[string]config.Profile{
			"default": {BotToken: "xoxb", UserToken: "xoxp"},
		}},
	}
	r := invoke(t, probes(cfg,
		map[string]string{"gh": "/opt/homebrew/bin/gh", "claude": "/usr/local/bin/claude"},
		map[string]string{"ATLASSIAN_JIRA_EMAIL": "m@x", "ATLASSIAN_JIRA_TOKEN": "t",
			"ATLASSIAN_BASE_URL": "https://example.atlassian.net"},
	))

	if len(r.Slack) != 1 || !r.Slack[0].IsDefault || r.Slack[0].Problem != "" {
		t.Errorf("slack = %+v, want one ready default profile", r.Slack)
	}
	for _, b := range r.Backends {
		if !b.Available {
			t.Errorf("backend %s unavailable (%s), want all ready", b.Name, b.Detail)
		}
	}
}

// The report never echoes the admin's actual values, so it is safe to paste
// into a thread.
func TestAdminValuesAreNotEchoed(t *testing.T) {
	cfg := &config.Config{Admin: config.Admin{SlackUserID: "U0SECRET", JiraEmail: "miere@nurturecloud.com"}}
	r := invoke(t, probes(cfg, nil, nil))
	rendered := r.String()
	if strings.Contains(rendered, "U0SECRET") || strings.Contains(rendered, "nurturecloud.com") {
		t.Errorf("report echoes configured values:\n%s", rendered)
	}
}

// This is the tool's whole purpose: naming what is missing.
func TestNamesWhatIsMissing(t *testing.T) {
	cfg := &config.Config{
		Path: "/tmp/config.yaml",
		Slack: config.Slack{Profiles: map[string]config.Profile{
			"default": {BotToken: ""}, // ${ENV} expanded to nothing
		}},
	}
	r := invoke(t, probes(cfg, nil, map[string]string{"ATLASSIAN_JIRA_EMAIL": "m@x"}))

	if r.Slack[0].Problem == "" {
		t.Error("a profile with an empty bot-token reported no problem")
	}
	byName := map[string]Backend{}
	for _, b := range r.Backends {
		byName[b.Name] = b
	}
	if byName["gh"].Available {
		t.Error("gh reported available with no binary on PATH")
	}
	if got := byName["jira"]; got.Available || !strings.Contains(got.Detail, "ATLASSIAN_JIRA_TOKEN") {
		t.Errorf("jira = %+v, want it disabled naming the one missing variable", got)
	}
}

func TestRendersEmptyProfileSet(t *testing.T) {
	r := invoke(t, probes(&config.Config{Path: "<no config file>"}, nil, nil))
	if !strings.Contains(r.String(), "none configured") {
		t.Errorf("report does not explain an empty profile set:\n%s", r.String())
	}
}

// The ledger is reported without being provisioned: running a diagnostic must
// not create the state it is diagnosing.
func TestLedgerProbeDoesNotCreateTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")

	got := readLedger(path)
	if got.Exists || got.Cards != 0 || got.Problem != "" {
		t.Errorf("ledger = %+v, want it reported absent", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("readLedger created the ledger file as a side effect")
	}
	if !strings.Contains(Report{Ledger: got}.String(), "not created yet") {
		t.Error("an absent ledger is not explained in the rendered report")
	}
}

// An existing ledger is read and counted.
func TestLedgerProbeCountsCards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	store, err := notify.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.SaveCard(context.Background(), "o/r#1", notify.Entry{
		Profile: "default", Channel: "C1", TS: "1700.1", Fingerprint: "abc", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveCard: %v", err)
	}
	store.Close()

	got := readLedger(path)
	if !got.Exists || got.Cards != 1 || got.Problem != "" {
		t.Errorf("ledger = %+v, want one tracked card", got)
	}
	if !strings.Contains(Report{Ledger: got}.String(), "1 cards tracked") {
		t.Errorf("card count missing from the report:\n%s", Report{Ledger: got}.String())
	}
}

// Every one of the four verbs is optional now, and a row quietly missing an
// option is exactly the absence this tool exists to explain.
func TestReportNamesTheSettingBehindEveryAction(t *testing.T) {
	cfg := &config.Config{}
	report := invoke(t, probes(cfg, nil, nil))

	if len(report.Actions) != 4 {
		t.Fatalf("actions = %d, want the four verbs", len(report.Actions))
	}
	want := map[string]string{
		"Ask for Code Review":    "review-request.user-id",
		"Ask for SME Assistance": "sme-assistance.user-id",
		"Run Code Review":        "ai.command",
		"Run AI Assistance":      "ai.command",
	}
	for _, a := range report.Actions {
		setting, known := want[a.Name]
		if !known {
			t.Fatalf("unexpected action %q", a.Name)
		}
		if a.Enabled {
			t.Errorf("%s is enabled on an empty config", a.Name)
		}
		if !strings.Contains(a.Detail, setting) {
			t.Errorf("%s: detail = %q, want it to name %s", a.Name, a.Detail, setting)
		}
	}
}

// Asking a person and running a harness are separately configured, and the
// report has to show that rather than collapsing them into "assistance: on".
func TestReportSeparatesAskingFromRunning(t *testing.T) {
	cfg := &config.Config{
		ReviewRequest: config.ReviewRequest{UserID: "U-someone"},
		AI:            config.AI{Command: "claude", WorkDir: "/work"},
	}
	report := invoke(t, probes(cfg, nil, nil))

	byName := map[string]Action{}
	for _, a := range report.Actions {
		byName[a.Name] = a
	}
	if !byName["Ask for Code Review"].Enabled {
		t.Error("a configured reviewer did not enable the ask")
	}
	if byName["Ask for SME Assistance"].Enabled {
		t.Error("the ticket ask was enabled by the pull-request setting")
	}
	if !byName["Run Code Review"].Enabled || !byName["Run AI Assistance"].Enabled {
		t.Error("one harness did not enable both Run options")
	}
	if !strings.Contains(byName["Run Code Review"].Detail, "/work") {
		t.Errorf("the harness detail does not say where it runs: %q", byName["Run Code Review"].Detail)
	}

	// A misspelled harness renders the options and fails every run, which is
	// invisible until somebody clicks — so it is probed by name.
	var found bool
	for _, be := range report.Backends {
		if be.Name == "claude" {
			found = true
			if be.Available {
				t.Error("claude was reported as available by a probe that finds nothing")
			}
		}
	}
	if !found {
		t.Errorf("the harness binary was not probed: %+v", report.Backends)
	}
}

// The retired section name still works, and saying so once beats every reader
// of the file having to work it out.
func TestReportNotesTheRetiredSectionName(t *testing.T) {
	cfg, err := config.Load(writeTempConfig(t, "ai-assistance:\n  user-id: U-expert\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	report := invoke(t, probes(cfg, nil, nil))
	if len(report.Notes) == 0 || !strings.Contains(report.Notes[0], "sme-assistance") {
		t.Fatalf("notes = %v", report.Notes)
	}
	if !strings.Contains(report.String(), "note:") {
		t.Fatalf("the rendered report does not carry the note:\n%s", report.String())
	}
}

// writeTempConfig lays a config file down and returns its path.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}
