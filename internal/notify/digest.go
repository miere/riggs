package notify

import (
	"context"
	"errors"
	"fmt"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
)

// This file is the digest half of the ledger. Upsert (notify.go) maintains one
// message about one entity, keyed by that entity forever; a digest is one
// message about many, and which many changes underneath it. The lifecycles
// differ enough — a digest is created, rewritten as its membership moves, and
// then *deleted* — that folding them together would leave Upsert answering two
// questions at once.

// PostDigest posts a new digest and records where it landed.
//
// The key is allocated by the caller (see Store.NextPostKey), because the
// membership rows have to point at it and they are written in the same pass.
func (n *Notifier) PostDigest(ctx context.Context, key string, target slack.Target, d blockkit.Digest, text string) (slack.Ref, error) {
	ref, err := n.poster.Post(ctx, target, slack.Message{Text: text, Blocks: d.Blocks()})
	if err != nil {
		return slack.Ref{}, fmt.Errorf("posting digest %s: %w", key, err)
	}
	if err := n.save(ctx, key, target, ref, d.Fingerprint(), ""); err != nil {
		return slack.Ref{}, err
	}
	return ref, nil
}

// UpdateDigest rewrites an existing digest in place.
//
// A fingerprint match makes no Slack call at all, which is what keeps a
// per-minute pass from rewriting ten messages that say the same thing as last
// minute. A vanished message is reported rather than re-posted: unlike a card,
// a digest that someone deleted should stay deleted — its items will be picked
// up by the next new post on their own cooldown, and silently resurrecting a
// message the reader dismissed is the wrong answer.
func (n *Notifier) UpdateDigest(ctx context.Context, key string, target slack.Target, d blockkit.Digest, text string) (Outcome, error) {
	entry, found, err := n.store.Card(ctx, key)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("updating digest %s: no such post in the ledger", key)
	}
	if entry.Fingerprint == d.Fingerprint() {
		return Unchanged, nil
	}

	ref := slack.Ref{Channel: entry.Channel, TS: entry.TS}
	err = n.poster.Update(ctx, target, ref, slack.Message{Text: text, Blocks: d.Blocks()})
	switch {
	case err == nil:
		return Updated, n.save(ctx, key, target, ref, d.Fingerprint(), "")
	case errors.Is(err, slack.ErrMessageNotFound):
		return Gone, n.store.DeleteCard(ctx, key)
	default:
		return "", fmt.Errorf("updating digest %s: %w", key, err)
	}
}

// DeleteDigest removes a digest's message and forgets the post.
//
// A message that is already gone is success, not failure: the intent was for it
// not to be there. The ledger row is dropped either way, so a post that Slack
// has lost cannot strand the next pass trying to update it.
//
// One exception: a digest whose thread somebody replied in is KEPT. Deleting a
// Slack message deletes its whole thread with it, and an emptied digest is
// tidiness where a colleague's reply is work. Tidiness loses.
//
// "Somebody" excludes Riggs, which posts into a digest's own thread when it
// narrates an approval or reports a failed click. Counting those would retain
// every digest that ever saw a click.
//
// The ledger row is dropped for a retained digest too. Its only purpose is to
// let a later pass update or delete that message, and this decides we will
// never do either again — so the message is left as the record the thread hangs
// off, and Riggs stops managing it.
//
// Reported reports whether the message was actually deleted.
func (n *Notifier) DeleteDigest(ctx context.Context, key string, target slack.Target) (bool, error) {
	entry, found, err := n.store.Card(ctx, key)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	ref := slack.Ref{Channel: entry.Channel, TS: entry.TS}

	// A failure to read the thread must not fall through to the delete: not
	// knowing whether a conversation is there is not permission to destroy one.
	replied, err := n.poster.HasForeignReplies(ctx, target, ref)
	if err != nil {
		return false, fmt.Errorf("checking the thread on digest %s before deleting it: %w", key, err)
	}
	if replied {
		return false, n.store.DeleteCard(ctx, key)
	}

	if err := n.poster.Delete(ctx, target, ref); err != nil && !errors.Is(err, slack.ErrMessageNotFound) {
		return false, fmt.Errorf("deleting digest %s: %w", key, err)
	}
	return true, n.store.DeleteCard(ctx, key)
}
