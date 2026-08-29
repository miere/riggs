package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/miere/riggs-mcp/internal/blockkit"
)

func digest(rows ...blockkit.Row) blockkit.Digest {
	return blockkit.Digest{Title: "Waiting on you", Rows: rows}
}

// postDigest posts one and hands back where it landed.
func postDigest(t *testing.T, n *Notifier) string {
	t.Helper()
	ref, err := n.PostDigest(context.Background(), "d:1", target,
		digest(blockkit.Row{BlockID: "o/r#1", Title: "a row"}), "fallback")
	if err != nil {
		t.Fatalf("PostDigest: %v", err)
	}
	return ref.TS
}

// The ordinary case: nobody replied, so an emptied digest goes away.
func TestDeleteDigestDeletesAnUntouchedPost(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()
	postDigest(t, n)
	fake.Reset()

	deleted, err := n.DeleteDigest(ctx, "d:1", target)
	if err != nil {
		t.Fatalf("DeleteDigest: %v", err)
	}
	if !deleted {
		t.Error("deleted = false, want the message removed")
	}
	if len(fake.Deleted()) != 1 {
		t.Errorf("slack calls = %v, want one delete", fake.Kinds())
	}
	if _, found, _ := store.Card(ctx, "d:1"); found {
		t.Error("the ledger row survived the delete")
	}
}

// The exception. Deleting a Slack message deletes its thread with it, so a
// digest somebody replied in is kept even once it has emptied out.
func TestDeleteDigestKeepsAPostWithReplies(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()
	ts := postDigest(t, n)
	fake.Threaded = map[string]bool{ts: true}
	fake.Reset()

	deleted, err := n.DeleteDigest(ctx, "d:1", target)
	if err != nil {
		t.Fatalf("DeleteDigest: %v", err)
	}
	if deleted {
		t.Error("deleted = true, want the reply to have saved the message")
	}
	if len(fake.Deleted()) != 0 {
		t.Errorf("slack calls = %v, want no delete at all", fake.Kinds())
	}
	// Riggs stops managing it: the row exists only to update or delete the
	// message, and it will never do either again.
	if _, found, _ := store.Card(ctx, "d:1"); found {
		t.Error("the ledger row survived, so a later pass would try again")
	}
}

// Not knowing whether a conversation is there is not permission to destroy
// one, so a failed thread read stops the delete rather than falling through.
func TestDeleteDigestWillNotDeleteWhenTheThreadCannotBeRead(t *testing.T) {
	n, fake, store := harness(t)
	ctx := context.Background()
	postDigest(t, n)
	fake.Reset()
	fake.RepliesErr = errors.New("missing_scope")

	deleted, err := n.DeleteDigest(ctx, "d:1", target)
	if err == nil {
		t.Fatal("DeleteDigest = nil error, want the failure surfaced")
	}
	if deleted {
		t.Error("deleted = true after a failed check")
	}
	if len(fake.Deleted()) != 0 {
		t.Errorf("slack calls = %v, want no delete on an unreadable thread", fake.Kinds())
	}
	if _, found, _ := store.Card(ctx, "d:1"); !found {
		t.Error("the ledger row was dropped, which would strand the message")
	}
}

// A post the ledger never knew about is a no-op, and costs no Slack call.
func TestDeleteDigestOnAnUnknownKeyIsSilent(t *testing.T) {
	n, fake, _ := harness(t)
	deleted, err := n.DeleteDigest(context.Background(), "d:nope", target)
	if err != nil || deleted {
		t.Fatalf("DeleteDigest = (%v, %v), want (false, nil)", deleted, err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("slack calls = %v, want none", fake.Kinds())
	}
}
