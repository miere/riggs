package notify

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slack/slacktest"
)

var target = slack.Target{Profile: "default", BotToken: "xoxb-t", Channel: "C123"}

func card(body string) blockkit.Card {
	return blockkit.Card{Title: "T", Subtitle: "o/r#1", Body: body, BodyBlockID: "pr_summary"}
}

// harness builds a Notifier over a temp ledger and a fake Slack, on a frozen
// clock. Tests that need time to move build their own (see TestMinGapLatch).
func harness(t *testing.T) (*Notifier, *slacktest.Fake, *Store) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	frozen := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	fake := slacktest.New()
	return New(store, fake).WithClock(func() time.Time { return frozen }), fake, store
}

func TestUpsertPostsWhenNew(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()

	outcome, err := n.Upsert(ctx, "o/r#1", target, card("first"), "fallback", "")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if outcome != Posted {
		t.Errorf("outcome = %s, want %s", outcome, Posted)
	}
	if got := fake.Kinds(); len(got) != 1 || got[0] != "post" {
		t.Errorf("calls = %v, want a single post", got)
	}
	entry, found, err := store.Card(ctx, "o/r#1")
	if err != nil || !found {
		t.Fatalf("card not recorded (found=%v, err=%v)", found, err)
	}
	if entry.Channel != "C123" || entry.TS == "" || entry.Profile != "default" {
		t.Errorf("entry = %+v, want where the card landed", entry)
	}
}

