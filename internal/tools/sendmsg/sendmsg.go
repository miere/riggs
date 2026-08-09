// Package sendmsg exposes a raw Slack post as a tool. It is the direct
// replacement for `murtaugh slack send-msg`, and the smallest thing that
// exercises profile resolution and the live client end to end.
package sendmsg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/miere/riggs-mcp/internal/slack"
)

// Tool posts a message through a configured Slack profile.
type Tool struct {
	resolver *slack.Resolver
	poster   slack.Poster
}

// New builds the tool over a profile resolver and a poster.
func New(resolver *slack.Resolver, poster slack.Poster) *Tool {
	return &Tool{resolver: resolver, poster: poster}
}

// Name is the registry key.
func (t *Tool) Name() string { return "slack.send-msg" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string {
	return "Post a message to Slack through a configured profile."
}

// InputSchema declares the parameters. slack_profile and slack_channel are the
// two every notifying tool carries; they are ordinary properties rather than
// global flags so an MCP caller can express them at all.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"body": {
				Type:        "string",
				Description: "Message text. Also the notification and fallback text when blocks are given.",
			},
			"blocks": {
				Type:        "string",
				Description: "Optional Block Kit payload, as a JSON array.",
			},
			"thread": {
				Type:        "string",
				Description: "Optional parent message ts; posts this as a threaded reply.",
			},
			"slack_profile": {
				Type:        "string",
				Description: "Which configured Slack account to post through. Defaults to \"default\".",
			},
			"slack_channel": {
				Type:        "string",
				Description: "Target channel id. Omitted, the message is a DM to the configured admin.",
			},
		},
		Required: []string{"body"},
	}
}

// Result is what the tool returns: where the message landed.
type Result struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Profile string `json:"profile"`
}

// String renders the result for a human.
func (r Result) String() string {
	return fmt.Sprintf("posted to %s at %s (profile %s)", r.Channel, r.TS, r.Profile)
}

// Invoke resolves the target and posts.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	body, _ := args["body"].(string)
	if body == "" {
		return nil, fmt.Errorf("body is required")
	}
	profile, _ := args["slack_profile"].(string)
	channel, _ := args["slack_channel"].(string)
	thread, _ := args["thread"].(string)

	target, err := t.resolver.Resolve(profile, channel)
	if err != nil {
		return nil, err
	}

	msg := slack.Message{Text: body, ThreadTS: thread}
	if raw, _ := args["blocks"].(string); raw != "" {
		var blocks []any
		if err := json.Unmarshal([]byte(raw), &blocks); err != nil {
			return nil, fmt.Errorf("blocks is not a JSON array: %w", err)
		}
		msg.Blocks = blocks
	}

	ref, err := t.poster.Post(ctx, target, msg)
	if err != nil {
		return nil, err
	}
	return Result{Channel: ref.Channel, TS: ref.TS, Profile: target.Profile}, nil
}
