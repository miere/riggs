package slack

import (
	"testing"

	slackgo "github.com/slack-go/slack"
)

// blockActions builds a block_actions callback carrying one action.
func blockActions(a *slackgo.BlockAction) slackgo.InteractionCallback {
	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	cb.ActionCallback.BlockActions = []*slackgo.BlockAction{a}
	cb.Channel.ID = "C123"
	cb.User.ID = "U999"
	cb.Message.Timestamp = "1700.0001"
	return cb
}

func TestDecodeInteractionReadsAButton(t *testing.T) {
	in, ok := DecodeInteraction(blockActions(&slackgo.BlockAction{
		ActionID: "approve_only",
		BlockID:  "UpsideRealty/upside#20534",
		Value:    "approve_only",
	}))
	if !ok {
		t.Fatal("DecodeInteraction reported not-ok for a block action")
	}
	if in.ActionID != "approve_only" || in.Intent != "approve_only" {
		t.Fatalf("action/intent = %q/%q", in.ActionID, in.Intent)
	}
	if in.Item != "UpsideRealty/upside#20534" {
		t.Fatalf("Item = %q, want the block_id", in.Item)
	}
	if in.Channel != "C123" || in.UserID != "U999" || in.MessageTS != "1700.0001" {
		t.Fatalf("coordinates = %q/%q/%q", in.Channel, in.UserID, in.MessageTS)
	}
}

// The overflow is the shape every bulk row uses, and its intent lives on the
// chosen option rather than on the element — the element's own Value is empty.
func TestDecodeInteractionPrefersTheSelectedOption(t *testing.T) {
	a := &slackgo.BlockAction{
		ActionID: "pr_overflow",
		BlockID:  "UpsideRealty/upside#20534",
	}
	a.SelectedOption.Value = "approve_merge"

	in, ok := DecodeInteraction(blockActions(a))
	if !ok {
		t.Fatal("DecodeInteraction reported not-ok")
	}
	if in.Intent != "approve_merge" {
		t.Fatalf("Intent = %q, want the selected option's value", in.Intent)
	}
	if in.Item != "UpsideRealty/upside#20534" {
		t.Fatalf("Item = %q, want the block_id", in.Item)
	}
}

// A message ts is what the ledger keys on, so an absent cb.Message must not
// leave it empty when the container carries it.
func TestDecodeInteractionFallsBackToTheContainerTS(t *testing.T) {
	cb := blockActions(&slackgo.BlockAction{ActionID: "pr_overflow", Value: "x"})
	cb.Message.Timestamp = ""
	cb.Container.MessageTs = "1800.0002"

	in, _ := DecodeInteraction(cb)
	if in.MessageTS != "1800.0002" {
		t.Fatalf("MessageTS = %q, want the container ts", in.MessageTS)
	}
}

func TestDecodeInteractionRejectsWhatIsNotOurs(t *testing.T) {
	cases := map[string]slackgo.InteractionCallback{
		"a view submission": {Type: slackgo.InteractionTypeViewSubmission},
		"no block actions":  {Type: slackgo.InteractionTypeBlockActions},
	}
	for name, cb := range cases {
		if _, ok := DecodeInteraction(cb); ok {
			t.Errorf("%s: DecodeInteraction reported ok, want not-ok", name)
		}
	}
}
