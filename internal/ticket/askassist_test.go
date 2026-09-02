package ticket

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// askRig assembles an asker over a temp ledger and a fake Slack.
func askRig(t *testing.T, fj *fakeJira, user, channel, prompt string) (*Asker, *slacktest.Fake) {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	fake := slacktest.New()
	return NewAsker(fj, notify.New(store, fake), fake, user, channel, prompt), fake
}

func askableJira() *fakeJira {
	i := jira.Issue{
		Key:         "NYX-1",
		Summary:     "Derive the business contact type",
		Description: "The importer needs to tell a business from a person.",
		Status:      ReadyStatus,
		Reporter:    "Miere Teixeira",
	}
	return &fakeJira{issues: map[string]jira.Issue{i.Key: i}}
}

// The ask is a card plus a tag in its own thread: the card is the subject, the
// tag is the message about it.
func TestAskPostsACardAndTagsInItsThread(t *testing.T) {
	asker, fake := askRig(t, askableJira(), "UHELPER01", "C-scope", "")

	result, err := asker.Ask(context.Background(), "NYX-1", "UASKER01", target)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !result.Tagged {
		t.Fatal("the ask was not tagged")
	}

	posts := fake.Posts()
	if len(posts) != 2 {
		t.Fatalf("calls = %v, want a card and a threaded tag", fake.Kinds())
	}
	if posts[0].Target.Channel != "C-scope" {
		t.Fatalf("card went to %q, want the configured channel", posts[0].Target.Channel)
	}
	if posts[1].Msg.ThreadTS != posts[0].Ref.TS && posts[1].Msg.ThreadTS == "" {
		t.Fatal("the tag was not threaded under the card")
	}
	if !strings.Contains(posts[1].Msg.Text, "<@UHELPER01>") {
		t.Fatalf("tag = %q, want the recipient mentioned", posts[1].Msg.Text)
	}
	if !strings.Contains(posts[1].Msg.Text, "<@UASKER01>") {
		t.Fatalf("tag = %q, want the requester copied in", posts[1].Msg.Text)
	}
	// The default prompt places both mentions itself, so nothing is prefixed
	// and no c/c is appended.
	want := "<@UHELPER01>, <@UASKER01> needs your help check if this ticket is actionable as it is"
	if posts[1].Msg.Text != want {
		t.Fatalf("tag = %q, want %q", posts[1].Msg.Text, want)
	}
}

