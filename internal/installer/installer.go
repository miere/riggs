// Package installer provisions a working Riggs: it writes the config, proves
// the Slack path end to end against real data, and — if Murtaugh is present —
// registers the scheduled jobs.
//
// It is interactive, so it lives outside the tool registry and is never
// exposed over MCP. `riggs install` is handled in cmd/riggs before mode
// parsing, the same treatment the blueprint gives its `auth` command.
//
// Murtaugh is configured exclusively through its CLI (`murtaugh cfg job set`).
// Riggs never writes Murtaugh's database: that command re-validates the whole
// assembled config and rolls back a change that would break it, which a
// hand-written row would bypass.
package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/slack"
)

// PRLister is the GitHub seam used by the smoke test.
type PRLister interface {
	ReviewRequested(ctx context.Context, login string, limit int) ([]github.PullRequest, error)
}

// CommandRunner executes an external command, returning its combined output.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Options carries what only the composition root knows.
type Options struct {
	// RiggsPath is the absolute path to this binary, used as the job command.
	RiggsPath string
	// ToolsFor reports which tools this binary exposes under the config at the
	// given path. It is a callback rather than a set because the answer
	// depends on the config the installer has just written — and a job whose
	// tool does not exist would otherwise be registered to fail every minute.
	ToolsFor func(configPath string) (map[string]bool, error)
}

// Installer runs the interactive flow.
type Installer struct {
	p    Prompter
	opts Options

	ghAuth   func(ctx context.Context) (github.Auth, error)
	newPRs   func(token string) PRLister
	poster   slack.Poster
	runCmd   CommandRunner
	getenv   func(string) string
	writeCfg func(path string, data []byte) error
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
}

// New builds an Installer with live dependencies.
func New(p Prompter, opts Options) *Installer {
	return &Installer{
		p:      p,
		opts:   opts,
		ghAuth: func(ctx context.Context) (github.Auth, error) { return github.AuthFromGH(ctx, nil) },
		newPRs: func(token string) PRLister { return github.New(token) },
		poster: slack.NewAPI(),
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, errOut, err := github.ExecRunner(ctx, name, args...)
			return append(out, errOut...), err
		},
		getenv: os.Getenv,
		writeCfg: func(path string, data []byte) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			// 0600: this file may hold literal tokens.
			return os.WriteFile(path, data, 0o600)
		},
		lookPath: exec.LookPath,
		stat:     os.Stat,
	}
}

// Run executes the whole flow. Any error aborts the install.
func (i *Installer) Run(ctx context.Context) error {
	i.p.Say("Riggs installer")
	i.p.Say("")

	path, err := i.configLocation()
	if err != nil {
		return err
	}
	cfg, err := i.gather(ctx)
	if err != nil {
		return err
	}
	if err := i.write(path, cfg); err != nil {
		return err
	}
	i.p.Say("")
	i.p.Say("Wrote %s (mode 0600).", path)

	if err := i.smokeTest(ctx, cfg); err != nil {
		return err
	}
	if err := i.wireMurtaugh(ctx, cfg, path); err != nil {
		return err
	}

	i.p.Say("")
	i.p.Say("Done. `riggs capabilities` will show what is live.")
	return nil
}

// configLocation asks where the config goes, refusing to clobber an existing
// file without consent.
func (i *Installer) configLocation() (string, error) {
	path, err := i.p.Ask("Where should the Riggs config live?", config.DefaultPath())
	if err != nil {
		return "", err
	}
	path = expandHome(path)
	if _, err := i.stat(path); err == nil {
		overwrite, err := i.p.Confirm(fmt.Sprintf("%s already exists. Overwrite it?", path), false)
		if err != nil {
			return "", err
		}
		if !overwrite {
			return "", errors.New("install cancelled: the existing config was kept")
		}
	}
	return path, nil
}

