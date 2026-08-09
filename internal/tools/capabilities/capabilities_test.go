package capabilities

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
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
	)
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
		Admin: config.Admin{SlackUserID: "U1", JiraEmail: "m@x", GitHubLogin: "miere"},
		Slack: config.Slack{Profiles: map[string]config.Profile{
			"default": {BotToken: "xoxb", UserToken: "xoxp"},
		}},
	}
	r := invoke(t, probes(cfg,
		map[string]string{"gh": "/opt/homebrew/bin/gh", "claude": "/usr/local/bin/claude"},
		map[string]string{"ATLASSIAN_JIRA_EMAIL": "m@x", "ATLASSIAN_JIRA_TOKEN": "t"},
	))

	if len(r.Slack) != 1 || !r.Slack[0].IsDefault || r.Slack[0].Problem != "" {
		t.Errorf("slack = %+v, want one ready default profile", r.Slack)
	}
	for _, b := range r.Backends {
		if !b.Available {
			t.Errorf("backend %s unavailable (%s), want all ready", b.Name, b.Detail)
		}
	}
	if r.Admin.GitHubLogin != "set" {
		t.Errorf("admin.github_login = %q, want \"set\"", r.Admin.GitHubLogin)
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
	if r.Admin.GitHubLogin != "unset" {
		t.Errorf("admin.github_login = %q, want \"unset\"", r.Admin.GitHubLogin)
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
