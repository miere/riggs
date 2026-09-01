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
	// TriggerID is the short-lived token a modal must be opened against. It is
	// present on a click and empty on a view submission, which has no surface
	// left to open anything over.
	//
	// It lives about three seconds. A handler that means to open a modal must
	// do it first and do the work afterwards.
	TriggerID string
	// Raw is the undecoded callback, for a handler that needs more than this.
	Raw slackgo.InteractionCallback
}

// ViewSubmitIntent is the intent every view submission carries.
//
// A submission has no control and no chosen option, so there is nothing on it
// to vary: the modal's callback_id says which modal, and this says "it was
// submitted". Together they are a route the same table matches, which is the
// point — a submission is a control Riggs rendered, operated by a human, and
// delivered to the app that drew it. That it arrived from a modal rather than a
// message changes where it came from, not what dispatching it means.
const ViewSubmitIntent = "submit"

// DecodeInteraction lifts the first block action out of a callback, or decodes
// a view submission.
//
// ok is false for anything else (a shortcut, a message action): those are not
// errors, they are simply not ours, and the daemon acknowledges and drops them.
//
// Only the first action is read. Slack sends an array because a single
// interaction can in principle carry several, but every control Riggs renders
// is a one-at-a-time button or overflow.
func DecodeInteraction(cb slackgo.InteractionCallback) (Interaction, bool) {
	if cb.Type == slackgo.InteractionTypeViewSubmission {
		return decodeViewSubmission(cb)
	}
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
		TriggerID: cb.TriggerID,
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

// decodeViewSubmission maps a submitted modal onto the same vocabulary a click
// uses, so one routing table answers both.
//
// The callback_id becomes the ActionID and the private_metadata becomes the
// Item, which is exactly the split a click already makes: the id says which
// control, the per-item value rides somewhere the payload carries back. A modal
// has no channel and no message, and those stay empty rather than being
// invented — the failure reporter reads an empty channel as "DM this person",
// which for a modal is the only place left to reach them.
func decodeViewSubmission(cb slackgo.InteractionCallback) (Interaction, bool) {
	if cb.View.CallbackID == "" {
		return Interaction{}, false
	}
	return Interaction{
		ActionID: cb.View.CallbackID,
		Intent:   ViewSubmitIntent,
		Item:     cb.View.PrivateMetadata,
		UserID:   cb.User.ID,
		Raw:      cb,
	}, true
}

// ViewInput reads one input block's value out of a submitted modal.
//
// Slack reports a submission's state under (block_id, action_id), which is why
// the modal names both. A missing entry yields empty rather than an error: an
// input block Slack did not send back is a modal this build no longer renders,
// and the handler's own "that is empty" is a better message than a decoding
// one.
func ViewInput(cb slackgo.InteractionCallback, blockID, actionID string) string {
	block, ok := cb.View.State.Values[blockID]
	if !ok {
		return ""
	}
	return block[actionID].Value
}
