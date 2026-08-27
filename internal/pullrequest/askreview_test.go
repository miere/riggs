package pullrequest

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// detailer serves one scripted pull request.
type detailer struct {
	detail github.Detail
	err    error
	calls  int
}

func (d *detailer) PullRequestDetail(context.Context, string, int) (github.Detail, error) {
	d.calls++
	return d.detail, d.err
}

func askPR() github.Detail {
	return github.Detail{
		Repo: "o/r", Number: 7, Title: "Fix the thing", Body: "body",
		URL: "https://github.com/o/r/pull/7", Author: "hjed", State: "open",
	}
}

// askerStore builds a real ledger over the fake Slack, because the ask card is
// tracked now: it has to be findable when the approval lands.
func askerStore(t *testing.T, fake *slacktest.Fake) (*notify.Store, *notify.Notifier) {
	t.Helper()
	store, err := notify.Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, notify.New(store, fake)
}

func newAsker(t *testing.T, fake *slacktest.Fake, channel string) (*Asker, *detailer) {
	t.Helper()
	d := &detailer{detail: askPR()}
	store, n := askerStore(t, fake)
	return NewAsker(d, store, n, fake, "U0B6HK02YBB", channel, "please take a look"), d
}

// blocksOf decodes a posted message's blocks.
func blocksOf(t *testing.T, c slacktest.Call) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(c.Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// --- the card --------------------------------------------------------------

// The ask is a card, not a line of text — the same container shape the per-PR
// queue used, which is why that renderer was retained rather than retired.
func TestAskPostsAContainerCard(t *testing.T) {
	fake := slacktest.New()
	asker, gh := newAsker(t, fake, "C-reviews")

	res, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gh.calls != 1 {
		t.Fatalf("read GitHub %d times, want once", gh.calls)
	}

	posts := fake.Posts()
	if len(posts) != 2 {
		t.Fatalf("got %d posts, want the card and its thread tag", len(posts))
	}
	if posts[0].Target.Channel != "C-reviews" {
		t.Fatalf("card posted to %q", posts[0].Target.Channel)
	}
	if !res.Tagged {
		t.Error("result does not report the tag")
	}

	card := blocksOf(t, posts[0])[0]
	if card["type"] != "container" {
		t.Fatalf("card type = %v, want container", card["type"])
	}
	if card["subtitle"].(map[string]any)["text"] != "o/r#7" {
		t.Errorf("card subtitle = %v", card["subtitle"])
	}
}

// Approve and Open in Browser stay; the overflow does not. The reviewer is
// being asked one question, and a menu of alternatives is a worse way to ask.
func TestAskCardHasNoOverflow(t *testing.T) {
	card := AskCard(askPR(), "a summary")

	var actions, buttons, links, overflows int
	raw, err := json.Marshal(card.Blocks())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, child := range blocks[0]["child_blocks"].([]any) {
		b := child.(map[string]any)
		if b["type"] != "actions" {
			continue
		}
		actions++
		if b["block_id"] != "o/r#7" {
			t.Errorf("actions block_id = %v, want the ref", b["block_id"])
		}
		for _, e := range b["elements"].([]any) {
			el := e.(map[string]any)
			switch {
			case el["type"] == "overflow":
				overflows++
			case el["url"] != nil:
				links++
			default:
				buttons++
			}
		}
	}
	if actions != 1 {
		t.Fatalf("got %d actions rows, want one", actions)
	}
	if overflows != 0 {
		t.Errorf("the ask card still renders an overflow")
	}
	if buttons != 1 || links != 1 {
		t.Errorf("got %d button(s) and %d link(s), want Approve + Open in Browser", buttons, links)
	}
}

// The approve button must carry a BARE intent, because the router matches it
// exactly. The reference rides in the actions block_id.
func TestAskCardApproveCarriesABareIntent(t *testing.T) {
	raw, _ := json.Marshal(AskCard(askPR(), "s").Blocks())
	var blocks []map[string]any
	_ = json.Unmarshal(raw, &blocks)

	for _, child := range blocks[0]["child_blocks"].([]any) {
		b := child.(map[string]any)
		if b["type"] != "actions" {
			continue
		}
		el := b["elements"].([]any)[0].(map[string]any)
		if el["action_id"] != AskActionID {
			t.Errorf("action_id = %v, want %q", el["action_id"], AskActionID)
		}
		if el["value"] != IntentApprove {
			t.Errorf("value = %v, want the bare intent %q", el["value"], IntentApprove)
		}
		if el["value"] == "o/r#7" {
			t.Error("the value carries the ref, which the router cannot match")
		}
	}
}

// Its own action_id, so Riggs' dispatch table and Murtaugh's legacy rules never
// have to agree about `approve_only`.
func TestAskActionIDIsDistinctFromTheLegacyCard(t *testing.T) {
	if AskActionID == "approve_only" {
		t.Fatal("the ask card reuses the legacy card's action_id")
	}
}

// --- the tag ---------------------------------------------------------------

// The tag goes in the card's thread, so the card reads as the subject and the
// ask as the message about it.
func TestAskTagsInTheCardsThread(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "C-reviews")

	if _, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	card, tag := fake.Posts()[0], fake.Posts()[1]

	if tag.Msg.ThreadTS == "" {
		t.Fatal("the tag was not threaded")
	}
	if tag.Msg.ThreadTS != card.Msg.ThreadTS && tag.Msg.ThreadTS == "" {
		t.Fatal("the tag is not under the card")
	}
	if !strings.Contains(tag.Msg.Text, "<@U0B6HK02YBB>") {
		t.Errorf("tag %q does not mention the reviewer", tag.Msg.Text)
	}
	if !strings.Contains(tag.Msg.Text, "please take a look") {
		t.Errorf("tag %q does not carry the prompt", tag.Msg.Text)
	}
	// The asker is copied in: a request with no visible asker leaves the
	// reviewer with nobody to reply to.
	if !strings.Contains(tag.Msg.Text, "c/c <@U0B20G0ET9T>") {
		t.Errorf("tag %q does not copy in the requester", tag.Msg.Text)
	}
}

