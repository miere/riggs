package app

import (
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/tools"
)

// jiraCfg builds a config with credentials, and whatever tenant is given.
func jiraCfg(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	// Cleared so an ambient ATLASSIAN_BASE_URL on the developer's machine
	// cannot decide the outcome of this test.
	t.Setenv(config.JiraBaseURLEnv, "")
	t.Setenv(config.JiraEmailEnv, "")
	t.Setenv(config.JiraTokenEnv, "")

	return &config.Config{
		Path:  "/tmp/config.yaml",
		Admin: config.Admin{SlackUserID: "U1", JiraEmail: "m@x"},
		Jira:  config.Jira{Email: "m@x", Token: "tok", BaseURL: baseURL},
	}
}

func registeredNames(cfg *config.Config) map[string]bool {
	reg := tools.NewRegistry()
	registerJiraTools(reg, cfg, slack.NewResolver(cfg))
	names := map[string]bool{}
	for _, tool := range reg.All() {
		names[tool.Name()] = true
	}
	return names
}

// There is no default tenant, so credentials alone are not enough. Registering
// these against a baked-in Atlassian instance would mean a misconfigured
// machine quietly reading and assigning tickets on somebody else's Jira.
func TestJiraToolsNeedATenant(t *testing.T) {
	if names := registeredNames(jiraCfg(t, "")); len(names) != 0 {
		t.Fatalf("registered %v with no tenant configured", names)
	}
}

func TestJiraToolsRegisterWithATenant(t *testing.T) {
	names := registeredNames(jiraCfg(t, "https://example.atlassian.net"))
	for _, want := range []string{"jira.tickets.poll", "jira.tickets.assign"} {
		if !names[want] {
			t.Errorf("%s was not registered: got %v", want, names)
		}
	}
}

// The tenant may come from the environment, on the same precedence as the
// credentials beside it.
func TestJiraTenantMayComeFromTheEnvironment(t *testing.T) {
	cfg := jiraCfg(t, "")
	t.Setenv(config.JiraBaseURLEnv, "https://from-env.atlassian.net")

	if !registeredNames(cfg)["jira.tickets.poll"] {
		t.Fatal("tools were not registered from an environment-supplied tenant")
	}
}
