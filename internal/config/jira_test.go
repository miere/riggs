package config

import (
	"strings"
	"testing"
)

// The bug this file exists for: base-url was the one Atlassian setting expand()
// never touched, so a ${VAR} reference reached the HTTP client verbatim and
// every request went to "${ATLASSIAN_BASE_URL}/rest/api/3/...".
func TestJiraBaseURLIsExpanded(t *testing.T) {
	t.Setenv("TEST_JIRA_TENANT", "https://example.atlassian.net")

	cfg, err := parse("/tmp/riggs/config.yaml", []byte(
		"jira:\n  base-url: ${TEST_JIRA_TENANT}\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Jira.BaseURL != "https://example.atlassian.net" {
		t.Fatalf("Jira.BaseURL = %q, want it expanded", cfg.Jira.BaseURL)
	}
	if cfg.JiraBaseURL() != "https://example.atlassian.net" {
		t.Fatalf("JiraBaseURL() = %q", cfg.JiraBaseURL())
	}
}

// Same precedence as the credentials beside it: config, then environment, then
// empty so the client applies its own default.
func TestJiraBaseURLPrecedence(t *testing.T) {
	t.Setenv(JiraBaseURLEnv, "https://from-env.atlassian.net")

	cfg := &Config{Jira: Jira{BaseURL: "https://from-config.atlassian.net"}}
	if got := cfg.JiraBaseURL(); got != "https://from-config.atlassian.net" {
		t.Fatalf("JiraBaseURL = %q, want the config value", got)
	}

	if got := (&Config{}).JiraBaseURL(); got != "https://from-env.atlassian.net" {
		t.Fatalf("JiraBaseURL = %q, want the environment value", got)
	}
}

// The tenant is never required — it has a working default, which is why an
// unconfigured machine talks to Jira at all.
func TestJiraBaseURLIsOptional(t *testing.T) {
	t.Setenv(JiraBaseURLEnv, "")
	if got := (&Config{}).JiraBaseURL(); got != "" {
		t.Fatalf("JiraBaseURL = %q, want empty so the client defaults", got)
	}
}

// A tenant that is not an absolute URL produces requests to a nonsense address
// and an error naming neither this setting nor this file. A missing scheme is
// the likely typo.
//
// An unexpanded ${VAR} cannot reach here: expansion runs first and an unset
// variable becomes empty, which falls through to the default — see
// TestUnsetBaseURLReferenceFallsThrough.
func TestJiraBaseURLMustBeAbsolute(t *testing.T) {
	for _, bad := range []string{
		"vmxproperty.atlassian.net",
		"/rest/api/3",
		"ftp://example.atlassian.net",
	} {
		_, err := parse("/tmp/riggs/config.yaml", []byte("jira:\n  base-url: '"+bad+"'\n"))
		if err == nil {
			t.Errorf("parse accepted base-url %q", bad)
			continue
		}
		if !strings.Contains(err.Error(), "base-url") {
			t.Errorf("error for %q does not name the setting: %v", bad, err)
		}
	}

	for _, good := range []string{
		"https://example.atlassian.net",
		"http://localhost:8080",
		"https://example.atlassian.net/",
	} {
		if _, err := parse("/tmp/riggs/config.yaml", []byte("jira:\n  base-url: '"+good+"'\n")); err != nil {
			t.Errorf("parse rejected base-url %q: %v", good, err)
		}
	}
}

// An unset reference expands to empty, which must fall through to the default
// rather than trip the absolute-URL guard.
func TestUnsetBaseURLReferenceFallsThrough(t *testing.T) {
	t.Setenv(JiraBaseURLEnv, "")
	cfg, err := parse("/tmp/riggs/config.yaml", []byte(
		"jira:\n  base-url: ${TEST_JIRA_TENANT_NOT_SET}\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.JiraBaseURL(); got != "" {
		t.Fatalf("JiraBaseURL = %q, want empty", got)
	}
}

func TestJiraCredentialsFallBackToTheEnvironment(t *testing.T) {
	t.Setenv(JiraEmailEnv, "env@example.com")
	t.Setenv(JiraTokenEnv, "env-token")

	email, token := (&Config{}).JiraCredentials()
	if email != "env@example.com" || token != "env-token" {
		t.Fatalf("credentials = %q / %q", email, token)
	}

	cfg := &Config{Jira: Jira{Email: "cfg@example.com", Token: "cfg-token"}}
	if email, token := cfg.JiraCredentials(); email != "cfg@example.com" || token != "cfg-token" {
		t.Fatalf("credentials = %q / %q, want the config values", email, token)
	}
}