// The exact shape, spelled out — this is the sentence people read.
func TestAskTagTextShape(t *testing.T) {
	got := AskTagText("U0B6HK02YBB", "U0B20G0ET9T", config.DefaultReviewPrompt)
	want := "Hey <@U0B6HK02YBB>, mind to review this Pull Request? c/c <@U0B20G0ET9T>"
	if got != want {
		t.Fatalf("AskTagText =\n  %q\nwant\n  %q", got, want)
	}
}

// Copying somebody in on their own request reads as a mistake.
func TestAskTagTextDropsASelfCC(t *testing.T) {
	for name, requester := range map[string]string{
		"nobody":       "",
		"the reviewer": "U0B6HK02YBB",
	} {
		got := AskTagText("U0B6HK02YBB", requester, config.DefaultReviewPrompt)
		if strings.Contains(got, "c/c") {
			t.Errorf("%s: tag still copies somebody in: %q", name, got)
		}
		if !strings.HasPrefix(got, "Hey <@U0B6HK02YBB>,") {
			t.Errorf("%s: tag = %q", name, got)
		}
	}
}

// An empty prompt falls back rather than rendering "Hey <@x>,  c/c <@y>".
func TestAskTagTextFallsBackToTheDefaultPrompt(t *testing.T) {
	if got := AskTagText("U0B6HK02YBB", "U0B20G0ET9T", "   "); got != AskTagText("U0B6HK02YBB", "U0B20G0ET9T", config.DefaultReviewPrompt) {
		t.Fatalf("AskTagText with a blank prompt = %q", got)
	}
}

// An untagged ask is still visible, and re-posting the card to retry the tag
// would be worse — so the card survives and the failure is reported.
func TestAskReportsATagFailureWithoutLosingTheCard(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "C-reviews")
	fake.PostErrAfter = 1

	res, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err == nil {
		t.Fatal("Ask hid a tag failure")
	}
	if res.TS == "" {
		t.Error("the card's ts was lost, so nothing records where the ask went")
	}
	if res.Tagged {
		t.Error("Tagged is true after the tag failed")
	}
}

// No channel means a DM — to the reviewer, who is not necessarily the admin.
func TestAskWithNoChannelDMsTheReviewer(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "")

	if _, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	call := fake.Posts()[0]
	if call.Target.Channel != "" {
		t.Fatalf("posted to channel %q, want a DM", call.Target.Channel)
	}
	if call.Target.AdminUserID != "U0B6HK02YBB" {
		t.Fatalf("DM target = %q, want the reviewer", call.Target.AdminUserID)
	}
}

// --- guards ----------------------------------------------------------------

