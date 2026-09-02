package app

import (
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
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

// configured reports whether the ticket digest was built for cfg.
func configured(cfg *config.Config) bool { return ticketDigest(cfg) != nil }

func TestJiraToolsNeedATenant(t *testing.T) {
	if configured(jiraCfg(t, "")) {
		t.Fatal("the ticket digest was built with no tenant configured")
	}
}

func TestJiraToolsRegisterWithATenant(t *testing.T) {
	if !configured(jiraCfg(t, "https://example.atlassian.net")) {
		t.Error("the ticket digest was not built with a tenant configured")
	}
}

// The tenant may come from the environment, on the same precedence as the
// credentials beside it.
func TestJiraTenantMayComeFromTheEnvironment(t *testing.T) {
	cfg := jiraCfg(t, "")
	t.Setenv(config.JiraBaseURLEnv, "https://from-env.atlassian.net")

	if !configured(cfg) {
		t.Fatal("the digest was not built from an environment-supplied tenant")
	}
}
