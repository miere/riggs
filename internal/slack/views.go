package slack

import (
	"context"
	"fmt"
)

// ViewPublisher publishes a user's App Home view.
//
// It is a separate interface from Poster rather than another method on it. The
// Home tab is a per-user surface with no conversation and no message id, so
// none of Poster's vocabulary — a Target, a Ref, an update-in-place — applies
// to it; and the notifying tools, which are the only implementers of Poster,
// have no business growing a method they will never call.
type ViewPublisher interface {
	// PublishHome replaces userID's Home tab with view. view is the whole
	// `{"type":"home","blocks":[…]}` payload.
	PublishHome(ctx context.Context, token, userID string, view any) error
}

// ViewOpener opens a modal over whatever surface a click came from.
//
// Separate from ViewPublisher for the same reason that one is separate from
// Poster: a modal is addressed by a TRIGGER, not by a user and not by a
// conversation, and the trigger expires. Nothing that publishes a Home tab has
// one, and nothing that has one is publishing a Home tab.
type ViewOpener interface {
	// OpenView opens view against triggerID.
	OpenView(ctx context.Context, token, triggerID string, view any) error
}

// OpenView implements ViewOpener against the live Web API.
//
// Slack gives a trigger_id about three seconds to live. That is why the daemon
// acknowledges a socket request before handling it (§7b) and why this handler
// does no work first: a modal opened after a ledger read and a GitHub call is a
// modal that does not open, and the failure Slack reports for it says only
// "expired_trigger_id".
func (a *API) OpenView(ctx context.Context, token, triggerID string, view any) error {
	if triggerID == "" {
		return fmt.Errorf("slack: views.open needs a trigger id")
	}
	body := map[string]any{"trigger_id": triggerID, "view": view}
	return a.call(ctx, token, "views.open", body, nil)
}

// PublishHome implements ViewPublisher against the live Web API.
//
// It takes a bare bot token rather than a Target because there is no
// destination to resolve: a Home tab belongs to a (user, app) pair, and the app
// is whichever one the token identifies. The daemon has exactly those two
// values at the point it handles app_home_opened.
func (a *API) PublishHome(ctx context.Context, token, userID string, view any) error {
	if userID == "" {
		return fmt.Errorf("slack: views.publish needs a user id")
	}
	body := map[string]any{"user_id": userID, "view": view}
	return a.call(ctx, token, "views.publish", body, nil)
}
