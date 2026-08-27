// Package capabilities reports what this installation can actually do, and
// names precisely what is missing when it cannot.
//
// It exists because Riggs disables features rather than failing to boot: a
// tool whose credential is absent is simply not there. That is the right
// behaviour for an unattended binary, but it makes "why is my tool missing?"
// unanswerable by reading the source — so this tool answers it instead. It is
// the analogue of the blueprint's `catalogue` decisions.
package capabilities

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/notify"
)

// Tool reports the configured capabilities.
type Tool struct {
	cfg *config.Config
	// lookPath is the executable probe, injected so tests do not depend on
	// what happens to be installed on the machine running them.
	lookPath func(string) (string, error)
	// getenv is the environment probe, injected for the same reason.
	getenv func(string) string
	// ledger reads the notification store without creating it.
	ledger func(path string) Ledger
}

// New constructs the capabilities tool over the loaded configuration.
func New(cfg *config.Config) *Tool {
	return &Tool{cfg: cfg, lookPath: exec.LookPath, getenv: os.Getenv, ledger: readLedger}
}

// readLedger inspects the ledger without provisioning it: an absent file is
// reported as absent, not created.
func readLedger(path string) Ledger {
	l := Ledger{Path: path}
	if _, err := os.Stat(path); err != nil {
		return l
	}
	l.Exists = true
	store, err := notify.Open(path)
	if err != nil {
		l.Problem = err.Error()
		return l
	}
	defer store.Close()
	n, err := store.CountCards(context.Background())
	if err != nil {
		l.Problem = err.Error()
		return l
	}
	l.Cards = n
	return l
}

// WithLedgerProbe overrides the ledger reader; for tests.
func (t *Tool) WithLedgerProbe(f func(string) Ledger) *Tool {
	t.ledger = f
	return t
}

// WithProbes overrides the executable and environment probes; for tests.
func (t *Tool) WithProbes(lookPath func(string) (string, error), getenv func(string) string) *Tool {
	t.lookPath, t.getenv = lookPath, getenv
	return t
}

// Name is the registry key.
func (t *Tool) Name() string { return "capabilities" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	return "Report which Riggs features are enabled, and what is missing for the rest."
}

// InputSchema is nil: capabilities takes no parameters.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Report is the result shape. The CLI renders it via String(); the MCP
// frontend marshals it as JSON.
type Report struct {
	ConfigPath string    `json:"config_path"`
	EnvFile    EnvFile   `json:"env_file"`
	Ledger     Ledger    `json:"ledger"`
	Admin      Admin     `json:"admin"`
	Slack      []Profile `json:"slack_profiles"`
	Backends   []Backend `json:"backends"`
}

// EnvFile reports which dotenv file supplied the ${VAR} references, and
// whether it was there.
//
// It is reported because the symptom of the wrong one is an *empty token*,
// which surfaces as "profile has no bot-token" — a message that says nothing
// about where the token was looked for. That is the first question anyone
// debugging a launch agent asks, since the agent inherits no environment of its
// own (§12b).
type EnvFile struct {
	Path   string `json:"path"`
	Loaded bool   `json:"loaded"`
}

// Ledger reports the notification store: where it is, and what it holds.
//
// It is read without creating anything. A diagnostic that provisions state as
// a side effect of being run is a diagnostic you cannot trust.
type Ledger struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Cards  int    `json:"cards"`
	// Problem is set when the file is there but could not be read.
	Problem string `json:"problem,omitempty"`
}

// Admin mirrors the configured admin identity, reporting only whether each
// field is set — never the values, so the report is safe to paste anywhere.
type Admin struct {
	SlackUserID string `json:"slack_user_id"`
	JiraEmail   string `json:"jira_email"`
}

// Profile is one configured Slack account's readiness.
type Profile struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
	BotToken  bool   `json:"bot_token"`
	UserToken bool   `json:"user_token"`
	// Problem names what stops this profile from delivering, empty when it is
	// ready.
	Problem string `json:"problem,omitempty"`
}

// Backend is an external dependency Riggs delegates to.
type Backend struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
}

// Invoke builds the report.
func (t *Tool) Invoke(context.Context, map[string]any) (any, error) {
	r := Report{
		ConfigPath: t.cfg.Path,
		EnvFile:    EnvFile{Path: t.cfg.EnvPath(), Loaded: t.cfg.EnvLoaded()},
		Ledger:     t.ledger(t.cfg.DBPath()),
		Admin: Admin{
			SlackUserID: set(t.cfg.Admin.SlackUserID),
			JiraEmail:   set(t.cfg.Admin.JiraEmail),
		},
		Slack:    t.slackProfiles(),
		Backends: t.backends(),
	}
	return r, nil
}

