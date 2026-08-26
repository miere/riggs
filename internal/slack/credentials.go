package slack

import (
	"fmt"
	"strings"
)

// Credentials is one profile's token pair, resolved for a listener rather than
// for a send.
//
// Resolve answers "where does this notification go", which requires a
// conversation. A Socket Mode listener has no conversation — it is attached to
// an app, not a channel — so it needs the tokens alone. Asking Resolve for them
// would mean inventing a destination just to get past its checks.
type Credentials struct {
	// Profile is the resolved profile name.
	Profile string
	// BotToken is the xoxb- token, used for the Web API calls a handler makes
	// in response to a click.
	BotToken string
	// AppToken is the xapp- token that opens the Socket Mode connection.
	AppToken string
}

// Credentials resolves the tokens for a listening profile.
//
// Both tokens are required, and a missing one is a typed ErrNotConfigured
// naming which: a daemon that starts without them would sit silently connected
// to nothing, which is exactly the failure mode §7 rejects for the send path.
func (r *Resolver) Credentials(profile string) (Credentials, error) {
	if r.cfg == nil {
		return Credentials{}, fmt.Errorf("slack: %w: no configuration loaded", ErrNotConfigured)
	}
	p, name, ok := r.cfg.Profile(profile)
	if !ok {
		return Credentials{}, fmt.Errorf("slack: %w: profile %q is not defined in %s%s",
			ErrNotConfigured, name, r.cfg.Path, r.available())
	}

	creds := Credentials{
		Profile:  name,
		BotToken: strings.TrimSpace(p.BotToken),
		AppToken: strings.TrimSpace(p.AppToken),
	}
	if creds.BotToken == "" {
		return Credentials{}, fmt.Errorf("slack: %w: profile %q has no bot-token (check the ${ENV} reference in %s)",
			ErrNotConfigured, name, r.cfg.Path)
	}
	if creds.AppToken == "" {
		return Credentials{}, fmt.Errorf("slack: %w: profile %q has no app-token, so it cannot open a Socket Mode connection (check the ${ENV} reference in %s)",
			ErrNotConfigured, name, r.cfg.Path)
	}
	return creds, nil
}
