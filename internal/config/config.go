// Package config loads Riggs' configuration: who the admin is, and which Slack
// accounts ("profiles") the notifying tools may deliver through.
//
// Precedence, first hit wins:
//
//  1. an explicit path (the --config-file flag)
//  2. $RIGGS_CONFIG
//  3. $XDG_CONFIG_HOME/riggs/config.yaml
//  4. ~/.config/riggs/config.yaml
//
// Unlike the techops blueprint there is no embedded default, because there is
// no useful default for "which Slack account is yours". A missing conventional
// file is therefore not an error: it yields an empty Config whose Path still
// points at where the file would live. That is what keeps `riggs ping` and
// `riggs mcp` working on a machine with nothing provisioned — the notifying
// tools report their own missing credentials (see internal/slack), rather than
// the binary refusing to boot.
//
// An explicit --config-file that does not exist IS an error: asking for a
// specific file and silently getting none is never what the caller meant.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// NoFilePath is the reported Path of a Config that came from no file at all.
// The DB path is still derived from the location the file *would* have had, so
// the ledger has a home even before a config exists.
const NoFilePath = "<no config file>"

// DefaultProfile is the Slack profile used when a tool names none. A config
// with no profile under this name cannot deliver to Slack without an explicit
// --slack-profile, which is the behaviour Miere specified: fall back to
// default, and fail if default is not defined.
const DefaultProfile = "default"

// Config is the effective, validated configuration.
type Config struct {
	Version       int           `yaml:"version"`
	Admin         Admin         `yaml:"admin"`
	Slack         Slack         `yaml:"slack"`
	Jira          Jira          `yaml:"jira"`
	ReviewRequest ReviewRequest `yaml:"review-request"`
	SMEAssistance SMEAssistance `yaml:"sme-assistance"`
	AI            AI            `yaml:"ai"`

	// LegacyAIAssistance is the old spelling of SMEAssistance, kept only so an
	// existing config still loads.
	//
	// The section was called `ai-assistance` when the ticket action was called
	// "Ask for AI Assistance" — which asked a *person*, and never involved an
	// LLM at all. That name is now taken by something that genuinely does run
	// one, so the human ask became `sme-assistance` and this is the alias.
	//
	// Parsed rather than refused, unlike `admin.github-login`. That key was
	// refused because leaving it in place would silently steer the review queue
	// at somebody else; this one means exactly what it always meant, so refusing
	// to boot over the spelling would cost a working install for nothing.
	// `riggs capabilities` reports the deprecation instead.
	LegacyAIAssistance SMEAssistance `yaml:"ai-assistance"`

	// EnvFile is a dotenv file loaded before ${VAR} references are expanded.
	//
	// This exists for launchd. A launch agent inherits none of the shell
	// environment, so every ${SLACK_...} in this file would expand to empty and
	// the daemon would start up connected to nothing. Pointing at a dotenv file
	// — Murtaugh's own, typically — makes the daemon behave identically whether
	// it was started by launchd or by hand.
	//
	// Empty looks for `.env` beside this config file, and a missing one is not
	// an error: the variables may perfectly well come from the real environment.
	EnvFile string `yaml:"env-file"`

	// Path records where this config came from, for diagnostics. It is
	// NoFilePath when no file was read.
	Path string `yaml:"-"`

	// dbPath is the ledger location, derived from the config file path: same
	// directory, same base name, ".db" extension.
	dbPath string

	// envPath is the dotenv file that was read, and envLoaded whether it
	// existed. Both are reported by `riggs capabilities`.
	envPath   string
	envLoaded bool

	// smeDeprecated records that the SME section arrived under its retired
	// `ai-assistance` name, so the deprecation can be reported once rather than
	// guessed at by every reader of the file.
	smeDeprecated bool

	// mu guards the four prompts, which are the only fields that change after
	// load: the App Home tab edits them in place so a running daemon acts on a
	// reworded prompt without being restarted (§7e).
	//
	// It covers those fields and nothing else. Everything else in here is
	// written once during Load and read for the life of the process, and
	// putting a lock in front of it would suggest a mutability that does not
	// exist.
	mu sync.RWMutex
}

