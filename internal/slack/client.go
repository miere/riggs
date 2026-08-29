package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is Slack's Web API root.
const DefaultBaseURL = "https://slack.com/api"

// ErrMessageNotFound is returned when Slack rejects an update because the
// message is gone (deleted, or a stale ts from a previous install). The ledger
// treats it as "re-post and adopt the new ts" rather than as a failure — a
// deleted card must come back, not disappear silently.
var ErrMessageNotFound = errors.New("slack: message not found")

// Message is one outbound message: its blocks, and the fallback text.
type Message struct {
	// Text is the notification and fallback text. It is not optional: it is
	// what Slack shows in the sidebar and on mobile, and what an agent reading
	// the thread sees instead of "message updated".
	Text string
	// Blocks is the rendered Block Kit payload.
	Blocks []any
	// ThreadTS posts this message as a reply to that message, when set.
	ThreadTS string
}

// Ref identifies a posted message.
type Ref struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// Poster is the seam every notifying path goes through. Tests substitute a
// fake (see slacktest) so no test makes a live call.
type Poster interface {
	Post(ctx context.Context, target Target, msg Message) (Ref, error)
	Update(ctx context.Context, target Target, ref Ref, msg Message) error
	// Delete removes a message. A digest that empties out is deleted rather
	// than updated to an empty shell (§7c), which is the only reason this
	// exists.
	Delete(ctx context.Context, target Target, ref Ref) error
	// HasForeignReplies reports whether anyone but Riggs replied in a
	// message's thread. Deleting the message would take the thread with it,
	// so this is what stops a tidy-up destroying a conversation.
	HasForeignReplies(ctx context.Context, target Target, ref Ref) (bool, error)
}

// Doer is the HTTP seam, so tests can drive the client without a network.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// API is the live Slack Web API client.
//
// It speaks HTTP directly rather than through slack-go, even though slack-go is
// now a dependency for decoding inbound callbacks (§7b). Outbound stays here:
// the blocks are ordered structs so their encoded bytes are stable, and that
// stability is what makes blockkit's fingerprint — and the ledger's "update only
// when it actually changed" — mean anything. Owning the request also means
// owning 429 handling, which matters for a per-minute job.
type API struct {
	http    Doer
	baseURL string
	sleep   func(time.Duration)
	// maxAttempts bounds retries on 429 and 5xx.
	maxAttempts int
	// self remembers who this token is, for recognising our own messages.
	self selfCache
}

// NewAPI constructs a client over the default HTTP transport.
func NewAPI() *API {
	return &API{
		http:        &http.Client{Timeout: 30 * time.Second},
		baseURL:     DefaultBaseURL,
		sleep:       time.Sleep,
		maxAttempts: 3,
	}
}

// WithTransport overrides the HTTP seam, the base URL and the sleep function;
// intended for tests.
func (a *API) WithTransport(d Doer, baseURL string, sleep func(time.Duration)) *API {
	a.http, a.baseURL, a.sleep = d, baseURL, sleep
	return a
}

// Post sends a message, resolving a DM target to a conversation first.
func (a *API) Post(ctx context.Context, target Target, msg Message) (Ref, error) {
	channel, err := a.channelFor(ctx, target)
	if err != nil {
		return Ref{}, err
	}
	body := map[string]any{"channel": channel, "text": msg.Text}
	if msg.Blocks != nil {
		body["blocks"] = msg.Blocks
	}
	if msg.ThreadTS != "" {
		body["thread_ts"] = msg.ThreadTS
	}
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := a.call(ctx, target.BotToken, "chat.postMessage", body, &out); err != nil {
		return Ref{}, err
	}
	if out.TS == "" {
		return Ref{}, fmt.Errorf("slack: chat.postMessage returned no ts")
	}
	return Ref{Channel: firstNonEmpty(out.Channel, channel), TS: out.TS}, nil
}

