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

	"github.com/miere/riggs-mcp/internal/ai"
	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
)

// PRLister is the GitHub seam used by the smoke test.
type PRLister interface {
	ReviewRequested(ctx context.Context, login string, limit int) ([]github.PullRequest, error)
}

// Options carries what only the composition root knows.
type Options struct {
	// RiggsPath is the absolute path to this binary.
	//
	// It used to be the command a Murtaugh job invoked. Jobs now run inside the
	// daemon, which resolves its own executable at run time, so this is left
	// only for the messages that tell the operator where Riggs is.
	RiggsPath string
}

// Installer runs the interactive flow.
type Installer struct {
	p    Prompter
	opts Options

	// ghLogin is the reviewer handle answered during gather. It is baked into
	// the review job's command and deliberately NOT written to the config:
	// storing it there is what let an edit repoint the queue at a different
	// person while the job looked unchanged.
	ghLogin string

	// lookupUser resolves a Slack handle to an id; a seam so the installer can
	// be driven without a workspace.
	lookupUser func(ctx context.Context, target slack.Target, handle string) (string, error)

	ghAuth   func(ctx context.Context) (github.Auth, error)
	newPRs   func(token string) PRLister
	poster   slack.Poster
	getenv   func(string) string
	writeCfg func(path string, data []byte) error
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
	// saveJobs persists the seeded schedule. A seam, so the installer's tests
	// never create a database.
	saveJobs func(dbPath string, jobs []notify.Job) error
}