func TestAskRequiresAReviewer(t *testing.T) {
	fake := slacktest.New()
	d := &detailer{detail: askPR()}
	store, n := askerStore(t, fake)
	_, err := NewAsker(d, store, n, fake, "", "C1", "p").
		Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err == nil {
		t.Fatal("Ask succeeded with no reviewer configured")
	}
	if len(fake.Calls) != 0 || d.calls != 0 {
		t.Fatal("Ask did work despite having nobody to tag")
	}
}

func TestAskRejectsABadRef(t *testing.T) {
	fake := slacktest.New()
	asker, gh := newAsker(t, fake, "C1")
	if _, err := asker.Ask(context.Background(), "not-a-ref", "U0B20G0ET9T", target); err == nil {
		t.Fatal("Ask accepted a malformed ref")
	}
	if len(fake.Calls) != 0 || gh.calls != 0 {
		t.Fatal("Ask did work for a malformed ref")
	}
}

// A pull request that cannot be read cannot be rendered, so nothing is posted.
func TestAskFailsClosedWhenGitHubDoes(t *testing.T) {
	fake := slacktest.New()
	d := &detailer{err: errors.New("404")}
	store, n := askerStore(t, fake)
	_, err := NewAsker(d, store, n, fake, "U0B6HK02YBB", "C1", "p").
		Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err == nil {
		t.Fatal("Ask succeeded despite a failed GitHub read")
	}
	if len(fake.Calls) != 0 {
		t.Fatal("Ask posted a card it could not render")
	}
}

// --- announcing a failure ---------------------------------------------------

// The card this action posts goes somewhere the clicker cannot see — another
// channel, or a DM to the reviewer — so a request that died on the way and one
// that was delivered look identical from the digest. That is the whole reason
// this failure has to be announced.
func TestAFailedAskIsAnnouncedUnderTheDigestRow(t *testing.T) {
	fake := slacktest.New()
	d := &detailer{err: errors.New("404")}
	store, n := askerStore(t, fake)

	_, err := NewAsker(d, store, n, fake, "U0B6HK02YBB", "C-reviews", "p").
		WithFailureThread("C-digest", "1700000000.000100").
		Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err == nil {
		t.Fatal("Ask succeeded despite a failed GitHub read")
	}

	posts := fake.Posts()
	if len(posts) != 1 {
		t.Fatalf("posted %d message(s), want 1 announcement", len(posts))
	}
	got := posts[0]
	if got.Target.Channel != "C-digest" {
		t.Errorf("announced in %q, want the digest's channel", got.Target.Channel)
	}
	if got.Msg.ThreadTS != "1700000000.000100" {
		t.Errorf("announced at top level, want the digest row's thread: %q", got.Msg.ThreadTS)
	}
	for _, want := range []string{blockkit.MarkerFailed, "o/r#7", "404"} {
		if !strings.Contains(got.Msg.Text, want) {
			t.Errorf("announcement %q does not mention %q", got.Msg.Text, want)
		}
	}
	// Marked, so the daemon does not tell them a second time somewhere else.
	if !slack.WasReported(err) {
		t.Error("the announced failure was not marked as reported")
	}
}

// A CLI ask has a terminal to fail into. Posting a card about it would put a
// message in a channel nobody was looking at on behalf of somebody who is
// already reading the error.
func TestACLIAskAnnouncesNothing(t *testing.T) {
	fake := slacktest.New()
	d := &detailer{err: errors.New("404")}
	store, n := askerStore(t, fake)

	_, err := NewAsker(d, store, n, fake, "U0B6HK02YBB", "C-reviews", "p").
		Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err == nil {
		t.Fatal("Ask succeeded despite a failed GitHub read")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("announced a failure with no thread to announce into: %v", fake.Calls)
	}
	if slack.WasReported(err) {
		t.Error("an unannounced failure claimed to have been reported")
	}
}

// If the announcement itself cannot be posted, the daemon's own reporter is the
// only thing left that can reach the user — so the error must not claim to have
// been handled.
func TestAnAnnouncementThatFailsLeavesTheErrorUnreported(t *testing.T) {
	fake := slacktest.New()
	fake.PostErr = errors.New("slack is down")
	d := &detailer{err: errors.New("404")}
	store, n := askerStore(t, fake)

	_, err := NewAsker(d, store, n, fake, "U0B6HK02YBB", "C-reviews", "p").
		WithFailureThread("C-digest", "1700000000.000100").
		Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err == nil {
		t.Fatal("Ask succeeded despite a failed GitHub read")
	}
	if slack.WasReported(err) {
		t.Error("an announcement that failed to post still marked the error reported")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the original cause was lost: %v", err)
	}
}