// Admin identifies the single human Riggs acts for. It exists because that
// identity was previously spread across five settings in three files
// (REVIEWER_HANDLE, REVIEWER_SLACK_ID, nudge_user_id, allowed_users and
// slack_to_jira_email), which could drift apart.
type Admin struct {
	// SlackUserID is the DM target when a tool is given no channel, the user
	// tagged by threaded replies, and the one human who sees past the divider
	// on the App Home tab (§7e).
	SlackUserID string `yaml:"slack-user-id"`
	// JiraEmail is the account tickets are assigned to.
	JiraEmail string `yaml:"jira-email"`

	// RetiredGitHubLogin is not a setting. It is parsed only so a config that
	// still carries `github-login` can be refused BY NAME, with a message
	// saying what to do instead — rather than by KnownFields, which would
	// report an unrecognised key and leave the reader guessing.
	//
	// The reviewer handle is now passed on the command that fetches reviews
	// (`riggs git pr --bulk <user>`). It lived here, resolved at run time, which
	// meant an edit to this file could silently repoint the review queue at a
	// different person while the scheduled job looked entirely unchanged. Whose
	// reviews a job fetches should be legible in the job.
	RetiredGitHubLogin string `yaml:"github-login"`
}

// Jira holds the Atlassian settings. All three may be ${ENV} references, and
// all three fall back to the ATLASSIAN_* variables Murtaugh's .env already
// exports — so an existing machine needs nothing re-provisioned, and a fresh
// one can be configured entirely by the installer.
type Jira struct {
	Email string `yaml:"email"`
	Token string `yaml:"token"`
	// BaseURL is the Atlassian tenant. Empty falls back to $ATLASSIAN_BASE_URL;
	// with neither set there is no tenant and the jira.* tools are not
	// registered at all. There is deliberately no default — see jira.New.
	BaseURL string `yaml:"base-url"`
}

// Environment variables the Jira settings fall back to. They are the names
// Murtaugh's .env already exports, so an existing machine needs nothing
// re-provisioned.
const (
	JiraEmailEnv   = "ATLASSIAN_JIRA_EMAIL"
	JiraTokenEnv   = "ATLASSIAN_JIRA_TOKEN"
	JiraBaseURLEnv = "ATLASSIAN_BASE_URL"
)

// JiraCredentials returns the effective email and token, preferring the config
// file and falling back to the environment.
func (c *Config) JiraCredentials() (email, token string) {
	email, token = c.Jira.Email, c.Jira.Token
	if email == "" {
		email = os.Getenv(JiraEmailEnv)
	}
	if token == "" {
		token = os.Getenv(JiraTokenEnv)
	}
	return email, token
}

// JiraBaseURL returns the effective tenant, on the same precedence as the
// credentials: the config file, then the environment, then empty — which leaves
// the client to apply its own default.
//
// It falls back to the environment for consistency, not necessity. The two
// settings beside it do, the variable is already exported on every machine that
// runs this, and a tenant that resolves differently from the credentials that
// authenticate against it is a confusing way to fail.
func (c *Config) JiraBaseURL() string {
	if url := strings.TrimSpace(c.Jira.BaseURL); url != "" {
		return url
	}
	return strings.TrimSpace(os.Getenv(JiraBaseURLEnv))
}

