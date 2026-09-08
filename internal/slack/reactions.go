package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Reactions: the third thing Riggs does to a message, after posting it and
// updating it.
//
// They exist because a click needs an answer and a thread reply is the wrong
// one. A reply is a message — it notifies, it occupies a line, it accumulates,
// and three of them under one digest say less than one glyph on the digest
// itself. A reaction is the smallest acknowledgement Slack has, it sits on the
// thing it is about, and it is replaced rather than appended (see
// internal/comms).

// ErrNoReaction is returned when Slack refuses a removal because the reaction
// is not there.
//
// It is a typed error rather than a failure because to the state machine it
// means "already gone", which is the outcome it wanted. The same is true in the
// other direction: adding a reaction twice is not an error worth propagating,
// so ErrAlreadyReacted exists for the same reason.
var (
	ErrNoReaction     = errors.New("slack: no such reaction on that message")
	ErrAlreadyReacted = errors.New("slack: that reaction is already there")
	// ErrInvalidEmoji is Slack rejecting a name it does not know. It is typed
	// because the admin configured that name and is the only person who can fix
	// it, so the message they get has to say which one was wrong.
	ErrInvalidEmoji = errors.New("slack: no emoji by that name in this workspace")
)

// Reactor adds and removes reactions on a message.
//
// It is a separate interface from Poster for the same reason ViewPublisher is:
// nothing that reacts is composing a message, and every implementer of Poster
// would otherwise grow two methods it never calls. The state machine takes this
// alone, which is what lets it be tested with a fake that does nothing but
// record.
type Reactor interface {
	// AddReaction places name on a message. An emoji already there is success.
	AddReaction(ctx context.Context, target Target, ref Ref, name string) error
	// RemoveReaction takes name off. One that is not there is success.
	//
	// Slack scopes this to the CALLING user's own reaction, which is the
	// property the whole design leans on: Riggs removing "the previous state"
	// can never take a colleague's ✋ off a digest, however the two happen to
	// overlap.
	RemoveReaction(ctx context.Context, target Target, ref Ref, name string) error
}

// AddReaction implements Reactor against the live Web API.
//
// `already_reacted` is swallowed rather than returned. The caller's intent is
// "this emoji is on that message", and it is; failing here would make a retried
// transition report a problem it had already solved.
func (a *API) AddReaction(ctx context.Context, target Target, ref Ref, name string) error {
	name = NormaliseEmojiName(name)
	if name == "" {
		return fmt.Errorf("slack: reactions.add needs an emoji name")
	}
	body := map[string]any{"channel": ref.Channel, "timestamp": ref.TS, "name": name}
	err := a.call(ctx, target.BotToken, "reactions.add", body, nil)
	if errors.Is(err, ErrAlreadyReacted) {
		return nil
	}
	return err
}

// RemoveReaction implements Reactor against the live Web API.
//
// `no_reaction` is swallowed, for the mirror of AddReaction's reason: the
// caller wants the emoji gone, and it is. That is what makes the state machine
// safe to run against a message somebody has already tidied by hand, and safe
// to run twice.
func (a *API) RemoveReaction(ctx context.Context, target Target, ref Ref, name string) error {
	name = NormaliseEmojiName(name)
	if name == "" {
		return fmt.Errorf("slack: reactions.remove needs an emoji name")
	}
	body := map[string]any{"channel": ref.Channel, "timestamp": ref.TS, "name": name}
	err := a.call(ctx, target.BotToken, "reactions.remove", body, nil)
	if errors.Is(err, ErrNoReaction) || errors.Is(err, ErrMessageNotFound) {
		return nil
	}
	return err
}

// NormaliseEmojiName strips the colons people paste around a shortcode.
//
// `:tada:` is how an emoji is written everywhere a human types one — in Slack,
// in a commit message, in this file — and `reactions.add` is the one place that
// takes the bare name. Absorbing the colons here means the Customisation modal
// accepts what somebody actually copies, rather than rejecting it with a
// message about a syntax nobody sees.
func NormaliseEmojiName(name string) string {
	// Space, then colons, then space again. The last pass is not belt and
	// braces: `: :` trims to a single space under the obvious two-step order,
	// which is not empty, so the guard above would let it through to Slack.
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(name), ":"))
}