// A successful ask announces nothing: the card is the answer.
func TestASuccessfulAskAnnouncesNoFailure(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "C-reviews")

	if _, err := asker.WithFailureThread("C-digest", "1700000000.000100").
		Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for _, c := range fake.Posts() {
		if strings.Contains(c.Msg.Text, blockkit.MarkerFailed) {
			t.Errorf("a successful ask announced a failure: %q", c.Msg.Text)
		}
	}
}

// --- the Common Rule -------------------------------------------------------

func TestAskNamesNoTool(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "C1")

	if _, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	for _, c := range fake.Posts() {
		assertNoSelfReference(t, c.Msg.Text)
	}
}

// The approval from this card leaves NO comment: it is the reviewer's own, and
// a body would be words they did not write.
func TestAskCardApprovalLeavesNoComment(t *testing.T) {
	a := NewApprover(nil, nil, nil).WithoutReviewBody()
	if a.reviewBody != "" {
		t.Fatalf("reviewBody = %q, want empty", a.reviewBody)
	}
	// The queue's own approvals still carry one, and it still names no tool.
	b := NewApprover(nil, nil, nil)
	assertNoSelfReference(t, b.reviewBody)
	if strings.TrimSpace(b.reviewBody) == "" {
		t.Error("the queue's approval body is empty")
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

func TestRefURL(t *testing.T) {
	if got := RefURL("UpsideRealty/upside#20534"); got != "https://github.com/UpsideRealty/upside/pull/20534" {
		t.Fatalf("RefURL = %q", got)
	}
	// A ref that will not parse degrades to itself rather than to a broken link.
	if got := RefURL("nonsense"); got != "nonsense" {
		t.Fatalf("RefURL(nonsense) = %q", got)
	}
}

// A configured wording places the mentions itself via the placeholders.
func TestAskTagTextSubstitutesPlaceholders(t *testing.T) {
	got := AskTagText("U0B6HK02YBB", "U0B20G0ET9T",
		"{requester} would like {reviewer} to look at this.")
	want := "<@U0B20G0ET9T> would like <@U0B6HK02YBB> to look at this."
	if got != want {
		t.Fatalf("AskTagText = %q, want %q", got, want)
	}
	// Both are already present, so nothing is prefixed or appended.
	if strings.Contains(got, "c/c") {
		t.Errorf("a prompt that named the requester still got a c/c: %q", got)
	}
}

// A wording that mentions neither still gets both: they are the point of the
// feature, and a config change must not silently drop them.
func TestAskTagTextGuaranteesBothMentions(t *testing.T) {
	got := AskTagText("U0B6HK02YBB", "U0B20G0ET9T", "please review")
	if !strings.Contains(got, "<@U0B6HK02YBB>") {
		t.Errorf("the reviewer is not mentioned: %q", got)
	}
	if !strings.Contains(got, "c/c <@U0B20G0ET9T>") {
		t.Errorf("the requester is not copied in: %q", got)
	}
}

// A handle in the config is resolved before anything is posted — a mention
// built from one renders as literal text and notifies nobody.
func TestAskResolvesAConfiguredHandle(t *testing.T) {
	fake := slacktest.New()
	d := &detailer{detail: askPR()}
	store, n := askerStore(t, fake)
	asker := NewAsker(d, store, n, fake, "@murtaugh", "C1", "").
		WithResolver(fakeResolver{"murtaugh": "U0B6HK02YBB"})

	res, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Reviewer != "U0B6HK02YBB" {
		t.Fatalf("Reviewer = %q, want the resolved id", res.Reviewer)
	}
	if tag := fake.Posts()[1].Msg.Text; !strings.Contains(tag, "<@U0B6HK02YBB>") {
		t.Fatalf("tag = %q, want the resolved mention", tag)
	}
}

// A handle that matches nobody fails BEFORE the card is posted: a card whose
// tag can name nobody is a request nobody receives.
func TestAskFailsClosedOnAnUnresolvableHandle(t *testing.T) {
	fake := slacktest.New()
	d := &detailer{detail: askPR()}
	store, n := askerStore(t, fake)
	asker := NewAsker(d, store, n, fake, "@nobody", "C1", "").
		WithResolver(fakeResolver{})

	if _, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err == nil {
		t.Fatal("Ask succeeded with an unresolvable reviewer")
	}
	if len(fake.Calls) != 0 {
		t.Fatal("Ask posted a card it could not tag")
	}
}

// fakeResolver is a scripted workspace directory.
type fakeResolver map[string]string

func (f fakeResolver) LookupUserID(_ context.Context, _ slack.Target, handle string) (string, error) {
	if id, ok := f[handle]; ok {
		return id, nil
	}
	return "", errors.New("no such member")
}

// The body is the author's own description, not a generated summary of it.
func TestBodyIsTheDescriptionExcerpt(t *testing.T) {
	d := askPR()
	d.Body = "## Summary\n\nThis **fixes** the [thing](https://x.test/t).\n\n" +
		"Second paragraph.\n\nThird paragraph nobody sees."

	got := Body(d)
	if strings.Contains(got, "Third paragraph") {
		t.Errorf("Body took more than two paragraphs: %q", got)
	}
	if !strings.Contains(got, "*fixes*") {
		t.Errorf("body was not converted to Slack mrkdwn: %q", got)
	}
	if strings.Contains(got, "##") {
		t.Errorf("a Markdown heading survived: %q", got)
	}
}

// A card with no body renders no section at all, which reads as though
// something failed.
func TestBodyFallsBackToTheTitle(t *testing.T) {
	d := askPR()
	for _, body := range []string{"", "   \n\n  ", "<!-- just the template -->"} {
		d.Body = body
		if got := Body(d); got != d.Title {
			t.Errorf("Body(%q) = %q, want the title", body, got)
		}
	}
}

// Deterministic — the other half of why this replaced an LLM call. A body that
// changes between renders cannot be fingerprinted, so the ledger would rewrite
// the card on every pass.
func TestBodyIsDeterministic(t *testing.T) {
	d := askPR()
	d.Body = "**a** [b](https://c.test)\n\nsecond"
	first := Body(d)
	for i := 0; i < 20; i++ {
		if got := Body(d); got != first {
			t.Fatalf("run %d differed: %q vs %q", i, got, first)
		}
	}
}

// --- settling ---------------------------------------------------------------

// The ask card is tracked now. It was not at first — "an ask is a one-off, not
// a card to maintain" held right up until an approval needed to change one.
func TestAskCardIsTracked(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "C-reviews")

	if _, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	entry, found, err := asker.store.Card(context.Background(), AskKey("o/r#7"))
	if err != nil || !found {
		t.Fatalf("the ask card was not recorded: %v", err)
	}
	if entry.TS == "" || entry.State != AskStateOpen {
		t.Fatalf("entry = %+v", entry)
	}
}

// Asking twice updates the card rather than posting a second one.
func TestAskingTwiceUpdatesTheSameCard(t *testing.T) {
	fake := slacktest.New()
	asker, _ := newAsker(t, fake, "C-reviews")

	for i := 0; i < 2; i++ {
		if _, err := asker.Ask(context.Background(), "o/r#7", "U0B20G0ET9T", target); err != nil {
			t.Fatalf("Ask %d: %v", i, err)
		}
	}
	cards := 0
	for _, c := range fake.Calls {
		if c.Kind == "post" && c.Msg.ThreadTS == "" {
			cards++
		}
	}
	if cards != 1 {
		t.Fatalf("posted %d cards, want one updated in place", cards)
	}
}

// A card still offering Approve for a merged pull request invites a click that
// can only fail.
func TestSettledCardDropsApproveAndCollapses(t *testing.T) {
	card := AskSettledCard(askPR(), "body", "Approved and merged")

	if !card.Collapsed {
		t.Error("the settled card is not collapsed")
	}
	raw, err := json.Marshal(card.Blocks())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if blocks[0]["default_collapsed"] != true {
		t.Error("default_collapsed is not set")
	}

	for _, child := range blocks[0]["child_blocks"].([]any) {
		b := child.(map[string]any)
		if b["type"] != "actions" {
			continue
		}
		els := b["elements"].([]any)
		if len(els) != 1 {
			t.Fatalf("settled card has %d controls, want only the link", len(els))
		}
		el := els[0].(map[string]any)
		if el["url"] == nil {
			t.Errorf("the surviving control is not the link: %v", el)
		}
		if el["value"] == IntentApprove {
			t.Error("Approve survived on the settled card")
		}
	}
}

// The live card still offers Approve, or none of the above means anything.
func TestLiveCardStillOffersApprove(t *testing.T) {
	raw, _ := json.Marshal(AskCard(askPR(), "body").Blocks())
	if !strings.Contains(string(raw), IntentApprove) {
		t.Fatal("the live ask card no longer offers Approve")
	}
}