// ReviewRequest configures the "Ask for Code Review" action: where the ask is
// posted, who is tagged in it, and what it says.
//
// Nothing here triggers a review. The action drops a message and stops; a human
// reads it and decides. That is the whole feature.
type ReviewRequest struct {
	// Channel is where the ask is posted. Empty DMs the tagged user, which is
	// what makes "a channel or a DM" a configuration choice rather than two
	// code paths.
	Channel string `yaml:"channel"`
	// UserID is the Slack user tagged in the ask. Empty falls back to the
	// admin — asking yourself is a defensible default and never silently
	// tags a stranger.
	//
	// An id (`U0B6HK02YBB`), a pasted mention (`<@U0B6HK02YBB>`) or a handle
	// (`@murtaugh`) are all accepted. A handle is resolved against the
	// workspace before the ask is posted, because a mention built from one
	// would render as literal text and notify nobody.
	UserID string `yaml:"user-id"`
	// Prompt is the wording of the ask. Empty uses DefaultReviewPrompt.
	//
	// `{reviewer}` and `{requester}` are replaced with the corresponding
	// mentions. A prompt that mentions neither still gets both: the reviewer is
	// prefixed and the requester appended as `c/c`, because those are the point
	// of the feature and a wording change must not silently drop them.
	Prompt string `yaml:"prompt"`
}

// DefaultReviewPrompt is the ask, used when the config defines none. The
// requester's `c/c` is appended to whatever prompt is in force:
//
//	Hey <@reviewer>, mind to review this Pull Request? c/c <@requester>
const DefaultReviewPrompt = "Hey {reviewer}, mind to review this Pull Request?"

// ReviewPrompt is the configured prompt, or the default.
func (c *Config) ReviewPrompt() string { return c.PromptText(PromptReviewRequest) }

// ReviewReviewer is the Slack user the ask tags. Empty means nobody is
// configured, and the action is not offered at all.
//
// It used to fall back to the admin, on the grounds that asking yourself is a
// defensible default. It is not one any more: the row now offers "Ask for Code
// Review" *beside* "Run Code Review", and a menu whose first option quietly
// means "send myself a card" reads as a mistake next to one that actually does
// the work. An unanswered installer question now disables the option instead —
// which is also the only reading under which the two are honestly distinct.
func (c *Config) ReviewReviewer() string {
	return strings.TrimSpace(c.ReviewRequest.UserID)
}

// ReviewEnabled reports whether "Ask for Code Review" can be offered.
func (c *Config) ReviewEnabled() bool { return c.ReviewReviewer() != "" }

// SMEAssistance configures the "Ask for SME Assistance" action on a ticket row:
// where the ask is posted, who is tagged in it, and what it says.
//
// It asks a HUMAN — a subject-matter expert — whether work that does not exist
// yet is ready to be picked up. Nothing here runs an agent; that is `ai` below,
// and keeping them apart is the whole point of this section's name.
//
// It is a SEPARATE section from ReviewRequest, deliberately, and the two are
// never defaulted from one another. They look identical today and answer
// different questions — one asks a human to review code that exists, the other
// asks somebody to scope work that does not — so they will be pointed at
// different channels and different people, and a shared setting would mean
// changing one silently moved the other.
type SMEAssistance struct {
	// Channel is where the ask is posted. Empty DMs the tagged user.
	Channel string `yaml:"channel"`
	// UserID is the Slack user tagged in the ask. Empty disables the action,
	// on the same rule as review-request.user-id.
	//
	// An id, a pasted mention or a handle are all accepted.
	UserID string `yaml:"user-id"`
	// Prompt is the wording of the ask. Empty uses DefaultSMEPrompt.
	//
	// `{user}` and `{requester}` are replaced with the corresponding mentions.
	// A prompt that mentions neither still gets both (see internal/ask).
	Prompt string `yaml:"prompt"`
}

// DefaultSMEPrompt is the ask, used when the config defines none:
//
//	<@user>, <@requester> needs your help check if this ticket is actionable as it is
//
// It places both mentions itself, so nothing is prefixed and no `c/c` is
// appended — the guarantee in internal/ask only adds what the wording left out.
const DefaultSMEPrompt = "{user}, {requester} needs your help check if this ticket is actionable as it is"

// SMEPrompt is the configured prompt, or the default.
func (c *Config) SMEPrompt() string { return c.PromptText(PromptSMEAssistance) }

// SMEUser is the Slack user a ticket ask tags. Empty disables the action, for
// the reason ReviewReviewer gives.
func (c *Config) SMEUser() string {
	return strings.TrimSpace(c.SMEAssistance.UserID)
}

