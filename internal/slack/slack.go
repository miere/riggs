// Package slack resolves *where* a notification goes: which configured Slack
// account (profile) delivers it, and to which conversation.
//
// It is deliberately separate from the act of sending. Resolution is pure and
// total — it either yields a fully-determined Target or a typed error naming
// exactly what is missing — so the CLI, the MCP frontend and `riggs
// capabilities` all agree on what is deliverable without any of them opening a
// socket.
package slack

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/miere/riggs-mcp/internal/config"
)

// ErrNotConfigured is the typed sentinel every "you have not set this up"
// failure wraps. Tools translate it into an operator-facing hint at their
// boundary; nothing treats it as a crash.
var ErrNotConfigured = errors.New("not configured")

// Target is a fully-resolved delivery destination.
type Target struct {
	// Profile is the name of the resolved profile, always populated (the
	// caller may have passed "", meaning the default).
	Profile string
	// BotToken is the xoxb- token used for all posting.
	BotToken string
	// UserToken is the xoxp- token, empty unless the profile declares one. It
	// is only used by tools that must post *as* the admin rather than as the
	// app.
	UserToken string

	// Channel is the target conversation. Empty means "DM the admin", in
	// which case AdminUserID is populated and the sender opens the IM.
	Channel string
	// AdminUserID is the admin's Slack user id: the DM fallback target, and
	// the user threaded nudges tag.
	AdminUserID string
}

// IsDM reports whether this target resolves to a direct message to the admin
// rather than a named channel.
func (t Target) IsDM() bool { return t.Channel == "" }

// Describe renders the destination for logs and dry runs.
func (t Target) Describe() string {
	if t.IsDM() {
		return fmt.Sprintf("%s → DM %s", t.Profile, t.AdminUserID)
	}
	return fmt.Sprintf("%s → %s", t.Profile, t.Channel)
}

// Resolver turns (profile, channel) requests into Targets using the loaded
// configuration.
type Resolver struct {
	cfg *config.Config
}

// NewResolver builds a Resolver over cfg.
func NewResolver(cfg *config.Config) *Resolver { return &Resolver{cfg: cfg} }

// Resolve determines where a notification goes.
//
// profile empty selects config.DefaultProfile; an unknown or undefined profile
// is an error, including the default one — "if default is not defined it
// fails" is the specified behaviour, because silently not notifying is worse
// than a loud failure.
//
// channel empty falls back to a DM to the admin, which requires
// admin.slack-user-id to be set.
func (r *Resolver) Resolve(profile, channel string) (Target, error) {
	if r.cfg == nil {
		return Target{}, fmt.Errorf("slack: %w: no configuration loaded", ErrNotConfigured)
	}

	p, name, ok := r.cfg.Profile(profile)
	if !ok {
		return Target{}, fmt.Errorf("slack: %w: profile %q is not defined in %s%s",
			ErrNotConfigured, name, r.cfg.Path, r.available())
	}
	if strings.TrimSpace(p.BotToken) == "" {
		return Target{}, fmt.Errorf("slack: %w: profile %q has no bot-token (check the ${ENV} reference in %s)",
			ErrNotConfigured, name, r.cfg.Path)
	}

	target := Target{
		Profile:     name,
		BotToken:    p.BotToken,
		UserToken:   p.UserToken,
		Channel:     strings.TrimSpace(channel),
		AdminUserID: r.cfg.Admin.SlackUserID,
	}
	if target.IsDM() && target.AdminUserID == "" {
		return Target{}, fmt.Errorf("slack: %w: no channel given and admin.slack-user-id is unset in %s, so there is nobody to DM",
			ErrNotConfigured, r.cfg.Path)
	}
	return target, nil
}

// available renders the configured profile names as a hint, so a typo'd
// --slack-profile says what the valid answers are.
func (r *Resolver) available() string {
	names := r.cfg.ProfileNames()
	if len(names) == 0 {
		return " (no profiles are configured)"
	}
	sort.Strings(names)
	return " (configured: " + strings.Join(names, ", ") + ")"
}
