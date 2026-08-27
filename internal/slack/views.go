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