// SMEEnabled reports whether "Ask for SME Assistance" can be offered.
func (c *Config) SMEEnabled() bool { return c.SMEUser() != "" }

// Slack holds the named accounts Riggs can deliver through.
type Slack struct {
	Profiles map[string]Profile `yaml:"profiles"`
}

// Profile is one Slack app's credentials. Tokens are written as ${ENV}
// references so the config file itself carries no secrets.
//
// AppToken is the xapp- token that opens a Socket Mode connection. It is
// required only by the profile `riggs daemon` listens as, and ignored on every
// other profile — a profile Riggs only ever posts through needs a bot token and
// nothing more.
type Profile struct {
	BotToken  string `yaml:"bot-token"`
	UserToken string `yaml:"user-token"`
	AppToken  string `yaml:"app-token"`
}

// Load reads the configuration, resolving the path by the documented
// precedence. explicit is the --config-file value; empty means the chain.
func Load(explicit string) (*Config, error) {
	path, data, err := read(explicit)
	if err != nil {
		return nil, err
	}
	return parse(path, data)
}

// read resolves the config path and returns its contents. data is nil when no
// file exists at a conventional location, which is not an error.
func read(explicit string) (string, []byte, error) {
	if explicit != "" {
		data, err := os.ReadFile(explicit)
		if err != nil {
			return "", nil, fmt.Errorf("--config-file %s: %w", explicit, err)
		}
		return explicit, data, nil
	}
	candidates := candidatePaths()
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err == nil {
			return path, data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	// Nothing on disk. Report the highest-precedence candidate anyway, so the
	// ledger lands beside where the config file would be created.
	return candidates[0], nil, nil
}

// candidatePaths lists the conventional config locations, highest precedence
// first.
func candidatePaths() []string {
	var out []string
	if p := os.Getenv("RIGGS_CONFIG"); p != "" {
		out = append(out, p)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		out = append(out, filepath.Join(xdg, "riggs", "config.yaml"))
	}
	out = append(out, DefaultPath())
	return out
}

// DefaultPath is ~/.config/riggs/config.yaml, the documented default.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "riggs", "config.yaml")
	}
	return filepath.Join(home, ".config", "riggs", "config.yaml")
}