// slackProfiles reports each configured profile in a stable order.
func (t *Tool) slackProfiles() []Profile {
	names := t.cfg.ProfileNames()
	sort.Strings(names)
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		p := t.cfg.Slack.Profiles[name]
		entry := Profile{
			Name:      name,
			IsDefault: name == config.DefaultProfile,
			BotToken:  strings.TrimSpace(p.BotToken) != "",
			UserToken: strings.TrimSpace(p.UserToken) != "",
		}
		if !entry.BotToken {
			entry.Problem = "bot-token is empty (check its ${ENV} reference)"
		}
		out = append(out, entry)
	}
	return out
}

// backends probes the external dependencies. `gh` is the GitHub *credential*
// provider — Riggs makes the HTTP calls itself — and card summaries shell out
// Nothing else is shelled out to: card bodies are derived from the description
// now rather than summarised by an LLM (§7d).
func (t *Tool) backends() []Backend {
	out := []Backend{t.binary("gh", "the GitHub token comes from the authenticated gh CLI")}

	email := t.getenv(config.JiraEmailEnv)
	token := t.getenv(config.JiraTokenEnv)
	// The tenant resolves config-first, mirroring Config.JiraBaseURL. It is
	// reported alongside the credentials because there is no default: an
	// unconfigured tenant disables Jira exactly as a missing token does, and
	// "which Jira?" is not a question the error at call time can answer.
	baseURL := strings.TrimSpace(t.cfg.Jira.BaseURL)
	if baseURL == "" {
		baseURL = strings.TrimSpace(t.getenv(config.JiraBaseURLEnv))
	}

	jira := Backend{Name: "jira", Available: email != "" && token != "" && baseURL != ""}
	var missing []string
	if email == "" {
		missing = append(missing, config.JiraEmailEnv)
	}
	if token == "" {
		missing = append(missing, config.JiraTokenEnv)
	}
	if baseURL == "" {
		missing = append(missing, "jira.base-url (or "+config.JiraBaseURLEnv+")")
	}
	if jira.Available {
		jira.Detail = "tenant " + baseURL
	} else {
		jira.Detail = "set " + strings.Join(missing, ", ")
	}
	return append(out, jira)
}

// binary probes for an executable on PATH.
func (t *Tool) binary(name, why string) Backend {
	path, err := t.lookPath(name)
	if err != nil {
		return Backend{Name: name, Available: false, Detail: "not on PATH; " + why}
	}
	return Backend{Name: name, Available: true, Detail: path}
}

// set reports a configured value as "set" or "unset" without echoing it.
func set(v string) string {
	if strings.TrimSpace(v) == "" {
		return "unset"
	}
	return "set"
}

// String renders the report for a human reading the CLI.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config: %s\n", r.ConfigPath)
	switch {
	case r.EnvFile.Path == "":
		b.WriteString("env:    (none — variables come from the environment)\n")
	case r.EnvFile.Loaded:
		fmt.Fprintf(&b, "env:    %s\n", r.EnvFile.Path)
	default:
		fmt.Fprintf(&b, "env:    %s (not found — variables come from the environment)\n", r.EnvFile.Path)
	}
	switch {
	case r.Ledger.Problem != "":
		fmt.Fprintf(&b, "ledger: %s — unreadable: %s\n", r.Ledger.Path, r.Ledger.Problem)
	case r.Ledger.Exists:
		fmt.Fprintf(&b, "ledger: %s (%d cards tracked)\n", r.Ledger.Path, r.Ledger.Cards)
	default:
		fmt.Fprintf(&b, "ledger: %s (not created yet)\n", r.Ledger.Path)
	}
	fmt.Fprintf(&b, "\nadmin:\n  slack-user-id: %s\n  jira-email:    %s\n",
		r.Admin.SlackUserID, r.Admin.JiraEmail)

	b.WriteString("\nslack profiles:\n")
	if len(r.Slack) == 0 {
		b.WriteString("  (none configured — every notifying tool is disabled)\n")
	}
	for _, p := range r.Slack {
		mark := "ok"
		if p.Problem != "" {
			mark = "!!"
		}
		fmt.Fprintf(&b, "  [%s] %s", mark, p.Name)
		if p.IsDefault {
			b.WriteString(" (default)")
		}
		if p.UserToken {
			b.WriteString(" +user-token")
		}
		if p.Problem != "" {
			fmt.Fprintf(&b, " — %s", p.Problem)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nbackends:\n")
	for _, be := range r.Backends {
		mark := "ok"
		if !be.Available {
			mark = "!!"
		}
		fmt.Fprintf(&b, "  [%s] %-7s %s\n", mark, be.Name, be.Detail)
	}
	return strings.TrimRight(b.String(), "\n")
}
