package slack

import (
	slackgo "github.com/slack-go/slack"
)

// Interaction is Riggs' view of one Slack interactive callback.
//
// It is deliberately narrower than slack-go's InteractionCallback. A handler
// needs to know which control was operated, on which row, in which message —
// and nothing else. Keeping the router's vocabulary this small is what stops
// the dispatch table growing a dependency on the SDK's shape.
//
// Intent and Item are the two halves of the identity, and they come from
// different places on purpose:
//
//   - Intent is the bare token in the control's value ("approve_merge"), which
//     is what the router matches on. It never varies per row, so the table can
//     compare it exactly.
//   - Item is the per-row identity, read from the block_id. An overflow click
//     reports its own block_id but not its siblings' values, so the block_id is
//     the only place a per-row reference can ride.
type Interaction struct {
	// ActionID is the control's action_id ("pr_overflow").
	ActionID string
	// Intent is the selected option's value for an overflow, or the button's
	// value for a button.
	Intent string
	// Item is the block_id of the block the control lives on — for the bulk
	// digest, the owner/repo#number of the row that was clicked.
	Item string
	// Channel is where the message lives.
	Channel string
	// MessageTS identifies the message the control is attached to.
	MessageTS string
	// UserID is who clicked.
	UserID string
	// Raw is the undecoded callback, for a handler that needs more than this.
	Raw slackgo.InteractionCallback
}

// DecodeInteraction lifts the first block action out of a callback.
//
// ok is false for a callback carrying no block action at all (a view
// submission, a shortcut): those are not errors, they are simply not ours, and
// the daemon acknowledges and drops them.
//
// Only the first action is read. Slack sends an array because a single
// interaction can in principle carry several, but every control Riggs renders
// is a one-at-a-time button or overflow.
func DecodeInteraction(cb slackgo.InteractionCallback) (Interaction, bool) {
	if cb.Type != slackgo.InteractionTypeBlockActions {
		return Interaction{}, false
	}
	actions := cb.ActionCallback.BlockActions
	if len(actions) == 0 {
		return Interaction{}, false
	}
	a := actions[0]

	in := Interaction{
		ActionID:  a.ActionID,
		Intent:    a.Value,
		Item:      a.BlockID,
		Channel:   cb.Channel.ID,
		MessageTS: cb.Message.Timestamp,
		UserID:    cb.User.ID,
		Raw:       cb,
	}
	// An overflow carries its intent on the chosen option, not on the element.
	if a.SelectedOption.Value != "" {
		in.Intent = a.SelectedOption.Value
	}
	// A container message ts is the more reliable source: cb.Message is absent
	// on some surfaces, and the ledger keys on the ts, so an empty one would
	// strand the update.
	if in.MessageTS == "" {
		in.MessageTS = cb.Container.MessageTs
	}
	return in, true
}
