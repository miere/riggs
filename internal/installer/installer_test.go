package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// script is a Prompter that answers from a queue, so the whole interactive
// flow runs in a test with no terminal.
type script struct {
	answers  []string
	secrets  []string
	confirms []bool
	said     []string
	asked    []string
}

func (s *script) next(queue *[]string) string {
	if len(*queue) == 0 {
		return ""
	}
	v := (*queue)[0]
	*queue = (*queue)[1:]
	return v
}

func (s *script) Ask(q, def string) (string, error) {
	s.asked = append(s.asked, q)
	v := s.next(&s.answers)
	if v == "" {
		return def, nil
	}
	if v == "<empty>" { // an explicit empty answer, overriding the default
		return "", nil
	}
	return v, nil
}

func (s *script) AskSecret(q string) (string, error) {
	s.asked = append(s.asked, q)
	return s.next(&s.secrets), nil
}

func (s *script) Confirm(q string, def bool) (bool, error) {
	s.asked = append(s.asked, q)
	if len(s.confirms) == 0 {
		return def, nil
	}
	v := s.confirms[0]
	s.confirms = s.confirms[1:]
	return v, nil
}

func (s *script) Say(format string, args ...any) {
	s.said = append(s.said, fmt.Sprintf(format, args...))
}

func (s *script) transcript() string { return strings.Join(s.said, "\n") }

// rig assembles an installer with every seam faked.
type rig struct {
	*Installer
	prompt  *script
	slack   *slacktest.Fake
	written map[string]string
	dbPath  string
	jobs    []notify.Job
	users   map[string]string
	prs     []github.PullRequest
	prErr   error
	authErr error
}

func newRig(t *testing.T, s *script) *rig {
	t.Helper()
	r := &rig{prompt: s, slack: slacktest.New(), written: map[string]string{},
		users: map[string]string{"murtaugh": "U0B6HK02YBB"}}
	inst := New(s, Options{RiggsPath: "/usr/local/bin/riggs"})
	inst.poster = r.slack
	inst.ghAuth = func(context.Context) (github.Auth, error) {
		if r.authErr != nil {
			return github.Auth{}, r.authErr
		}
		return github.Auth{Token: "gho_test", Login: "miere"}, nil
	}
	inst.newPRs = func(string) PRLister { return prLister{r} }
	inst.writeCfg = func(path string, data []byte) error { r.written[path] = string(data); return nil }
	inst.saveJobs = func(dbPath string, jobs []notify.Job) error {
		r.dbPath, r.jobs = dbPath, jobs
		return nil
	}
	inst.lookPath = func(string) (string, error) { return "/usr/local/bin/murtaugh", nil }
	inst.stat = func(path string) (os.FileInfo, error) {
		if strings.Contains(path, "murtaugh") {
			return nil, nil // Murtaugh's config exists
		}
		return nil, os.ErrNotExist // the Riggs config does not
	}
	inst.getenv = func(string) string { return "" }
	// A workspace directory the installer can resolve a handle against.
	inst.lookupUser = func(_ context.Context, _ slack.Target, handle string) (string, error) {
		if id, ok := r.users[handle]; ok {
			return id, nil
		}
		return "", fmt.Errorf("no such member %q", handle)
	}
	r.Installer = inst
	return r
}

type prLister struct{ r *rig }

func (p prLister) ReviewRequested(context.Context, string, int) ([]github.PullRequest, error) {
	return p.r.prs, p.r.prErr
}

// happyScript answers the whole flow: config path, identity, secrets, then the
// Murtaugh path and one channel per installable job.
// Indices into happyScript's answer list. Named because they are positional:
// adding a prompt shifts everything after it, and a silently shifted index
// makes a test assert against the wrong question.
const answerConfigPath = 0

