package pullrequest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

// completerRig posts a real digest first, so the completer acts on the same
// ledger state a click would find.
type completerRig struct {
	*bulkRig
	completer *Completer
}

func newCompleterRig(t *testing.T) *completerRig {
	t.Helper()
	gh := bulkGH(
		bulkPR("o/r#1", 3*time.Hour, "dependabot[bot]"),
		bulkPR("o/r#2", 2*time.Hour, "hjed"),
	)
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	c := NewCompleter(r.store, r.bulk.notifier, r.slack).
		WithClock(func() time.Time { return r.clock })
	return &completerRig{bulkRig: r, completer: c}
}

// rowsOfCall decodes a call's section rows.
func rowsOfCall(t *testing.T, c slacktest.Call) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(c.Msg.Blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return blocks[1:]
}

// The row is struck through at once. The reconcile pass would get there on its
// own, but up to three minutes later — and a button that visibly does nothing
// for three minutes reads as one that did not work.
func TestCompleteStrikesTheRowImmediately(t *testing.T) {
	r := newCompleterRig(t)

	done, err := r.completer.Complete(context.Background(), "o/r#1", ReasonApproved, target)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !done {
		t.Fatal("Complete reported nothing to do for a tracked item")
	}

	kinds := r.slack.Kinds()
	if len(kinds) != 1 || kinds[0] != "update" {
		t.Fatalf("calls = %v, want one in-place update", kinds)
	}

	rows := rowsOfCall(t, r.slack.Calls[0])
	if len(rows) != 2 {
		t.Fatalf("digest has %d rows, want both still present", len(rows))
	}

	// The approved row is struck and loses everything but its link.
	text := rows[0]["text"].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "~*") {
		t.Errorf("approved row is not struck through: %q", text)
	}
	if got := optionsOf(t, rows[0]); !equal(got, []string{IntentOpenBrowser}) {
		t.Errorf("approved row options = %v, want only the link", got)
	}
}

// Acting on one row must not disturb the others. Before the row data was
// recorded in the ledger, a rebuild with no fresh upstream read collapsed every
// untouched row to its bare reference with a dead link.
func TestCompleteLeavesTheOtherRowsIntact(t *testing.T) {
	r := newCompleterRig(t)

	if _, err := r.completer.Complete(context.Background(), "o/r#1", ReasonApproved, target); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	untouched := rowsOfCall(t, r.slack.Calls[0])[1]
	text := untouched["text"].(map[string]any)["text"].(string)

	if strings.Contains(text, "~") {
		t.Errorf("an untouched row was struck through: %q", text)
	}
	if !strings.Contains(text, "Fix the thing") {
		t.Errorf("an untouched row lost its title: %q", text)
	}
	if got := optionsOf(t, untouched); len(got) < 2 {
		t.Errorf("an untouched row lost its actions: %v", got)
	}
}

// The link survives the rebuild — it is read from the ledger, not refetched.
func TestCompleteKeepsTheLinkOnRebuiltRows(t *testing.T) {
	r := newCompleterRig(t)

	if _, err := r.completer.Complete(context.Background(), "o/r#1", ReasonApproved, target); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for i, row := range rowsOfCall(t, r.slack.Calls[0]) {
		acc := row["accessory"].(map[string]any)
		first := acc["options"].([]any)[0].(map[string]any)
		if first["url"] == nil || first["url"] == "" {
			t.Errorf("row %d lost its link on rebuild: %v", i, first)
		}
	}
}

// A pull request the digest never carried is a no-op, not an error: the
// approval still happened, and an ask-review card can be approved for one that
// never made it into a digest.
func TestCompleteIgnoresAnUntrackedRef(t *testing.T) {
	r := newCompleterRig(t)

	done, err := r.completer.Complete(context.Background(), "o/r#999", ReasonApproved, target)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done {
		t.Error("Complete claimed to have done something for an untracked ref")
	}
	if kinds := r.slack.Kinds(); len(kinds) != 0 {
		t.Fatalf("calls = %v, want none", kinds)
	}
}

// Running it twice makes one update, not two.
func TestCompleteIsIdempotent(t *testing.T) {
	r := newCompleterRig(t)

	for i := 0; i < 2; i++ {
		if _, err := r.completer.Complete(context.Background(), "o/r#1", ReasonApproved, target); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
	}
	updates := 0
	for _, k := range r.slack.Kinds() {
		if k == "update" {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("made %d updates over two calls, want 1", updates)
	}
}

// A failure is said out loud, in the digest's own thread — where the row still
// sits waiting, and where somebody looking for the outcome will look.
func TestFailReportsInTheDigestThread(t *testing.T) {
	r := newCompleterRig(t)

	posted, err := r.completer.Fail(context.Background(), "o/r#1",
		"✗ Could not approve o/r#1 — 403", target)
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if !posted {
		t.Fatal("Fail reported nowhere to post for a tracked item")
	}

	posts := r.slack.Posts()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want the failure", len(posts))
	}
	if posts[0].Msg.ThreadTS == "" {
		t.Error("the failure was not threaded under the digest")
	}
	if !strings.Contains(posts[0].Msg.Text, "403") {
		t.Errorf("failure text = %q", posts[0].Msg.Text)
	}
	// A failure must not strike the row through: it is still waiting.
	for _, k := range r.slack.Kinds() {
		if k == "update" {
			t.Error("a failure rewrote the digest")
		}
	}
}

// Nothing to thread onto is reported, so the caller can say it elsewhere rather
// than swallowing it.
func TestFailReportsWhenThereIsNoDigest(t *testing.T) {
	r := newCompleterRig(t)

	posted, err := r.completer.Fail(context.Background(), "o/r#999", "boom", target)
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if posted {
		t.Error("Fail claimed to have posted for an untracked ref")
	}
}

// The last row leaving empties the post, which is deleted rather than blanked.
func TestCompleteDeletesAPostThatEmpties(t *testing.T) {
	gh := bulkGH(bulkPR("o/r#1", time.Hour, "hjed"))
	r := newBulkRig(t, gh, BulkOptions{})
	r.run(t)
	r.slack.Reset()

	// Remove the only item, then rebuild through the completer.
	if err := r.store.DeleteItem(context.Background(), BulkItemPrefix+"o/r#1"); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	c := NewCompleter(r.store, r.bulk.notifier, r.slack)
	if err := c.rebuild(context.Background(), BulkPostPrefix+"1", target); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if len(r.slack.Deleted()) != 1 {
		t.Fatalf("calls = %v, want the emptied post deleted", r.slack.Kinds())
	}
}