// parse decodes the YAML and fills in derived fields. Decoding uses
// KnownFields(true): a mistyped key is a silent behaviour change, so it is
// refused rather than ignored.
func parse(path string, data []byte) (*Config, error) {
	cfg := &Config{Path: path}
	if data != nil {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cfg.Path = path
	} else {
		cfg.Path = NoFilePath
	}
	cfg.dbPath = deriveDBPath(path)
	cfg.adoptLegacySME()
	// Before expansion, not after: the dotenv file is where the ${VAR}
	// references are meant to resolve from.
	if err := cfg.loadEnvFile(path); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.expand()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// deriveDBPath puts the ledger beside the config file, under the same base
// name: ~/.config/riggs/config.yaml -> ~/.config/riggs/config.db.
func deriveDBPath(configPath string) string {
	dir := filepath.Dir(configPath)
	base := filepath.Base(configPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return filepath.Join(dir, base+".db")
}

// DBPath is where the notification ledger lives.
func (c *Config) DBPath() string { return c.dbPath }

// expand resolves ${VAR} references in every token, so the config file holds
// references and the environment holds secrets.
func (c *Config) expand() {
	c.Jira.Email = os.ExpandEnv(c.Jira.Email)
	c.Jira.Token = os.ExpandEnv(c.Jira.Token)
	// The tenant is expanded too. It was omitted originally, which made
	// `base-url: ${ATLASSIAN_BASE_URL}` resolve to that string *literally* —
	// every Jira request then went to "${ATLASSIAN_BASE_URL}/rest/api/3/...".
	c.Jira.BaseURL = os.ExpandEnv(c.Jira.BaseURL)
	// The harness invocation and its working directory, so `workdir:
	// ${HOME}/Development` resolves. The PROMPTS are deliberately left alone: a
	// prompt is prose the admin wrote, and a `$` in it is a dollar sign.
	c.AI.Command = os.ExpandEnv(c.AI.Command)
	c.AI.WorkDir = os.ExpandEnv(c.AI.WorkDir)
	for name, p := range c.Slack.Profiles {
		p.BotToken = os.ExpandEnv(p.BotToken)
		p.UserToken = os.ExpandEnv(p.UserToken)
		p.AppToken = os.ExpandEnv(p.AppToken)
		c.Slack.Profiles[name] = p
	}
}

// validate reports every structural problem at once rather than the first, so
// fixing a config takes one edit rather than one round-trip per mistake.
//
// Only structure is checked here. An empty token is a *capability* gap, not a
// config error: it disables the tools that need it (reported by `riggs
// capabilities`) and leaves the rest of the binary working.
func (c *Config) validate() error {
	var problems []string
	for name := range c.Slack.Profiles {
		if strings.TrimSpace(name) == "" {
			problems = append(problems, "slack.profiles has an empty profile name")
		}
	}
	// A tenant that is not an absolute http(s) URL produces requests to a
	// nonsense address and a failure that names neither this setting nor this
	// file. `vmxproperty.atlassian.net` with no scheme is the likely typo, and
	// an unexpanded ${VAR} the likely accident.
	if strings.TrimSpace(c.Admin.RetiredGitHubLogin) != "" {
		problems = append(problems,
			"admin.github-login is no longer a setting: remove it, and pass the login on the command instead (e.g. `riggs git pr --bulk <user>`)")
	}
	if url := strings.TrimSpace(c.Jira.BaseURL); url != "" && !isAbsoluteHTTPURL(url) {
		problems = append(problems,
			fmt.Sprintf("jira.base-url %q is not an absolute http(s) URL (e.g. https://example.atlassian.net)", url))
	}
	problems = append(problems, c.validateAI()...)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// adoptLegacySME folds a config still spelling the section `ai-assistance` into
// SMEAssistance.
//
// Field by field rather than wholesale, so a file carrying BOTH spellings —
// which a half-finished hand edit produces — keeps the new one's values and
// fills only what it left blank. The alternative, "the legacy section wins when
// the new one is empty", would let a forgotten old key silently override a
// deliberate new one.
//
// SMEDeprecated records that it happened, so `riggs capabilities` can say so.
// Nothing else reads it: the alias is honoured in full, not half-honoured.
func (c *Config) adoptLegacySME() {
	legacy := c.LegacyAIAssistance
	if legacy == (SMEAssistance{}) {
		return
	}
	c.smeDeprecated = true
	if strings.TrimSpace(c.SMEAssistance.Channel) == "" {
		c.SMEAssistance.Channel = legacy.Channel
	}
	if strings.TrimSpace(c.SMEAssistance.UserID) == "" {
		c.SMEAssistance.UserID = legacy.UserID
	}
	if strings.TrimSpace(c.SMEAssistance.Prompt) == "" {
		c.SMEAssistance.Prompt = legacy.Prompt
	}
}

// SMEDeprecated reports whether this config was read from the retired
// `ai-assistance` key.
func (c *Config) SMEDeprecated() bool { return c.smeDeprecated }

// isAbsoluteHTTPURL reports whether s is a parseable absolute http(s) URL with
// a host.
func isAbsoluteHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// Profile returns the named Slack profile, falling back to DefaultProfile when
// name is empty. ok is false when the requested profile is not configured.
func (c *Config) Profile(name string) (Profile, string, bool) {
	if name == "" {
		name = DefaultProfile
	}
	p, ok := c.Slack.Profiles[name]
	return p, name, ok
}

// ProfileNames lists the configured profile names, for diagnostics.
func (c *Config) ProfileNames() []string {
	out := make([]string, 0, len(c.Slack.Profiles))
	for name := range c.Slack.Profiles {
		out = append(out, name)
	}
	return out
}