// The headline behaviour: re-running a tick with nothing changed must make no
// Slack call at all. This is what makes the every-1m job cheap and idempotent.
func TestUpsertUnchangedMakesNoSlackCall(t *testing.T) {
	n, fake, _ := harness(t)
	ctx := context.Background()

	if _, err := n.Upsert(ctx, "k", target, card("same"), "f", ""); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	fake.Reset()

	for i := 0; i < 5; i++ {
		outcome, err := n.Upsert(ctx, "k", target, card("same"), "f", "")
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if outcome != Unchanged {
			t.Fatalf("outcome = %s on tick %d, want %s", outcome, i, Unchanged)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %v, want none for an unchanged card", fake.Kinds())
	}
}

func TestUpsertUpdatesInPlace(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()

	_, _ = n.Upsert(ctx, "k", target, card("before"), "f", "")
	before, _, _ := store.Card(ctx, "k")
	fake.Reset()

	outcome, err := n.Upsert(ctx, "k", target, card("after"), "f", "")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if outcome != Updated {
		t.Errorf("outcome = %s, want %s", outcome, Updated)
	}
	if got := fake.Kinds(); len(got) != 1 || got[0] != "update" {
		t.Fatalf("calls = %v, want a single update", got)
	}
	if fake.Calls[0].Ref.TS != before.TS {
		t.Errorf("updated ts %q, want the tracked message %q", fake.Calls[0].Ref.TS, before.TS)
	}
	after, _, _ := store.Card(ctx, "k")
	if after.TS != before.TS {
		t.Errorf("ts changed on an in-place update: %q -> %q", before.TS, after.TS)
	}
	if after.Fingerprint == before.Fingerprint {
		t.Error("fingerprint not advanced after an update")
	}
}

// A deleted card must come back, not vanish — and the replacement carries none
// of the old thread, so the latches have to reopen with it.
func TestUpsertRepostsWhenMessageIsGone(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()

	_, _ = n.Upsert(ctx, "k", target, card("v1"), "f", "")
	old, _, _ := store.Card(ctx, "k")
	if _, err := n.Thread(ctx, "k", target, "tagged once", Once("tagged")); err != nil {
		t.Fatalf("Thread: %v", err)
	}

	fake.Reset()
	fake.UpdateErr = slack.ErrMessageNotFound

	outcome, err := n.Upsert(ctx, "k", target, card("v2"), "f", "")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if outcome != Reposted {
		t.Errorf("outcome = %s, want %s", outcome, Reposted)
	}
	if got := fake.Kinds(); len(got) != 2 || got[0] != "update" || got[1] != "post" {
		t.Errorf("calls = %v, want an update then a re-post", got)
	}
	fresh, _, _ := store.Card(ctx, "k")
	if fresh.TS == old.TS {
		t.Errorf("ts = %q, want the replacement message's ts", fresh.TS)
	}
	// The latch must have reopened: the new message has no tag on its thread.
	if _, fired, _ := store.LatchFiredAt(ctx, "k", "tagged"); fired {
		t.Error("latch survived a re-post; the reviewer would never be re-tagged")
	}
}

// A reply about something never advertised has nowhere to go. Inventing a
// top-level message would be worse than staying quiet.
func TestThreadWithoutCardIsQuiet(t *testing.T) {
	n, fake, _ := harness(t)

	sent, err := n.Thread(context.Background(), "never-posted", target, "hello", Once("tagged"))
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if sent {
		t.Error("Thread reported a send with no card to reply to")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %v, want none", fake.Kinds())
	}
}

func TestThreadRepliesOnTheCard(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()

	_, _ = n.Upsert(ctx, "k", target, card("v1"), "f", "")
	entry, _, _ := store.Card(ctx, "k")
	fake.Reset()

	sent, err := n.Thread(ctx, "k", target, "<@U1> ready for review", Once("tagged"))
	if err != nil || !sent {
		t.Fatalf("Thread: sent=%v err=%v", sent, err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("calls = %v, want one", fake.Kinds())
	}
	if got := fake.Calls[0].Msg.ThreadTS; got != entry.TS {
		t.Errorf("thread_ts = %q, want the card's ts %q", got, entry.TS)
	}
}

// The reply goes where the card is, even if the caller's default channel has
// since changed — otherwise a moved default would strand replies.
func TestThreadUsesTheCardsChannel(t *testing.T) {
	n, fake, _ := harness(t)
	ctx := context.Background()

	_, _ = n.Upsert(ctx, "k", target, card("v1"), "f", "")
	fake.Reset()

	moved := target
	moved.Channel = "C-SOMEWHERE-ELSE"
	if _, err := n.Thread(ctx, "k", moved, "reply", Once("x")); err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if got := fake.Calls[0].Target.Channel; got != "C123" {
		t.Errorf("replied into %q, want the card's own channel C123", got)
	}
}

// One ping per episode of being asked to review, no matter how many ticks run.
func TestOnceLatchFiresOnce(t *testing.T) {
	n, fake, _ := harness(t)
	ctx := context.Background()
	_, _ = n.Upsert(ctx, "k", target, card("v1"), "f", "")
	fake.Reset()

	sent, _ := n.Thread(ctx, "k", target, "tag", Once("tagged"))
	if !sent {
		t.Fatal("first tag did not fire")
	}
	for i := 0; i < 3; i++ {
		if sent, _ := n.Thread(ctx, "k", target, "tag", Once("tagged")); sent {
			t.Fatalf("tag fired again on repeat %d", i)
		}
	}
	if len(fake.Calls) != 1 {
		t.Errorf("calls = %d, want exactly one tag", len(fake.Calls))
	}
}

// Leaving the reviewable state clears the latch, which is what re-tags the
// reviewer when a PR is sent back to them.
func TestClearLatchAllowsRefiring(t *testing.T) {
	n, _, _ := harness(t)
	ctx := context.Background()
	_, _ = n.Upsert(ctx, "k", target, card("v1"), "f", "")

	_, _ = n.Thread(ctx, "k", target, "tag", Once("tagged"))
	if err := n.ClearLatch(ctx, "k", "tagged"); err != nil {
		t.Fatalf("ClearLatch: %v", err)
	}
	sent, err := n.Thread(ctx, "k", target, "tag again", Once("tagged"))
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if !sent {
		t.Error("tag did not re-fire after the latch was cleared")
	}
}

// The ledger is what lets a short-lived process pick up where the last one
// left off — the whole point of it not being in memory.
func TestStateSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.db")
	ctx := context.Background()

	store1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake1 := slacktest.New()
	n1 := New(store1, fake1)
	if _, err := n1.Upsert(ctx, "k", target, card("v1"), "f", ""); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, _ = n1.Thread(ctx, "k", target, "tag", Once("tagged"))
	store1.Close()

	// A fresh process, as the next cron tick would be.
	store2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	fake2 := slacktest.New()
	n2 := New(store2, fake2)

	outcome, err := n2.Upsert(ctx, "k", target, card("v1"), "f", "")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if outcome != Unchanged {
		t.Errorf("outcome = %s after reopen, want %s — the card would be re-posted", outcome, Unchanged)
	}
	if sent, _ := n2.Thread(ctx, "k", target, "tag", Once("tagged")); sent {
		t.Error("the once-latch re-fired after reopen; the reviewer would be re-pinged every tick")
	}
	if len(fake2.Calls) != 0 {
		t.Errorf("calls = %v, want none", fake2.Kinds())
	}
}

// Two processes — the every-1m reconcile and a button press — share one
// ledger. The second must see the first's writes.
func TestTwoProcessesShareTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()

	storeA, err := Open(path)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer storeA.Close()
	storeB, err := Open(path)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer storeB.Close()

	nA := New(storeA, slacktest.New())
	fakeB := slacktest.New()
	nB := New(storeB, fakeB)

	if _, err := nA.Upsert(ctx, "k", target, card("v1"), "f", ""); err != nil {
		t.Fatalf("A Upsert: %v", err)
	}
	outcome, err := nB.Upsert(ctx, "k", target, card("v1"), "f", "")
	if err != nil {
		t.Fatalf("B Upsert: %v", err)
	}
	if outcome != Unchanged {
		t.Errorf("B outcome = %s, want %s — B did not see A's card", outcome, Unchanged)
	}
	if len(fakeB.Calls) != 0 {
		t.Errorf("B made calls %v, want none", fakeB.Kinds())
	}
}

// A 304 carries no payload, so the cached body must be stored with the ETag or
// a conditional request saves quota and yields nothing.
func TestHTTPCacheRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	url := "https://api.github.com/repos/o/r/pulls?state=open"

	if _, _, ok, err := store.CachedResponse(ctx, url); ok || err != nil {
		t.Fatalf("empty cache reported ok=%v err=%v", ok, err)
	}
	body := []byte(`[{"number":1}]`)
	if err := store.SaveResponse(ctx, url, `W/"abc"`, body, time.Now()); err != nil {
		t.Fatalf("SaveResponse: %v", err)
	}
	etag, got, ok, err := store.CachedResponse(ctx, url)
	if err != nil || !ok {
		t.Fatalf("CachedResponse: ok=%v err=%v", ok, err)
	}
	if etag != `W/"abc"` || string(got) != string(body) {
		t.Errorf("cache = (%q, %s), want the stored etag and body", etag, got)
	}
}
