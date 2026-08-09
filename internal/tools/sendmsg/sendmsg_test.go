package sendmsg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

func tool(t *testing.T, admin string, profiles map[string]config.Profile) (*Tool, *slacktest.Fake) {
	t.Helper()
	cfg := &config.Config{
		Path:  "<test>",
		Admin: config.Admin{SlackUserID: admin},
		Slack: config.Slack{Profiles: profiles},
	}
	fake := slacktest.New()
	return New(slack.NewResolver(cfg), fake), fake
}

var ready = map[string]config.Profile{
	"default": {BotToken: "xoxb-default"},
	"nc":      {BotToken: "xoxb-nc"},
}

func TestPostsToNamedChannel(t *testing.T) {
	tl, fake := tool(t, "U1", ready)

	got, err := tl.Invoke(context.Background(), map[string]any{
		"body": "hello", "slack_channel": "C123",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := got.(Result)
	if res.Channel != "C123" || res.Profile != "default" || res.TS == "" {
		t.Errorf("result = %+v, want the default profile and the named channel", res)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Msg.Text != "hello" {
		t.Errorf("calls = %+v, want one post carrying the body", fake.Calls)
	}
}

func TestSelectsNamedProfile(t *testing.T) {
	tl, fake := tool(t, "U1", ready)

	if _, err := tl.Invoke(context.Background(), map[string]any{
		"body": "x", "slack_channel": "C1", "slack_profile": "nc",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := fake.Calls[0].Target.BotToken; got != "xoxb-nc" {
		t.Errorf("posted with %q, want the nc profile's token", got)
	}
}

// No channel means DM the admin — the documented fallback.
func TestFallsBackToAdminDM(t *testing.T) {
	tl, fake := tool(t, "U0B20G0ET9T", ready)

	if _, err := tl.Invoke(context.Background(), map[string]any{"body": "x"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !fake.Calls[0].Target.IsDM() {
		t.Errorf("target = %+v, want a DM", fake.Calls[0].Target)
	}
	if fake.Calls[0].Target.AdminUserID != "U0B20G0ET9T" {
		t.Errorf("DM target = %q, want the admin", fake.Calls[0].Target.AdminUserID)
	}
}

// An undefined profile fails loudly rather than posting somewhere unintended.
func TestUnknownProfileFails(t *testing.T) {
	tl, fake := tool(t, "U1", ready)

	_, err := tl.Invoke(context.Background(), map[string]any{
		"body": "x", "slack_channel": "C1", "slack_profile": "nope",
	})
	if !errors.Is(err, slack.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if len(fake.Calls) != 0 {
		t.Error("posted despite an unresolvable profile")
	}
}

func TestBlocksAreParsed(t *testing.T) {
	tl, fake := tool(t, "U1", ready)

	if _, err := tl.Invoke(context.Background(), map[string]any{
		"body": "fallback", "slack_channel": "C1",
		"blocks": `[{"type":"divider"}]`,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.Calls[0].Msg.Blocks) != 1 {
		t.Errorf("blocks = %v, want the parsed array", fake.Calls[0].Msg.Blocks)
	}
}

func TestMalformedBlocksAreRejected(t *testing.T) {
	tl, fake := tool(t, "U1", ready)

	_, err := tl.Invoke(context.Background(), map[string]any{
		"body": "x", "slack_channel": "C1", "blocks": `{"type":"divider"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "JSON array") {
		t.Fatalf("err = %v, want a clear complaint about the blocks payload", err)
	}
	if len(fake.Calls) != 0 {
		t.Error("posted despite malformed blocks")
	}
}

func TestBodyIsRequired(t *testing.T) {
	tl, _ := tool(t, "U1", ready)
	if _, err := tl.Invoke(context.Background(), map[string]any{"slack_channel": "C1"}); err == nil {
		t.Fatal("Invoke with no body = nil error")
	}
}

func TestThreadedReply(t *testing.T) {
	tl, fake := tool(t, "U1", ready)
	if _, err := tl.Invoke(context.Background(), map[string]any{
		"body": "x", "slack_channel": "C1", "thread": "1699.5",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.Calls[0].Msg.ThreadTS != "1699.5" {
		t.Errorf("thread_ts = %q, want the parent ts", fake.Calls[0].Msg.ThreadTS)
	}
}

// The schema is the contract both frontends share, so the two conventional
// delivery parameters must be declared on it.
func TestSchemaDeclaresDeliveryParameters(t *testing.T) {
	tl, _ := tool(t, "U1", ready)
	schema := tl.InputSchema()
	for _, want := range []string{"body", "blocks", "thread", "slack_profile", "slack_channel"} {
		if _, ok := schema.Properties[want]; !ok {
			t.Errorf("schema is missing property %q", want)
		}
	}
}
