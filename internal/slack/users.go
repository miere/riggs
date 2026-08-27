package slack

import (
	"context"
	"fmt"
	"strings"
)

// A Slack mention only works with an *id*: `<@U0B6HK02YBB>`. A handle typed by
// a human — `@murtaugh`, or `murtaugh` — renders as literal text, notifies
// nobody, and reports no error. That is the worst way for a review request to
// fail, so a configured reviewer is normalised and, when necessary, resolved.

// UserRef is a configured reference to a Slack user, before resolution.
type UserRef struct {
	// ID is set when the reference was already an id, or a `<@ID>` mention.
	ID string
	// Handle is set otherwise: a display name or username, without its `@`.
	Handle string
}

// IsID reports whether the reference needs no lookup.
func (r UserRef) IsID() bool { return r.ID != "" }

// ParseUserRef normalises whatever the config holds.
//
// Accepted, because all three are things a person reasonably writes:
//
//	U0B6HK02YBB      an id
//	<@U0B6HK02YBB>   a mention, as copied out of Slack
//	@murtaugh        a handle
//	murtaugh         a handle
//
// An id is recognised by shape: Slack user ids start with U, W or B and are
// otherwise upper-case alphanumeric. A handle cannot look like one, because
// Slack handles are lower-cased.
func ParseUserRef(s string) UserRef {
	s = strings.TrimSpace(s)
	if s == "" {
		return UserRef{}
	}
	// `<@U123>` or `<@U123|name>`, as pasted from Slack.
	if strings.HasPrefix(s, "<@") && strings.HasSuffix(s, ">") {
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "<@"), ">")
		if i := strings.IndexByte(inner, '|'); i >= 0 {
			inner = inner[:i]
		}
		s = inner
	}
	s = strings.TrimPrefix(s, "@")
	if looksLikeUserID(s) {
		return UserRef{ID: s}
	}
	return UserRef{Handle: s}
}

// looksLikeUserID reports whether s has the shape of a Slack user id.
func looksLikeUserID(s string) bool {
	// Bounded loosely. The real discriminator is the shape — U/W/B followed by
	// upper-case alphanumerics — because Slack lower-cases usernames, so a
	// handle cannot look like this. Length is only here to reject noise.
	if len(s) < 5 || len(s) > 15 {
		return false
	}
	switch s[0] {
	case 'U', 'W', 'B':
	default:
		return false
	}
	for _, r := range s[1:] {
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if !isUpper && !isDigit {
			return false
		}
	}
	return true
}

// LookupUserID resolves a handle to a user id via users.list.
//
// Slack has no lookup-by-handle endpoint, so the directory is paged and matched
// on username and display name, case-insensitively. It is called only when the
// configured value is not already an id, which on a correctly-configured
// machine is never.
//
// A workspace with no matching member is an error naming the handle: the
// alternative is posting a mention that reaches nobody.
func (a *API) LookupUserID(ctx context.Context, target Target, handle string) (string, error) {
	want := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	if want == "" {
		return "", fmt.Errorf("slack: no handle to resolve")
	}

	cursor := ""
	for page := 0; page < 20; page++ { // bounded: a runaway cursor must not loop forever
		body := map[string]any{"limit": 200}
		if cursor != "" {
			body["cursor"] = cursor
		}
		var out struct {
			Members []struct {
				ID      string `json:"id"`
				Name    string `json:"name"`
				Deleted bool   `json:"deleted"`
				Profile struct {
					DisplayName string `json:"display_name"`
					RealName    string `json:"real_name"`
				} `json:"profile"`
			} `json:"members"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := a.call(ctx, target.BotToken, "users.list", body, &out); err != nil {
			return "", fmt.Errorf("resolving @%s: %w (the app may lack the users:read scope)", want, err)
		}

		for _, m := range out.Members {
			if m.Deleted {
				continue
			}
			for _, candidate := range []string{m.Name, m.Profile.DisplayName, m.Profile.RealName} {
				if strings.ToLower(strings.TrimSpace(candidate)) == want {
					return m.ID, nil
				}
			}
		}

		cursor = out.Metadata.NextCursor
		if cursor == "" {
			break
		}
	}
	return "", fmt.Errorf("slack: no member of this workspace is called %q", handle)
}
