package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// selfCache remembers the bot user id per token.
//
// The identity behind a token never changes, and the lookup is only ever
// needed to answer "did we write this?" — so it is read once and kept, rather
// than added to every thread check.
type selfCache struct {
	mu sync.Mutex
	id map[string]string
}

// Self returns the bot user id the token belongs to.
//
// Riggs needs to recognise its own messages: it narrates approvals and reports
// failures into a digest's own thread, so "this thread has replies" is not the
// same question as "somebody replied".
func (a *API) Self(ctx context.Context, token string) (string, error) {
	a.self.mu.Lock()
	if id, ok := a.self.id[token]; ok {
		a.self.mu.Unlock()
		return id, nil
	}
	a.self.mu.Unlock()

	var out struct {
		UserID string `json:"user_id"`
	}
	if err := a.call(ctx, token, "auth.test", map[string]any{}, &out); err != nil {
		return "", fmt.Errorf("resolving our own bot identity: %w", err)
	}
	if out.UserID == "" {
		return "", errors.New("slack: auth.test returned no user_id")
	}

	a.self.mu.Lock()
	if a.self.id == nil {
		a.self.id = map[string]string{}
	}
	a.self.id[token] = out.UserID
	a.self.mu.Unlock()
	return out.UserID, nil
}

// repliesPageLimit is how many thread messages one check reads.
//
// The answer is a yes/no, so the first page is enough: a thread whose opening
// replies are all ours and whose 51st is a colleague's is not a shape this
// codebase can produce — Riggs posts at most a couple of lines per thread.
const repliesPageLimit = 50

// HasForeignReplies reports whether anyone other than Riggs has replied in a
// message's thread.
//
// "Other than Riggs" is the whole point. Riggs posts into a digest's own
// thread on two paths — narrating an approval, and reporting a failed click —
// so counting every reply would mark almost every digest that ever saw a click
// as conversational, and nothing would ever be tidied away again.
//
// A thread that no longer exists has no replies to protect, so a vanished
// message answers false rather than failing: the caller is about to delete it
// anyway.
func (a *API) HasForeignReplies(ctx context.Context, target Target, ref Ref) (bool, error) {
	self, err := a.Self(ctx, target.BotToken)
	if err != nil {
		return false, err
	}

	var out struct {
		Messages []struct {
			TS     string `json:"ts"`
			User   string `json:"user"`
			BotID  string `json:"bot_id"`
			AppID  string `json:"app_id"`
			SubTyp string `json:"subtype"`
		} `json:"messages"`
	}
	body := map[string]any{
		"channel": ref.Channel,
		"ts":      ref.TS,
		"limit":   repliesPageLimit,
	}
	if err := a.call(ctx, target.BotToken, "conversations.replies", body, &out); err != nil {
		if errors.Is(err, ErrMessageNotFound) || isThreadGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading the thread on %s: %w", ref.TS, err)
	}

	for _, m := range out.Messages {
		if m.TS == ref.TS {
			continue // the parent is the card itself, not a reply
		}
		if m.User == self {
			continue // our own narration
		}
		return true, nil
	}
	return false, nil
}

// isThreadGone recognises Slack's other ways of saying the message is not
// there. `conversations.replies` answers `thread_not_found` where `chat.update`
// would answer `message_not_found`, and a channel we were removed from is gone
// for this purpose too.
func isThreadGone(err error) bool {
	if err == nil {
		return false
	}
	for _, s := range []string{"thread_not_found", "channel_not_found"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}
	return false
}