// Nothing Riggs sends anywhere may name Riggs. The ask reads as the words of
// whoever clicked, because it is their decision that produced it.
func TestAskNamesNoTool(t *testing.T) {
	asker, fake := askRig(t, askableJira(), "UHELPER01", "C-scope", "")
	if _, err := asker.Ask(context.Background(), "NYX-1", "UASKER01", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for _, c := range fake.Calls {
		lowered := strings.ToLower(c.Msg.Text)
		if strings.Contains(lowered, "riggs") || strings.Contains(lowered, "murtaugh") {
			t.Fatalf("message names the tool: %q", c.Msg.Text)
		}
	}
	if strings.Contains(strings.ToLower(config.DefaultSMEPrompt), "riggs") {
		t.Fatalf("the default prompt names the tool: %q", config.DefaultSMEPrompt)
	}
}

// With no channel configured the ask is a DM to the recipient — who is not
// necessarily the admin, so the DM target is overridden rather than left to
// fall through.
func TestAskDMsTheRecipientWithNoChannel(t *testing.T) {
	asker, fake := askRig(t, askableJira(), "UHELPER01", "", "")

	if _, err := asker.Ask(context.Background(), "NYX-1", "UASKER01", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	card := fake.Posts()[0]
	if card.Target.Channel != "" || card.Target.AdminUserID != "UHELPER01" {
		t.Fatalf("card target = %+v, want a DM to the recipient", card.Target)
	}
}

// Asking twice updates the card rather than posting a second one.
func TestAskTwiceUpdatesTheCard(t *testing.T) {
	asker, fake := askRig(t, askableJira(), "UHELPER01", "C-scope", "")
	ctx := context.Background()

	if _, err := asker.Ask(ctx, "NYX-1", "UASKER01", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	fake.Reset()
	if _, err := asker.Ask(ctx, "NYX-1", "UASKER01", target); err != nil {
		t.Fatalf("Ask (again): %v", err)
	}

	// The card is unchanged, so the ledger's fingerprint gate makes no call for
	// it at all; only the tag is posted again.
	if got := len(fake.Posts()); got != 1 {
		t.Fatalf("calls = %v, want only the tag re-posted", fake.Kinds())
	}
}

// A digest row's block_id is the bare key; the legacy card's is jira_qct_<KEY>.
// Both must reach the same ticket.
func TestAskAcceptsEitherBlockIDForm(t *testing.T) {
	for _, item := range []string{"NYX-1", ActionsBlockID("NYX-1")} {
		asker, fake := askRig(t, askableJira(), "UHELPER01", "C-scope", "")
		result, err := asker.Ask(context.Background(), item, "UASKER01", target)
		if err != nil {
			t.Fatalf("Ask(%q): %v", item, err)
		}
		if result.Key != "NYX-1" {
			t.Fatalf("Ask(%q) resolved to %q", item, result.Key)
		}
		if len(fake.Posts()) != 2 {
			t.Fatalf("Ask(%q) calls = %v", item, fake.Kinds())
		}
	}
}

// A recipient who cannot be named is refused BEFORE anything is posted: a card
// whose tag reaches nobody is a request nobody receives.
func TestAskRefusesWithNobodyToTag(t *testing.T) {
	asker, fake := askRig(t, askableJira(), "", "C-scope", "")

	if _, err := asker.Ask(context.Background(), "NYX-1", "UASKER01", target); err == nil {
		t.Fatal("Ask succeeded with nobody configured")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("posted anyway: %v", fake.Kinds())
	}
}

// A handle with nothing wired to resolve it is an error, not a mention that
// renders as literal text and notifies nobody.
func TestAskRefusesAnUnresolvableHandle(t *testing.T) {
	asker, fake := askRig(t, askableJira(), "@helper", "C-scope", "")

	_, err := asker.Ask(context.Background(), "NYX-1", "UASKER01", target)
	if err == nil {
		t.Fatal("Ask succeeded with an unresolvable handle")
	}
	if !strings.Contains(err.Error(), "sme-assistance.user-id") {
		t.Fatalf("error = %v, want it to name the setting", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("posted anyway: %v", fake.Kinds())
	}
}

// A ticket that cannot be read is not asked about, and nothing is posted.
func TestAskRefusesAnUnreadableTicket(t *testing.T) {
	fj := askableJira()
	fj.getErr = errors.New("502 bad gateway")
	asker, fake := askRig(t, fj, "UHELPER01", "C-scope", "")

	if _, err := asker.Ask(context.Background(), "NYX-1", "UASKER01", target); err == nil {
		t.Fatal("Ask succeeded on an unreadable ticket")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("posted anyway: %v", fake.Kinds())
	}
}

// The ask card offers a link and no verb: assigning from here is not this pass,
// and a control that cannot work is worse than one that is not there.
func TestAskCardCarriesOnlyTheLink(t *testing.T) {
	const url = "https://jira.test/browse/NYX-1"
	card := AskCard(jira.Issue{Key: "NYX-1", Summary: "Something"}, "body", url)

	if len(card.Actions) != 1 {
		t.Fatalf("actions = %+v, want the link alone", card.Actions)
	}
	link, ok := card.Actions[0].(blockkit.LinkButton)
	if !ok {
		t.Fatalf("action = %T, want a link button", card.Actions[0])
	}
	if link.URL != url {
		t.Fatalf("link.URL = %q, want %q", link.URL, url)
	}
}

// The daemon and the asker both recover a ticket key from a block_id, so this
// outlives the card loop its test was originally written beside.
func TestTicketKeyFromBlockID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"jira_qct_NYX-1234", "NYX-1234"},
		{"NYX-1234", "NYX-1234"},
		{"", ""},
	} {
		if got := TicketKeyFromBlockID(tc.in); got != tc.want {
			t.Errorf("TicketKeyFromBlockID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
