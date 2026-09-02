package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Outcome reports what Upsert actually did, so a caller can log the difference
// between "nothing to do" and "posted a new card".
type Outcome string

const (
	// Unchanged means the rendered card matched the stored fingerprint and no
	// Slack call was made at all.
	Unchanged Outcome = "unchanged"
	// Updated means the existing message was edited in place.
	Updated Outcome = "updated"
	// Posted means a new message was created.
	Posted Outcome = "posted"
	// Reposted means the tracked message was gone, so a replacement was
	// posted and its latches cleared.
	Reposted Outcome = "reposted"
	// Gone means the tracked message had been deleted and was not replaced.
	// Only a digest reports this: a card that vanishes is re-posted, but a
	// digest the reader dismissed should stay dismissed.
	Gone Outcome = "gone"
)

// Notifier posts and maintains cards, keeping the ledger in step.
type Notifier struct {
	store  *Store
	poster slack.Poster
	now    func() time.Time
}

// New builds a Notifier over a store and a Slack poster.
func New(store *Store, poster slack.Poster) *Notifier {
	return &Notifier{store: store, poster: poster, now: time.Now}
}

// WithClock overrides the clock; intended for tests.
func (n *Notifier) WithClock(now func() time.Time) *Notifier {
	n.now = now
	return n
}

// Upsert brings key's card into line with card.
//
//   - unknown key            -> post, and remember where it landed
//   - fingerprint unchanged  -> do nothing, and make no Slack call
//   - fingerprint changed    -> edit the message in place
//   - message gone           -> post a replacement and clear the latches,
//     because the new message carries none of the
//     old one's threaded replies
//
// text is the notification/fallback text: what Slack shows in the sidebar, and
// what an agent reading the thread sees.
func (n *Notifier) Upsert(ctx context.Context, key string, target slack.Target, card blockkit.Card, text string, state string) (Outcome, error) {
	entry, found, err := n.store.Card(ctx, key)
	if err != nil {
		return "", err
	}
	fingerprint := card.Fingerprint()
	msg := slack.Message{Text: text, Blocks: card.Blocks()}

	if !found {
		ref, err := n.poster.Post(ctx, target, msg)
		if err != nil {
			return "", fmt.Errorf("posting card %s: %w", key, err)
		}
		return Posted, n.save(ctx, key, target, ref, fingerprint, state)
	}

	if entry.Fingerprint == fingerprint && entry.State == state {
		return Unchanged, nil
	}

	ref := slack.Ref{Channel: entry.Channel, TS: entry.TS}
	err = n.poster.Update(ctx, target, ref, msg)
	switch {
	case err == nil:
		return Updated, n.save(ctx, key, target, ref, fingerprint, state)
	case errors.Is(err, slack.ErrMessageNotFound):
		newRef, postErr := n.poster.Post(ctx, target, msg)
		if postErr != nil {
			return "", fmt.Errorf("re-posting vanished card %s: %w", key, postErr)
		}
		if err := n.store.ClearLatches(ctx, key); err != nil {
			return "", err
		}
		return Reposted, n.save(ctx, key, target, newRef, fingerprint, state)
	default:
		return "", fmt.Errorf("updating card %s: %w", key, err)
	}
}

// save records where the card is and what it looked like.
func (n *Notifier) save(ctx context.Context, key string, target slack.Target, ref slack.Ref, fingerprint, state string) error {
	return n.store.SaveCard(ctx, key, Entry{
		Profile:     target.Profile,
		Channel:     ref.Channel,
		TS:          ref.TS,
		Fingerprint: fingerprint,
		State:       state,
		UpdatedAt:   n.now(),
	})
}

// Card exposes the tracked entry, for callers that need the ts (to link to a
// thread) or the channel.
func (n *Notifier) Card(ctx context.Context, key string) (Entry, bool, error) {
	return n.store.Card(ctx, key)
}