// PostEphemeral sends a message only userID can see.
//
// It returns no Ref on purpose: an ephemeral message has a `message_ts` that
// cannot be updated, deleted or replied to, so recording one would invite a
// caller to try. Nothing about it goes in the ledger either — it is a remark to
// one person, not a card with a life of its own.
//
// This is what a failed click reports through. The alternative, a real message,
// puts one person's mistyped intent in front of a whole channel and leaves it
// there; the alternative to *that* is the daemon log, which is where this
// started.
func (a *API) PostEphemeral(ctx context.Context, target Target, userID string, msg Message) error {
	channel, err := a.channelFor(ctx, target)
	if err != nil {
		return err
	}
	body := map[string]any{"channel": channel, "user": userID, "text": msg.Text}
	if msg.Blocks != nil {
		body["blocks"] = msg.Blocks
	}
	if msg.ThreadTS != "" {
		body["thread_ts"] = msg.ThreadTS
	}
	return a.call(ctx, target.BotToken, "chat.postEphemeral", body, nil)
}

// Update replaces a message in place.
func (a *API) Update(ctx context.Context, target Target, ref Ref, msg Message) error {
	body := map[string]any{"channel": ref.Channel, "ts": ref.TS, "text": msg.Text}
	if msg.Blocks != nil {
		body["blocks"] = msg.Blocks
	}
	return a.call(ctx, target.BotToken, "chat.update", body, nil)
}

// Delete removes a message.
//
// A message that is already gone is reported as ErrMessageNotFound, which the
// caller treats as success: the intent was for it not to be there.
func (a *API) Delete(ctx context.Context, target Target, ref Ref) error {
	body := map[string]any{"channel": ref.Channel, "ts": ref.TS}
	return a.call(ctx, target.BotToken, "chat.delete", body, nil)
}

// channelFor resolves the conversation to post into: the named channel, or the
// admin's IM, opened on demand.
func (a *API) channelFor(ctx context.Context, target Target) (string, error) {
	if !target.IsDM() {
		return target.Channel, nil
	}
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	body := map[string]any{"users": target.AdminUserID}
	if err := a.call(ctx, target.BotToken, "conversations.open", body, &out); err != nil {
		return "", fmt.Errorf("opening a DM with %s: %w", target.AdminUserID, err)
	}
	if out.Channel.ID == "" {
		return "", fmt.Errorf("slack: conversations.open returned no channel for %s", target.AdminUserID)
	}
	return out.Channel.ID, nil
}

// call performs one Web API method, retrying on 429 and 5xx.
//
// Slack signals application errors with HTTP 200 and `"ok": false`, so the
// status code alone never tells you whether a post succeeded.
func (a *API) call(ctx context.Context, token, method string, body map[string]any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: encoding %s request: %w", method, err)
	}

	var lastErr error
	for attempt := 1; attempt <= a.maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			a.baseURL+"/"+method, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("slack: building %s request: %w", method, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("slack: %s: %w", method, err)
			if attempt < a.maxAttempts {
				a.sleep(backoff(attempt))
				continue
			}
			return lastErr
		}

		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("slack: %s: rate limited", method)
			if attempt < a.maxAttempts {
				a.sleep(retryAfter(resp, attempt))
				continue
			}
			return lastErr
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("slack: %s: HTTP %d", method, resp.StatusCode)
			if attempt < a.maxAttempts {
				a.sleep(backoff(attempt))
				continue
			}
			return lastErr
		}
		if readErr != nil {
			return fmt.Errorf("slack: reading %s response: %w", method, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("slack: %s: HTTP %d", method, resp.StatusCode)
		}

		var env struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("slack: %s returned non-JSON: %w", method, err)
		}
		if !env.OK {
			if env.Error == "message_not_found" {
				return ErrMessageNotFound
			}
			return fmt.Errorf("slack: %s failed: %s", method, env.Error)
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("slack: decoding %s response: %w", method, err)
			}
		}
		return nil
	}
	return lastErr
}

// retryAfter honours Slack's Retry-After header, falling back to a backoff.
func retryAfter(resp *http.Response, attempt int) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return backoff(attempt)
}

// backoff is a plain exponential step; the bound is maxAttempts, not the delay.
func backoff(attempt int) time.Duration {
	return time.Duration(attempt) * time.Second
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