// New builds an Installer with live dependencies.
func New(p Prompter, opts Options) *Installer {
	return &Installer{
		p:          p,
		opts:       opts,
		ghAuth:     func(ctx context.Context) (github.Auth, error) { return github.AuthFromGH(ctx, nil) },
		newPRs:     func(token string) PRLister { return github.New(token) },
		poster:     slack.NewAPI(),
		lookupUser: slack.NewAPI().LookupUserID,
		getenv:     os.Getenv,
		writeCfg: func(path string, data []byte) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			// 0600: this file may hold literal tokens.
			return os.WriteFile(path, data, 0o600)
		},
		lookPath: exec.LookPath,
		stat:     os.Stat,
		saveJobs: storeJobs,
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
	if err := i.gatherJobs(ctx, path); err != nil {
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
	i.p.Say("  (the GitHub login goes on the job's command line, not into the config)")
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
	i.ghLogin = strings.TrimSpace(login)
	cfg.Admin = config.Admin{SlackUserID: slackID, JiraEmail: jiraEmail}

	i.p.Say("")
	i.p.Say("Slack credentials for the \"default\" profile — Riggs' OWN app.")
	i.p.Say("Not a shared one: clicks are delivered to whichever app posted the")
	i.p.Say("message, so the daemon can only answer buttons on its own messages.")
	i.p.Say("Paste a token, or ${ENV_VAR} to reference the environment instead.")
	bot, err := i.secret("  Bot token (xoxb-)", "SLACK_BOT_TOKEN")
	if err != nil {
		return nil, err
	}
	if bot == "" {
		return nil, errors.New("a bot token is required: without it nothing can be posted")
	}
	// The app-level token is what `riggs daemon` opens Socket Mode with. It is
	// optional here so an install can finish without one, but every interactive
	// control Riggs renders is dead until it exists — so the absence is called
	// out rather than passed over.
	app, err := i.secret("  App token (xapp-, for `riggs daemon`)", "SLACK_APP_TOKEN")
	if err != nil {
		return nil, err
	}
	if app == "" {
		i.p.Say("    no app token: `riggs daemon` cannot start, so the digest's")
		i.p.Say("    buttons will not respond until one is configured.")
	}
	user, err := i.secret("  User token (xoxp-, optional)", "SLACK_USER_TOKEN")
	if err != nil {
		return nil, err
	}
	cfg.Slack.Profiles[config.DefaultProfile] = config.Profile{
		BotToken: bot, AppToken: app, UserToken: user}

	i.p.Say("")
	i.p.Say("Jira credentials (used by the ticket jobs).")
	jiraToken, err := i.secret("  API token (optional)", "ATLASSIAN_JIRA_TOKEN")
	if err != nil {
		return nil, err
	}
	// Asked for, never guessed. Riggs has no default tenant: a machine that
	// silently read and assigned tickets on somebody else's Jira would be worse
	// than one that read none.
	jiraBaseURL, err := i.p.Ask("  Tenant URL (e.g. https://example.atlassian.net)",
		i.getenv(config.JiraBaseURLEnv))
	if err != nil {
		return nil, err
	}
	cfg.Jira = config.Jira{
		Email: jiraEmail, Token: jiraToken, BaseURL: strings.TrimSpace(jiraBaseURL)}

	if err := i.gatherReviewRequest(ctx, cfg); err != nil {
		return nil, err
	}
	if err := i.gatherSMEAssistance(ctx, cfg); err != nil {
		return nil, err
	}
	if err := i.gatherAI(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// gatherReviewRequest collects who assists with pull requests: where "Ask for
// Code Review" sends its card, and whom it tags.
//
// An empty answer DISABLES the option rather than falling back to the admin.
// The fallback was defensible while this was the only verb on the row — asking
// yourself at least reaches somebody. It is not defensible beside "Run Code
// Review": a menu whose first entry quietly means "send myself a card" reads as
// a bug next to one that does the work.
//
// The reviewer is RESOLVED here, so the written config holds an id. A handle
// would otherwise be resolved on every click, and — worse — a handle that
// matches nobody would not be discovered until someone pressed the button and
// no message arrived.
func (i *Installer) gatherReviewRequest(ctx context.Context, cfg *config.Config) error {
	i.p.Say("")
	i.p.Say("Who should assist with PULL REQUESTS?")
	i.p.Say("\"Ask for Code Review\" posts a card and tags them under it. It asks a")
	i.p.Say("person and stops — nothing is delegated and no review is started.")
	i.p.Say("Leave it empty to leave the option off the menu entirely.")

	reviewer, err := i.p.Ask("  Reviewer (@handle or Slack id; empty = disabled)", "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(reviewer) == "" {
		i.p.Say("    disabled: rows will not offer \"Ask for Code Review\".")
		return nil
	}
	i.p.Say("  Leave the channel empty to DM the reviewer instead.")
	channel, err := i.p.Ask("  Channel id", "")
	if err != nil {
		return err
	}
	cfg.ReviewRequest = config.ReviewRequest{Channel: strings.TrimSpace(channel)}
	cfg.ReviewRequest.UserID = i.resolveTagged(ctx, cfg, reviewer)
	return nil
}

// gatherSMEAssistance collects who assists with tickets.
//
// Asked separately from the review request, and never defaulted from it. The
// two look identical and answer different questions — one asks a human to
// review code that exists, the other asks a subject-matter expert whether work
// that does not exist yet is worth picking up — so they end up pointed at
// different channels and different people, and a shared answer would mean
// changing one silently moved the other.
func (i *Installer) gatherSMEAssistance(ctx context.Context, cfg *config.Config) error {
	i.p.Say("")
	i.p.Say("Who should assist with TICKETS?")
	i.p.Say("\"Ask for SME Assistance\" does the same for a Jira ticket: it tags a")
	i.p.Say("person, and starts nothing. Leave it empty to leave the option off.")

	who, err := i.p.Ask("  Person to tag (@handle or Slack id; empty = disabled)", "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(who) == "" {
		i.p.Say("    disabled: rows will not offer \"Ask for SME Assistance\".")
		return nil
	}
	i.p.Say("  Leave the channel empty to DM them instead.")
	channel, err := i.p.Ask("  Channel id", "")
	if err != nil {
		return err
	}
	cfg.SMEAssistance = config.SMEAssistance{Channel: strings.TrimSpace(channel)}
	cfg.SMEAssistance.UserID = i.resolveTagged(ctx, cfg, who)
	return nil
}

// gatherAI collects the local harness that backs "Run Code Review" and "Run AI
// Assistance" — the two options that do the work rather than asking somebody to.
//
// One command serves both, because it is one harness; the prompts are asked for
// separately, because reviewing code that exists and scoping work that does not
// are different instructions. Both prompts can be left at their defaults here
// and reworded later from the App Home tab (§7e), which is where they will
// actually be tuned: a prompt is judged by its output, and there is none yet.
func (i *Installer) gatherAI(cfg *config.Config) error {
	i.p.Say("")
	i.p.Say("Which command runs an AI review on this machine?")
	i.p.Say("It runs HERE, under Riggs, and reports back in the digest's thread.")
	i.p.Say("A known harness gets its own prompt flag; anything else is handed the")
	i.p.Say("prompt as its first argument. Empty leaves both \"Run …\" options off.")

	command, err := i.p.Ask("  Command (e.g. claude; empty = disabled)", "")
	if err != nil {
		return err
	}
	command = strings.TrimSpace(command)
	if command == "" {
		i.p.Say("    disabled: rows will not offer \"Run Code Review\" or \"Run AI Assistance\".")
		return nil
	}
	cfg.AI = config.AI{Command: command}
	i.describeHarness(command)

	i.p.Say("  Where should it run? Claude Code reads the project it is standing in,")
	i.p.Say("  and a launch agent inherits no working directory worth having.")
	workdir, err := i.p.Ask("  Working directory", i.getenv("HOME"))
	if err != nil {
		return err
	}
	cfg.AI.WorkDir = strings.TrimSpace(expandHome(workdir))
	if cfg.AI.WorkDir != "" {
		if _, err := i.stat(cfg.AI.WorkDir); err != nil {
			// Said, not refused. The directory may be created before the first
			// click, and an install that aborts over it is out of proportion.
			i.p.Say("    note: %s does not exist yet.", cfg.AI.WorkDir)
		}
	}

	cfg.AI.ReviewPrompt, err = i.gatherPrompt("pull request", config.DefaultAIReviewPrompt)
	if err != nil {
		return err
	}
	cfg.AI.AssistPrompt, err = i.gatherPrompt("ticket", config.DefaultAIAssistPrompt)
	if err != nil {
		return err
	}
	return nil
}

// describeHarness says how the configured command will actually be invoked.
//
// Worth a line: the difference between a known harness and a custom one is
// invisible in the answer just typed, and it decides whether the prompt arrives
// behind a flag or as a bare argument. It also probes PATH, because a
// misspelled binary otherwise goes unnoticed until somebody clicks.
func (i *Installer) describeHarness(command string) {
	harness := ai.New(command, "", 0)
	argv := harness.Argv("<prompt>")
	i.p.Say("    will run: %s", strings.Join(argv, " "))
	if _, err := i.lookPath(harness.Program()); err != nil {
		i.p.Say("    note: %s is not on PATH; the options are rendered but every run will fail.",
			harness.Program())
	}
}

// gatherPrompt offers one harness prompt for override.
//
// An answer equal to the default is stored as EMPTY, not as the default's own
// words. That keeps "never overridden" distinguishable from "overridden to
// whatever the default said the day I installed", and means a later change to
// the default reaches this machine.
func (i *Installer) gatherPrompt(noun, def string) (string, error) {
	i.p.Say("  Default %s prompt:", noun)
	i.p.Say("    %s", def)
	answer, err := i.p.Ask("  Override it? (empty = keep the default)", "")
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" || answer == def {
		return "", nil
	}
	return answer, nil
}

// resolveTagged normalises a typed reviewer or expert into a Slack id.
//
// A handle that cannot be resolved is stored AS WRITTEN rather than failing the
// install: the ask is one action, and refusing to finish over it would be out
// of proportion. It is said out loud, because a handle left in the config is
// resolved on every click and fails on every click if it is wrong.
func (i *Installer) resolveTagged(ctx context.Context, cfg *config.Config, typed string) string {
	ref := slack.ParseUserRef(typed)
	switch {
	case ref.IsID():
		return ref.ID
	case ref.Handle == "":
		return strings.TrimSpace(typed)
	}
	id, err := i.resolveUser(ctx, cfg, ref.Handle)
	if err != nil {
		i.p.Say("    could not resolve @%s: %v", ref.Handle, err)
		i.p.Say("    storing the handle as written; it is resolved on each ask.")
		return strings.TrimSpace(typed)
	}
	i.p.Say("    @%s is %s", ref.Handle, id)
	return id
}

// resolveUser looks a handle up through the bot token just collected.
func (i *Installer) resolveUser(ctx context.Context, cfg *config.Config, handle string) (string, error) {
	p, _, ok := cfg.Profile(config.DefaultProfile)
	if !ok || strings.TrimSpace(p.BotToken) == "" {
		return "", fmt.Errorf("no bot token to look it up with")
	}
	return i.lookupUser(ctx, slack.Target{BotToken: os.ExpandEnv(p.BotToken)}, handle)
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
	b.WriteString("\n")

	b.WriteString("slack:\n  profiles:\n")
	p := cfg.Slack.Profiles[config.DefaultProfile]
	b.WriteString("    default:\n")
	fmt.Fprintf(&b, "      bot-token: %s\n", yamlValue(p.BotToken))
	if p.AppToken != "" {
		fmt.Fprintf(&b, "      app-token: %s\n", yamlValue(p.AppToken))
	}
	if p.UserToken != "" {
		fmt.Fprintf(&b, "      user-token: %s\n", yamlValue(p.UserToken))
	}
	b.WriteString("\n")

	// Written only when configured. An empty section is not the same as an
	// absent one here: absent means the option is off, and a file full of empty
	// keys invites somebody to fill one in and wonder why nothing changed.
	if cfg.ReviewRequest.UserID != "" {
		b.WriteString("# Who assists with pull requests. \"Ask for Code Review\" tags them\n")
		b.WriteString("# and stops; a human reads it and decides. Remove user-id to turn the\n")
		b.WriteString("# option off. An empty channel DMs them.\n")
		b.WriteString("review-request:\n")
		if cfg.ReviewRequest.Channel != "" {
			fmt.Fprintf(&b, "  channel: %s\n", yamlValue(cfg.ReviewRequest.Channel))
		}
		fmt.Fprintf(&b, "  user-id: %s\n", yamlValue(cfg.ReviewRequest.UserID))
		if cfg.ReviewRequest.Prompt != "" {
			fmt.Fprintf(&b, "  prompt: %s\n", yamlValue(cfg.ReviewRequest.Prompt))
		}
		b.WriteString("\n")
	}

	if cfg.SMEAssistance.UserID != "" {
		b.WriteString("# Who assists with tickets. \"Ask for SME Assistance\" tags a person —\n")
		b.WriteString("# a subject-matter expert — and starts nothing. Configured separately\n")
		b.WriteString("# from review-request on purpose: same shape, different question.\n")
		b.WriteString("sme-assistance:\n")
		if cfg.SMEAssistance.Channel != "" {
			fmt.Fprintf(&b, "  channel: %s\n", yamlValue(cfg.SMEAssistance.Channel))
		}
		fmt.Fprintf(&b, "  user-id: %s\n", yamlValue(cfg.SMEAssistance.UserID))
		if cfg.SMEAssistance.Prompt != "" {
			fmt.Fprintf(&b, "  prompt: %s\n", yamlValue(cfg.SMEAssistance.Prompt))
		}
		b.WriteString("\n")
	}

	if cfg.AI.Command != "" {
		b.WriteString("# The local harness behind \"Run Code Review\" and \"Run AI Assistance\".\n")
		b.WriteString("# These RUN, on this machine, and report back in the digest's thread.\n")
		b.WriteString("# A known harness (claude) gets its own prompt flag; anything else is\n")
		b.WriteString("# handed the prompt as its first argument.\n")
		b.WriteString("#\n")
		b.WriteString("# The prompts are editable from the App Home tab. An absent prompt uses\n")
		b.WriteString("# the built-in default, which is the point of leaving it absent.\n")
		b.WriteString("ai:\n")
		fmt.Fprintf(&b, "  command: %s\n", yamlValue(cfg.AI.Command))
		if cfg.AI.WorkDir != "" {
			fmt.Fprintf(&b, "  workdir: %s\n", yamlValue(cfg.AI.WorkDir))
		}
		if cfg.AI.ReviewPrompt != "" {
			fmt.Fprintf(&b, "  pull-request-prompt: %s\n", yamlValue(cfg.AI.ReviewPrompt))
		}
		if cfg.AI.AssistPrompt != "" {
			fmt.Fprintf(&b, "  ticket-prompt: %s\n", yamlValue(cfg.AI.AssistPrompt))
		}
		b.WriteString("\n")
	}

	if cfg.Jira.Email != "" || cfg.Jira.Token != "" {
		b.WriteString("jira:\n")
		fmt.Fprintf(&b, "  email: %s\n", yamlValue(cfg.Jira.Email))
		if cfg.Jira.Token != "" {
			fmt.Fprintf(&b, "  token: %s\n", yamlValue(cfg.Jira.Token))
		}
		// There is no default tenant, so a config without this line leaves the
		// jira.* tools unregistered.
		if cfg.Jira.BaseURL != "" {
			fmt.Fprintf(&b, "  base-url: %s\n", yamlValue(cfg.Jira.BaseURL))
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
	login := i.ghLogin
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
