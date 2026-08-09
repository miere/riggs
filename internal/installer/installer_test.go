package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
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

// ran records one external command the installer executed.
type ran struct {
	name string
	args []string
}

// rig assembles an installer with every seam faked.
type rig struct {
	*Installer
	prompt  *script
	slack   *slacktest.Fake
	written map[string]string
	cmds    []ran
	prs     []github.PullRequest
	prErr   error
	authErr error
}

func newRig(t *testing.T, s *script, tools map[string]bool) *rig {
	t.Helper()
	r := &rig{prompt: s, slack: slacktest.New(), written: map[string]string{}}
	inst := New(s, Options{
		RiggsPath: "/usr/local/bin/riggs",
		ToolsFor:  func(string) (map[string]bool, error) { return tools, nil },
	})
	inst.poster = r.slack
	inst.ghAuth = func(context.Context) (github.Auth, error) {
		if r.authErr != nil {
			return github.Auth{}, r.authErr
		}
		return github.Auth{Token: "gho_test", Login: "miere"}, nil
	}
	inst.newPRs = func(string) PRLister { return prLister{r} }
	inst.writeCfg = func(path string, data []byte) error { r.written[path] = string(data); return nil }
	inst.runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		r.cmds = append(r.cmds, ran{name: name, args: args})
		return nil, nil
	}
	inst.lookPath = func(string) (string, error) { return "/usr/local/bin/murtaugh", nil }
	inst.stat = func(path string) (os.FileInfo, error) {
		if strings.Contains(path, "murtaugh") {
			return nil, nil // Murtaugh's config exists
		}
		return nil, os.ErrNotExist // the Riggs config does not
	}
	inst.getenv = func(string) string { return "" }
	r.Installer = inst
	return r
}

type prLister struct{ r *rig }

func (p prLister) ReviewRequested(context.Context, string, int) ([]github.PullRequest, error) {
	return p.r.prs, p.r.prErr
}

// happyScript answers the whole flow: config path, identity, secrets, then the
// Murtaugh path and one channel per installable job.
func happyScript(extra ...string) *script {
	return &script{
		answers: append([]string{
			"/tmp/riggs/config.yaml",               // config location
			"miere",                                // github login
			"U0B20G0ET9T",                          // slack user id
			"miere@nurturecloud.com",               // jira email
			"/home/m/.config/murtaugh/config.yaml", // murtaugh config
		}, extra...),
		secrets:  []string{"xoxb-real-bot-token", "", "jira-api-token"},
		confirms: []bool{true}, // send the test message
	}
}

func TestHappyPathWritesConfig(t *testing.T) {
	s := happyScript("C0B29C20Z9S")
	r := newRig(t, s, map[string]bool{"git.pr.fetch-reviews": true})
	r.prs = []github.PullRequest{{
		Repo: "UpsideRealty/upside", Number: 20069, Title: "Fix the resolver",
		URL: "https://github.com/UpsideRealty/upside/pull/20069", Author: "hjed",
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
		`github-login: "miere"`,
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
	r := newRig(t, s, nil)

	dir := t.TempDir()
	path := dir + "/config.yaml"
	s.answers[0] = path
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
	r := newRig(t, s, nil)

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
	r := newRig(t, s, nil)
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
	r := newRig(t, s, nil)

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Slack user id") {
		t.Fatalf("err = %v, want a complaint about the missing Slack user id", err)
	}
}

func TestBotTokenIsRequired(t *testing.T) {
	s := happyScript()
	s.secrets = []string{"", "", ""}
	r := newRig(t, s, nil)

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
	r := newRig(t, s, nil)
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
	r := newRig(t, s, map[string]bool{"git.pr.fetch-reviews": true})
	r.prErr = errors.New("bad credentials")

	err := r.Run(context.Background())
	if err == nil {
		t.Fatal("Run = nil error despite the test message failing")
	}
	if !strings.Contains(err.Error(), "test message failed") {
		t.Errorf("err = %v, want it attributed to the test message", err)
	}
	if len(r.cmds) != 0 {
		t.Error("jobs were registered after the smoke test failed")
	}
}

func TestSlackFailureAbortsTheInstall(t *testing.T) {
	s := happyScript()
	r := newRig(t, s, nil)
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
	r := newRig(t, s, nil)
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
	r := newRig(t, s, nil)
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