// gather collects the identity and credentials.
func (i *Installer) gather(ctx context.Context) (*config.Config, error) {
	cfg := &config.Config{Version: 1, Slack: config.Slack{Profiles: map[string]config.Profile{}}}

	// The GitHub login is the one thing we can discover rather than ask for.
	ghLogin := ""
	if auth, err := i.ghAuth(ctx); err == nil {
		ghLogin = auth.Login
	} else {
		i.p.Say("Note: %v", err)
		i.p.Say("      GitHub features stay disabled until `gh auth login` is done.")
	}

	i.p.Say("")
	i.p.Say("Who is Riggs acting for?")
	login, err := i.p.Ask("  GitHub login", ghLogin)
	if err != nil {
		return nil, err
	}
	slackID, err := i.p.Ask("  Slack user id (the DM target when a job names no channel)", "")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(slackID) == "" {
		return nil, errors.New("a Slack user id is required: without it there is nobody to DM")
	}
	jiraEmail, err := i.p.Ask("  Jira email", i.getenv("ATLASSIAN_JIRA_EMAIL"))
	if err != nil {
		return nil, err
	}
	cfg.Admin = config.Admin{GitHubLogin: login, SlackUserID: slackID, JiraEmail: jiraEmail}

	i.p.Say("")
	i.p.Say("Slack credentials for the \"default\" profile.")
	i.p.Say("Paste a token, or ${ENV_VAR} to reference the environment instead.")
	bot, err := i.secret("  Bot token (xoxb-)", "SLACK_BOT_TOKEN")
	if err != nil {
		return nil, err
	}
	if bot == "" {
		return nil, errors.New("a bot token is required: without it nothing can be posted")
	}
	user, err := i.secret("  User token (xoxp-, optional)", "SLACK_USER_TOKEN")
	if err != nil {
		return nil, err
	}
	cfg.Slack.Profiles[config.DefaultProfile] = config.Profile{BotToken: bot, UserToken: user}

	i.p.Say("")
	i.p.Say("Jira credentials (used by the ticket jobs).")
	jiraToken, err := i.secret("  API token (optional)", "ATLASSIAN_JIRA_TOKEN")
	if err != nil {
		return nil, err
	}
	cfg.Jira = config.Jira{Email: jiraEmail, Token: jiraToken}

	return cfg, nil
}

// secret prompts for a credential. An empty answer adopts a ${ENV} reference
// when that variable is already populated, which is how an existing machine
// keeps its secrets in the environment rather than in the file.
func (i *Installer) secret(question, envVar string) (string, error) {
	if i.getenv(envVar) != "" {
		question += fmt.Sprintf(" [empty = use ${%s}]", envVar)
	}
	val, err := i.p.AskSecret(question)
	if err != nil {
		return "", err
	}
	val = strings.TrimSpace(val)
	if val == "" {
		if i.getenv(envVar) != "" {
			i.p.Say("    using ${%s}", envVar)
			return "${" + envVar + "}", nil
		}
		return "", nil
	}
	i.p.Say("    got %s", redact(val))
	return val, nil
}

// write renders and persists the config file.
func (i *Installer) write(path string, cfg *config.Config) error {
	return i.writeCfg(path, []byte(render(cfg)))
}

// render produces the config file. It is written by hand rather than
// marshalled so the result carries the same explanatory comments as
// config.example.yaml — a config you cannot read is a config you cannot fix.
func render(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("# Riggs configuration, written by `riggs install`.\n")
	b.WriteString("# The ledger lives beside this file, under the same base name.\n")
	b.WriteString("#\n")
	b.WriteString("# Tokens may be literals (this file is mode 0600) or ${ENV} references.\n")
	b.WriteString("version: 1\n\n")

	b.WriteString("admin:\n")
	fmt.Fprintf(&b, "  slack-user-id: %s\n", yamlValue(cfg.Admin.SlackUserID))
	fmt.Fprintf(&b, "  jira-email: %s\n", yamlValue(cfg.Admin.JiraEmail))
	fmt.Fprintf(&b, "  github-login: %s\n\n", yamlValue(cfg.Admin.GitHubLogin))

	b.WriteString("slack:\n  profiles:\n")
	p := cfg.Slack.Profiles[config.DefaultProfile]
	b.WriteString("    default:\n")
	fmt.Fprintf(&b, "      bot-token: %s\n", yamlValue(p.BotToken))
	if p.UserToken != "" {
		fmt.Fprintf(&b, "      user-token: %s\n", yamlValue(p.UserToken))
	}
	b.WriteString("\n")

	if cfg.Jira.Email != "" || cfg.Jira.Token != "" {
		b.WriteString("jira:\n")
		fmt.Fprintf(&b, "  email: %s\n", yamlValue(cfg.Jira.Email))
		if cfg.Jira.Token != "" {
			fmt.Fprintf(&b, "  token: %s\n", yamlValue(cfg.Jira.Token))
		}
	}
	return b.String()
}