func happyScript(extra ...string) *script {
	return &script{
		answers: append([]string{
			"/tmp/riggs/config.yaml",        // config location
			"miere",                         // github login
			"U0B20G0ET9T",                   // slack user id
			"miere@nurturecloud.com",        // jira email
			"https://example.atlassian.net", // jira tenant
			"@murtaugh",                     // who assists with pull requests
			"C0B24F579T4",                   // review-request channel
			"@murtaugh",                     // who assists with tickets
			"C0B29C20Z9S",                   // sme-assistance channel
			"claude",                        // the AI command
			"/home/m/Development",           // where it runs
			"",                              // pull-request prompt: keep the default
			"",                              // ticket prompt: keep the default
			"C0B24F579T4", "default",        // review digest: channel, profile
			"C0B29C20Z9S", "default", // ticket digest: channel, profile
		}, extra...),
		// bot, app, user, jira — in prompt order.
		secrets:  []string{"xoxb-real-bot-token", "xapp-real-app-token", "", "jira-api-token"},
		confirms: []bool{true}, // send the test message
	}
}

func TestHappyPathWritesConfig(t *testing.T) {
	s := happyScript("C0B29C20Z9S")
	r := newRig(t, s)
	r.prs = []github.PullRequest{{
		Repo: "acme/monolith", Number: 20069, Title: "Fix the resolver",
		URL: "https://github.com/acme/monolith/pull/20069", Author: "alex",
	}}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, ok := r.written["/tmp/riggs/config.yaml"]
	if !ok {
		t.Fatalf("nothing written; files = %v", r.written)
	}
	for _, want := range []string{
		`slack-user-id: "U0B20G0ET9T"`,
		`bot-token: "xoxb-real-bot-token"`,
		`email: "miere@nurturecloud.com"`,
		`token: "jira-api-token"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	// An omitted optional secret must not appear as an empty key.
	if strings.Contains(got, "user-token") {
		t.Errorf("empty user-token was written:\n%s", got)
	}
}

// The config the installer writes must be loadable by the very binary that
// wrote it — otherwise the first job run is the first time anyone finds out.
func TestWrittenConfigIsLoadable(t *testing.T) {
	s := happyScript()
	s.confirms = []bool{false} // skip the test message
	r := newRig(t, s)

	dir := t.TempDir()
	path := dir + "/config.yaml"
	s.answers[answerConfigPath] = path
	r.Installer.writeCfg = func(p string, data []byte) error { return os.WriteFile(p, data, 0o600) }

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the installer wrote a config it cannot load: %v", err)
	}
	if cfg.Admin.SlackUserID != "U0B20G0ET9T" {
		t.Errorf("admin.slack-user-id = %q, want the answered value", cfg.Admin.SlackUserID)
	}
	if _, _, ok := cfg.Profile(""); !ok {
		t.Error("the default Slack profile is not resolvable from the written config")
	}
}

// A pasted token is never echoed back in full.
func TestSecretsAreRedactedInTheTranscript(t *testing.T) {
	s := happyScript()
	s.confirms = []bool{false}
	r := newRig(t, s)

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(s.transcript(), "xoxb-real-bot-token") {
		t.Errorf("the bot token was echoed to the console:\n%s", s.transcript())
	}
	if !strings.Contains(s.transcript(), "xoxb***********oken") {
		t.Errorf("no redacted form shown:\n%s", s.transcript())
	}
}

// An existing config is not clobbered without consent.
func TestDeclinedOverwriteAborts(t *testing.T) {
	s := happyScript()
	s.confirms = []bool{false} // decline the overwrite
	r := newRig(t, s)
	r.Installer.stat = func(string) (os.FileInfo, error) { return nil, nil } // everything exists

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run = nil error after declining to overwrite")
	}
	if len(r.written) != 0 {
		t.Errorf("wrote %v despite declining", r.written)
	}
}

func TestSlackUserIDIsRequired(t *testing.T) {
	s := happyScript()
	s.answers[2] = "<empty>"
	r := newRig(t, s)

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Slack user id") {
		t.Fatalf("err = %v, want a complaint about the missing Slack user id", err)
	}
}

func TestBotTokenIsRequired(t *testing.T) {
	s := happyScript()
	s.secrets = []string{"", "", ""}
	r := newRig(t, s)

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bot token") {
		t.Fatalf("err = %v, want a complaint about the missing bot token", err)
	}
}

// An existing machine keeps its secrets in the environment: an empty answer
// adopts the ${ENV} reference rather than writing a literal.
func TestEmptySecretAdoptsEnvReference(t *testing.T) {
	s := happyScript()
	s.secrets = []string{"", "", ""}
	s.confirms = []bool{false}
	r := newRig(t, s)
	r.Installer.getenv = func(k string) string {
		if k == "SLACK_BOT_TOKEN" {
			return "xoxb-from-env"
		}
		return ""
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := r.written["/tmp/riggs/config.yaml"]
	if !strings.Contains(got, `bot-token: "${SLACK_BOT_TOKEN}"`) {
		t.Errorf("config did not adopt the env reference:\n%s", got)
	}
	if strings.Contains(got, "xoxb-from-env") {
		t.Errorf("the env value was inlined rather than referenced:\n%s", got)
	}
}

// The smoke test is the point of the installer: if it fails, the install
// fails, rather than leaving a broken setup that looks finished.
func TestSmokeTestFailureAbortsTheInstall(t *testing.T) {
	s := happyScript()
	r := newRig(t, s)
	r.prErr = errors.New("bad credentials")

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run = nil error despite the test message failing")
	}
	if !strings.Contains(err.Error(), "test message failed") {
		t.Errorf("err = %v, want it attributed to the test message", err)
	}
	if len(r.jobs) != 0 {
		t.Error("jobs were scheduled after the smoke test failed")
	}
}

func TestSlackFailureAbortsTheInstall(t *testing.T) {
	s := happyScript()
	r := newRig(t, s)
	r.slack.PostErr = errors.New("invalid_auth")

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "test message failed") {
		t.Fatalf("err = %v, want the install aborted on the Slack failure", err)
	}
}

// No PRs awaiting review is not a failure — the path still works, and the
// message says so.
func TestSmokeTestWithNoPullRequestsStillSucceeds(t *testing.T) {
	s := happyScript()
	r := newRig(t, s)
	r.prs = nil

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.slack.Calls) != 1 {
		t.Fatalf("slack calls = %d, want the confirmation card sent", len(r.slack.Calls))
	}
	if !strings.Contains(r.prompt.transcript(), "No PRs are awaiting your review") {
		t.Errorf("the empty result was not explained:\n%s", r.prompt.transcript())
	}
}

// The test message goes to the admin as a DM, per the brief.
func TestSmokeTestDMsTheAdmin(t *testing.T) {
	s := happyScript()
	r := newRig(t, s)
	r.prs = []github.PullRequest{{Repo: "o/r", Number: 1, Title: "T", URL: "u", Author: "a"}}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := r.slack.Calls[0]
	if !call.Target.IsDM() || call.Target.AdminUserID != "U0B20G0ET9T" {
		t.Errorf("target = %+v, want a DM to the admin", call.Target)
	}
	if !strings.Contains(call.Msg.Text, "o/r") && !strings.Contains(call.Msg.Text, "u") {
		t.Errorf("fallback text = %q, want it to name the PR", call.Msg.Text)
	}
}

// loadWritten runs the installer against a real file and loads it back, which
// is the only assertion that proves a setting survives the whole round trip.
func loadWritten(t *testing.T, s *script) *config.Config {
	t.Helper()
	s.confirms = []bool{false} // skip the test message
	r := newRig(t, s)

	path := t.TempDir() + "/config.yaml"
	s.answers[answerConfigPath] = path
	r.Installer.writeCfg = func(p string, data []byte) error { return os.WriteFile(p, data, 0o600) }

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the installer wrote a config it cannot load: %v", err)
	}
	return cfg
}

// Riggs installs its OWN Slack app, so the app-level token the daemon needs is
// collected and written like any other credential. Without it every interactive
// control Riggs renders is dead.
func TestWritesTheAppToken(t *testing.T) {
	cfg := loadWritten(t, happyScript())

	p, _, ok := cfg.Profile(config.DefaultProfile)
	if !ok {
		t.Fatal("no default profile was written")
	}
	if p.AppToken == "" {
		t.Error("app-token did not survive the round trip")
	}
	if p.BotToken == "" {
		t.Error("bot-token did not survive the round trip")
	}
}

// There is no default tenant, so a written config that omits it would leave the
// jira.* tools permanently unregistered.
func TestWritesTheJiraTenant(t *testing.T) {
	if got := loadWritten(t, happyScript()).JiraBaseURL(); got != "https://example.atlassian.net" {
		t.Fatalf("JiraBaseURL = %q", got)
	}
}

// --- the split ---------------------------------------------------------------

// The three questions this phase adds, and what each of them writes.
func TestInstallWritesTheSplitSections(t *testing.T) {
	s := happyScript("C0B29C20Z9S")
	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := r.written["/tmp/riggs/config.yaml"]

	for _, want := range []string{
		// The reviewer's handle is resolved at install time, so a click does
		// not pay for a lookup and a wrong handle is found now rather than by
		// somebody pressing a button and seeing nothing arrive.
		"review-request:\n",
		`user-id: "U0B6HK02YBB"`,
		// The human ask under its own name. Renaming it is the point of the
		// phase: it never involved an LLM.
		"sme-assistance:\n",
		// And the harness that does.
		"ai:\n",
		`command: "claude"`,
		`workdir: "/home/m/Development"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	// The retired spelling is not written, even though it still loads.
	if strings.Contains(got, "ai-assistance:") {
		t.Errorf("the retired section name was written:\n%s", got)
	}
	// A prompt left at its default is not written at all, so a later change to
	// the default reaches this machine.
	if strings.Contains(got, "pull-request-prompt") || strings.Contains(got, "ticket-prompt") {
		t.Errorf("an unchanged default prompt was written out:\n%s", got)
	}
}

// An unanswered question turns the option off. It does not fall back to the
// admin, and it does not write an empty key for somebody to fill in and wonder
// why nothing changed.
func TestUnansweredQuestionsDisableTheOptions(t *testing.T) {
	// Written out rather than patched into happyScript: an unanswered question
	// means the follow-ups it guards are never asked, so the positions of
	// everything after it move.
	s := &script{
		answers: []string{
			"/tmp/riggs/config.yaml",        // config location
			"miere",                         // github login
			"U0B20G0ET9T",                   // slack user id
			"miere@nurturecloud.com",        // jira email
			"https://example.atlassian.net", // jira tenant
			"<empty>",                       // nobody assists with pull requests
			"<empty>",                       // nobody assists with tickets
			"<empty>",                       // no AI command
			"C0B24F579T4", "default",        // review digest: channel, profile
			"C0B29C20Z9S", "default", // ticket digest: channel, profile
		},
		secrets:  []string{"xoxb-real-bot-token", "xapp-real-app-token", "", "jira-api-token"},
		confirms: []bool{true},
	}

	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := r.written["/tmp/riggs/config.yaml"]

	for _, absent := range []string{"review-request:", "sme-assistance:", "ai:"} {
		if strings.Contains(got, absent) {
			t.Errorf("%q was written for an unanswered question:\n%s", absent, got)
		}
	}
	// And it must still load: an empty answer is an ordinary install, not a
	// broken one.
	cfg, err := parseWritten(t, got)
	if err != nil {
		t.Fatalf("the written config does not load: %v\n%s", err, got)
	}
	if cfg.ReviewEnabled() || cfg.SMEEnabled() || cfg.AIEnabled() {
		t.Fatalf("an option is enabled with nothing configured: %+v", cfg)
	}
}

// A known harness gets its own prompt flag, and the console says which
// invocation was chosen — the difference is invisible in the answer just typed.
func TestTheInstallerShowsHowTheHarnessWillBeRun(t *testing.T) {
	s := happyScript("C0B29C20Z9S")
	r := newRig(t, s)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(s.transcript(), "claude -p <prompt>") {
		t.Fatalf("the transcript does not say how the harness runs:\n%s", s.transcript())
	}
}

// parseWritten loads a rendered config the way the binary that wrote it would.
func parseWritten(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return config.Load(path)
}
