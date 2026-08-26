package pullrequest

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

func TestAskPostsToTheConfiguredChannel(t *testing.T) {
	fake := slacktest.New()
	asker := NewAsker(fake, "U456", "C-reviews", "Mind casting an eye over this?")

	res, err := asker.Ask(context.Background(), "o/r#7", target)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	calls := fake.Posts()
	if len(calls) != 1 {
		t.Fatalf("got %d posts, want one", len(calls))
	}
	if got := calls[0].Target.Channel; got != "C-reviews" {
		t.Fatalf("posted to %q, want the configured channel", got)
	}
	if res.Reviewer != "U456" || res.Ref != "o/r#7" {
		t.Fatalf("result = %+v", res)
	}

	text := calls[0].Msg.Text
	for _, want := range []string{"<@U456>", "Mind casting an eye over this?", "o/r#7", "https://github.com/o/r/pull/7"} {
		if !strings.Contains(text, want) {
			t.Errorf("ask text %q is missing %q", text, want)
		}
	}
}

// No channel means a DM — to the reviewer, who is not necessarily the admin.
func TestAskWithNoChannelDMsTheReviewer(t *testing.T) {
	fake := slacktest.New()
	asker := NewAsker(fake, "U456", "", "please review")

	if _, err := asker.Ask(context.Background(), "o/r#7", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	call := fake.Posts()[0]
	if call.Target.Channel != "" {
		t.Fatalf("posted to channel %q, want a DM", call.Target.Channel)
	}
	if call.Target.AdminUserID != "U456" {
		t.Fatalf("DM target = %q, want the reviewer", call.Target.AdminUserID)
	}
}

// The Common Rule: nothing Riggs sends anywhere may refer to Riggs.
func TestAskNamesNoTool(t *testing.T) {
	fake := slacktest.New()
	asker := NewAsker(fake, "U456", "C1", "please review")

	if _, err := asker.Ask(context.Background(), "o/r#7", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	assertNoSelfReference(t, fake.Posts()[0].Msg.Text)
}

func TestAskRequiresAReviewer(t *testing.T) {
	fake := slacktest.New()
	_, err := NewAsker(fake, "", "C1", "please review").Ask(context.Background(), "o/r#7", target)
	if err == nil {
		t.Fatal("Ask succeeded with no reviewer configured")
	}
	if len(fake.Calls) != 0 {
		t.Fatal("Ask posted despite having nobody to tag")
	}
}

func TestAskRejectsABadRef(t *testing.T) {
	fake := slacktest.New()
	if _, err := NewAsker(fake, "U1", "C1", "p").Ask(context.Background(), "not-a-ref", target); err == nil {
		t.Fatal("Ask accepted a malformed ref")
	}
	if len(fake.Calls) != 0 {
		t.Fatal("Ask posted for a malformed ref")
	}
}

func TestRefURL(t *testing.T) {
	if got := RefURL("UpsideRealty/upside#20534"); got != "https://github.com/UpsideRealty/upside/pull/20534" {
		t.Fatalf("RefURL = %q", got)
	}
	// A ref that will not parse degrades to itself rather than to a broken link.
	if got := RefURL("nonsense"); got != "nonsense" {
		t.Fatalf("RefURL(nonsense) = %q", got)
	}
}

// assertNoSelfReference fails if text names the tool. The rule is absolute:
// every interaction Riggs has with Slack, GitHub or Jira must read as the
// admin's own, because it is submitted with the admin's credentials.
func assertNoSelfReference(t *testing.T, text string) {
	t.Helper()
	for _, banned := range []string{"riggs", "murtaugh"} {
		if strings.Contains(strings.ToLower(text), banned) {
			t.Errorf("text names the tool (%q): %q", banned, text)
		}
	}
}

// The approval body reaches GitHub, where everyone reading the pull request
// sees it — the most public place the rule applies.
func TestApprovalBodyNamesNoTool(t *testing.T) {
	a := NewApprover(nil, nil, nil)
	assertNoSelfReference(t, a.reviewBody)
	if strings.TrimSpace(a.reviewBody) == "" {
		t.Fatal("approval body is empty")
	}
}