// yamlValue quotes a scalar so a ${ENV} reference or a token with punctuation
// survives the round trip.
func yamlValue(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// expandHome resolves a leading ~ so a typed path behaves as the user expects.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}

// blockedError marks a smoke-test failure, which aborts the install.
type blockedError struct{ error }

// smokeTest proves the whole outbound path with real data: a real PR, a real
// card, a real Slack message. Anything less would leave the first genuine
// failure to happen unattended, at 3am, in a scheduled job.
func (i *Installer) smokeTest(ctx context.Context, cfg *config.Config) error {
	i.p.Say("")
	send, err := i.p.Confirm("Send a test message to the admin user?", true)
	if err != nil {
		return err
	}
	if !send {
		i.p.Say("Skipped. Nothing has proved the Slack path yet.")
		return nil
	}

	auth, err := i.ghAuth(ctx)
	if err != nil {
		return blockedError{fmt.Errorf("test message failed: %w", err)}
	}
	login := cfg.Admin.GitHubLogin
	if login == "" {
		login = auth.Login
	}

	i.p.Say("Fetching one pull request awaiting your review…")
	prs, err := i.newPRs(auth.Token).ReviewRequested(ctx, login, 1)
	if err != nil {
		return blockedError{fmt.Errorf("test message failed: %w", err)}
	}

	target, err := slack.NewResolver(cfg).Resolve("", "")
	if err != nil {
		return blockedError{fmt.Errorf("test message failed: %w", err)}
	}

	card, text := testCard(prs, login)
	ref, err := i.poster.Post(ctx, target, slack.Message{Text: text, Blocks: card.Blocks()})
	if err != nil {
		return blockedError{fmt.Errorf("test message failed: %w", err)}
	}

	if len(prs) == 0 {
		i.p.Say("No PRs are awaiting your review — sent a confirmation card instead.")
	} else {
		i.p.Say("Sent the card for %s.", prs[0].Ref())
	}
	i.p.Say("Delivered to %s at %s.", ref.Channel, ref.TS)
	return nil
}

// testCard renders the smoke-test message. With a real PR it is the actual
// review card, so what lands in Slack is what the job will produce — not a
// stand-in that proves less than it appears to.
func testCard(prs []github.PullRequest, login string) (blockkit.Card, string) {
	if len(prs) == 0 {
		return blockkit.Card{
			Title:    "Riggs is installed",
			Subtitle: "test message",
			Body: fmt.Sprintf("Riggs reached GitHub and Slack successfully. "+
				"No open pull requests are currently awaiting review from *%s*.", login),
			Context: "Sent by `riggs install`",
		}, "Riggs is installed and can reach Slack."
	}
	pr := prs[0]
	return blockkit.Card{
		Title:          pr.Title,
		Subtitle:       pr.Ref(),
		Body:           fmt.Sprintf("Opened by *%s*. This is a test card from `riggs install`.", pr.Author),
		BodyBlockID:    "pr_summary",
		ActionsBlockID: pr.Ref(),
		Actions: []blockkit.Element{
			blockkit.LinkButton{Text: "Open in Browser", URL: pr.URL},
		},
	}, fmt.Sprintf("Riggs test message — pull request %s", pr.URL)
}
