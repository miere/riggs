package pullrequest

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Asker implements "Ask for Code Review".
//
// It drops one message and stops. Nothing is delegated, no agent is started,
// no review is performed — a human reads the ask and decides. That is the whole
// feature, and it is deliberately smaller than the delegate-to-agent rule it
// replaces.
type Asker struct {
	poster slack.Poster
	// reviewer is the Slack user tagged in the ask.
	reviewer string
	// channel is where the ask is posted. Empty DMs the reviewer.
	channel string
	// prompt is the body.
	prompt string
}

// NewAsker builds the asker.
func NewAsker(poster slack.Poster, reviewer, channel, prompt string) *Asker {
	return &Asker{poster: poster, reviewer: reviewer, channel: channel, prompt: prompt}
}

// AskResult reports where the ask went.
type AskResult struct {
	Ref      string `json:"ref"`
	Reviewer string `json:"reviewer"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
}

// Ask posts the review request for ref.
//
// target supplies the credentials; the destination is this asker's own
// configuration, not the caller's — the point of the action is to reach
// somebody who is not looking at the digest.
func (a *Asker) Ask(ctx context.Context, ref string, target slack.Target) (AskResult, error) {
	if a.reviewer == "" {
		return AskResult{}, fmt.Errorf("cannot ask for a review: no reviewer configured (set review-request.user-id or admin.slack-user-id)")
	}
	if _, _, err := SplitRef(ref); err != nil {
		return AskResult{}, err
	}

	dest := target
	dest.Channel = a.channel
	if dest.Channel == "" {
		// A DM to the reviewer, who is not necessarily the admin — so the DM
		// target is overridden rather than left to fall through to admin.
		dest.AdminUserID = a.reviewer
	}

	text := AskText(a.reviewer, ref, a.prompt)
	ref2, err := a.poster.Post(ctx, dest, slack.Message{
		Text:   text,
		Blocks: blockkit.TextBlocks(text),
	})
	if err != nil {
		return AskResult{}, fmt.Errorf("asking %s for a review of %s: %w", a.reviewer, ref, err)
	}
	return AskResult{Ref: ref, Reviewer: a.reviewer, Channel: ref2.Channel, TS: ref2.TS}, nil
}

// String renders the result for the CLI.
func (r AskResult) String() string {
	return fmt.Sprintf("Asked <@%s> to review %s.", r.Reviewer, r.Ref)
}

// AskText composes the message.
//
// It names no tool and claims no authorship. The ask reads as though the person
// who clicked wrote it, because they did: they chose the pull request, the
// reviewer and the prompt. Saying otherwise would put a machine's name on
// somebody else's request.
func AskText(reviewer, ref, prompt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<@%s> %s\n\n", reviewer, strings.TrimSpace(prompt))
	fmt.Fprintf(&b, "*%s* — %s", ref, RefURL(ref))
	return b.String()
}

// RefURL renders the browser URL for owner/repo#number.
//
// Derived rather than fetched: the click carries only the ref, and a GitHub
// round-trip to learn a URL whose shape is fixed would make the button slower
// for nothing.
func RefURL(ref string) string {
	repo, number, err := SplitRef(ref)
	if err != nil {
		return ref
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}
