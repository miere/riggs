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
)

// Policy selects how a latch decides whether a threaded message may fire.
type Policy int

const (
	// PolicyOnce fires at most once until the latch is explicitly cleared.
	// This is the reviewer tag: one ping per episode of being asked to
	// review, no matter how many ticks pass.
	PolicyOnce Policy = iota
	// PolicyMinGap fires only when at least MinGap has passed since the last
	// firing. This is the idle nudge's floor, which stops a manual re-run
	// landing on top of a scheduled one.
	PolicyMinGap
)

// Latch names a one-shot or rate-limited threaded message.
type Latch struct {
	Name   string
	Policy Policy
	MinGap time.Duration
}

// Once builds a fire-once-until-cleared latch.
func Once(name string) Latch { return Latch{Name: name, Policy: PolicyOnce} }

// MinGap builds a rate-limited latch.
func MinGap(name string, d time.Duration) Latch {
	return Latch{Name: name, Policy: PolicyMinGap, MinGap: d}
}

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
func (n *Notifier) Upsert(ctx context.Context, key string, target slack.Target, card blockkit.Card, text string) (Outcome, error) {
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
		return Posted, n.save(ctx, key, target, ref, fingerprint)
	}

	if entry.Fingerprint == fingerprint {
		return Unchanged, nil
	}

	ref := slack.Ref{Channel: entry.Channel, TS: entry.TS}
	err = n.poster.Update(ctx, target, ref, msg)
	switch {
	case err == nil:
		return Updated, n.save(ctx, key, target, ref, fingerprint)
	case errors.Is(err, slack.ErrMessageNotFound):
		newRef, postErr := n.poster.Post(ctx, target, msg)
		if postErr != nil {
			return "", fmt.Errorf("re-posting vanished card %s: %w", key, postErr)
		}
		if err := n.store.ClearLatches(ctx, key); err != nil {
			return "", err
		}
		return Reposted, n.save(ctx, key, target, newRef, fingerprint)
	default:
		return "", fmt.Errorf("updating card %s: %w", key, err)
	}
}

// save records where the card is and what it looked like.
func (n *Notifier) save(ctx context.Context, key string, target slack.Target, ref slack.Ref, fingerprint string) error {
	return n.store.SaveCard(ctx, key, Entry{
		Profile:     target.Profile,
		Channel:     ref.Channel,
		TS:          ref.TS,
		Fingerprint: fingerprint,
		UpdatedAt:   n.now(),
	})
}

// Thread posts a reply on key's card, subject to latch.
//
// It reports whether the message was actually sent. false with a nil error is
// the ordinary case, not a failure: either the latch is closed, or there is no
// card to reply to. A nudge for a ticket that was never advertised has nowhere
// to go, and inventing a top-level message would be worse than staying quiet.
func (n *Notifier) Thread(ctx context.Context, key string, target slack.Target, text string, latch Latch) (bool, error) {
	entry, found, err := n.store.Card(ctx, key)
	if err != nil {
		return false, err
	}
	if !found || entry.TS == "" {
		return false, nil
	}

	open, err := n.latchOpen(ctx, key, latch)
	if err != nil || !open {
		return false, err
	}

	// Reply into the tracked message's own channel, not the caller's default:
	// the thread lives where the card is.
	threadTarget := target
	threadTarget.Channel = entry.Channel
	msg := slack.Message{Text: text, Blocks: blockkit.TextBlocks(text), ThreadTS: entry.TS}
	if _, err := n.poster.Post(ctx, threadTarget, msg); err != nil {
		return false, fmt.Errorf("threading on card %s: %w", key, err)
	}
	if err := n.store.SetLatch(ctx, key, latch.Name, n.now()); err != nil {
		return false, err
	}
	return true, nil
}

// latchOpen reports whether latch permits a message right now.
func (n *Notifier) latchOpen(ctx context.Context, key string, latch Latch) (bool, error) {
	if latch.Name == "" {
		return true, nil
	}
	firedAt, fired, err := n.store.LatchFiredAt(ctx, key, latch.Name)
	if err != nil {
		return false, err
	}
	if !fired {
		return true, nil
	}
	if latch.Policy == PolicyOnce {
		return false, nil
	}
	return n.now().Sub(firedAt) >= latch.MinGap, nil
}

// ClearLatch reopens a latch, so a once-per-episode message fires again on the
// next episode. The review queue calls this when a PR stops being reviewable,
// which is what makes a re-review request ping again.
func (n *Notifier) ClearLatch(ctx context.Context, key, name string) error {
	return n.store.ClearLatch(ctx, key, name)
}

// Card exposes the tracked entry, for callers that need the ts (to link to a
// thread) or the channel.
func (n *Notifier) Card(ctx context.Context, key string) (Entry, bool, error) {
	return n.store.Card(ctx, key)
}
