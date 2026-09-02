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

// A deleted card must come back, not vanish.
func TestUpsertRepostsWhenMessageIsGone(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()

	_, _ = n.Upsert(ctx, "k", target, card("v1"), "f", "")
	old, _, _ := store.Card(ctx, "k")

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
