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
		BlockID:  "acme/monolith#20534",
		Value:    "approve_only",
	}))
	if !ok {
		t.Fatal("DecodeInteraction reported not-ok for a block action")
	}
	if in.ActionID != "approve_only" || in.Intent != "approve_only" {
		t.Fatalf("action/intent = %q/%q", in.ActionID, in.Intent)
	}
	if in.Item != "acme/monolith#20534" {
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
		BlockID:  "acme/monolith#20534",
	}
	a.SelectedOption.Value = "approve_merge"

	in, ok := DecodeInteraction(blockActions(a))
	if !ok {
		t.Fatal("DecodeInteraction reported not-ok")
	}
	if in.Intent != "approve_merge" {
		t.Fatalf("Intent = %q, want the selected option's value", in.Intent)
	}
	if in.Item != "acme/monolith#20534" {
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

// A view submission is dispatched by the same table as a click, so it has to
// arrive in the same vocabulary: the callback_id is the control, the
// private_metadata is the item.
func TestDecodeViewSubmission(t *testing.T) {
	cb := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		User: slackgo.User{ID: "U-admin"},
	}
	cb.View.CallbackID = "prompt_edit"
	cb.View.PrivateMetadata = "ai_review"

	in, ok := DecodeInteraction(cb)
	if !ok {
		t.Fatal("a view submission was dropped")
	}
	if in.ActionID != "prompt_edit" || in.Intent != ViewSubmitIntent {
		t.Fatalf("route = %s/%s", in.ActionID, in.Intent)
	}
	if in.Item != "ai_review" {
		t.Fatalf("Item = %q, want the private_metadata", in.Item)
	}
	if in.UserID != "U-admin" {
		t.Fatalf("UserID = %q", in.UserID)
	}
	// No channel and no message: those stay empty rather than being invented.
	// The failure reporter reads an empty channel as "DM this person", which
	// for a modal is the only place left to reach them.
	if in.Channel != "" || in.MessageTS != "" {
		t.Fatalf("a modal was given a conversation: %+v", in)
	}
}

// A submission with no callback_id names no control, so there is nothing to
// route it to.
func TestAViewSubmissionWithoutACallbackIDIsDropped(t *testing.T) {
	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeViewSubmission}
	if _, ok := DecodeInteraction(cb); ok {
		t.Fatal("a submission with no callback_id was routed")
	}
}

// A trigger id lives about three seconds, and a handler that means to open a
// modal needs it.
func TestABlockActionCarriesItsTriggerID(t *testing.T) {
	cb := slackgo.InteractionCallback{
		Type:      slackgo.InteractionTypeBlockActions,
		TriggerID: "trigger-123",
		ActionCallback: slackgo.ActionCallbacks{
			BlockActions: []*slackgo.BlockAction{{ActionID: "app_prompt", BlockID: "prompt:ai_review"}},
		},
	}
	cb.ActionCallback.BlockActions[0].SelectedOption.Value = "edit"

	in, ok := DecodeInteraction(cb)
	if !ok {
		t.Fatal("the click was dropped")
	}
	if in.TriggerID != "trigger-123" {
		t.Fatalf("TriggerID = %q", in.TriggerID)
	}
	if in.Intent != "edit" || in.Item != "prompt:ai_review" {
		t.Fatalf("route = %s on %s", in.Intent, in.Item)
	}
}

// Slack reports a submission's state under (block_id, action_id), which is why
// the modal names both.
func TestViewInputReadsTheSubmittedText(t *testing.T) {
	cb := slackgo.InteractionCallback{}
	cb.View.State = &slackgo.ViewState{
		Values: map[string]map[string]slackgo.BlockAction{
			"prompt": {"text": {Value: "the new wording"}},
		},
	}
	if got := ViewInput(cb, "prompt", "text"); got != "the new wording" {
		t.Fatalf("ViewInput = %q", got)
	}
	// A block Slack did not send back is a modal this build no longer renders.
	// The handler's own "that is empty" beats a decoding error.
	if got := ViewInput(cb, "gone", "text"); got != "" {
		t.Fatalf("ViewInput = %q, want empty", got)
	}
}

// A select and a text input are read from DIFFERENT fields of the same state
// entry, and a select read as an input comes back empty — which looks exactly
// like a field the user left blank.
//
// That is a bug that cannot fail loudly, so the two reads are kept apart rather
// than merged behind a fallback, and this is what says so.
func TestViewSelectAndViewInputReadDifferentFields(t *testing.T) {
	cb := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{
			CallbackID: "customisation",
			State: &slackgo.ViewState{
				Values: map[string]map[string]slackgo.BlockAction{
					"emoji:success": {"value": {Value: "tada"}},
					"banner":        {"value": {SelectedOption: slackgo.OptionBlockObject{Value: "hide"}}},
				},
			},
		},
	}

	if got := ViewInput(cb, "emoji:success", "value"); got != "tada" {
		t.Errorf("ViewInput on a text input = %q, want the typed value", got)
	}
	if got := ViewSelect(cb, "banner", "value"); got != "hide" {
		t.Errorf("ViewSelect on a select = %q, want the chosen option", got)
	}
	// The trap, stated: reading the select as an input is silently empty.
	if got := ViewInput(cb, "banner", "value"); got != "" {
		t.Errorf("ViewInput on a select = %q; if this ever stops being empty the "+
			"two readers could be merged", got)
	}
	// A block the submission never carried is empty rather than a panic: it is a
	// modal this build no longer renders, and the handler's own "that is empty"
	// is a better message than a decoding one.
	if got := ViewSelect(cb, "gone", "value"); got != "" {
		t.Errorf("ViewSelect on a missing block = %q", got)
	}
}

// A checkbox is a THIRD shape again: `selected_options`, a list, and an
// unticked group reports an empty one.
//
// That last part is the whole reason it cannot be folded into ViewSelect. For a
// select, empty means "not answered"; for a checkbox it means "answered, no" —
// which on the GitHub job form is the admin asking for the job to be deleted.
// Reading it through ViewInput would come back empty on EVERY submission, which
// is that same instruction, every time.
func TestViewCheckedReadsACheckbox(t *testing.T) {
	ticked := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{
			CallbackID: "github_job",
			State: &slackgo.ViewState{
				Values: map[string]map[string]slackgo.BlockAction{
					"github_enabled": {"value": {SelectedOptions: []slackgo.OptionBlockObject{
						{Value: "enabled"},
					}}},
					"github_login": {"value": {Value: "miere"}},
				},
			},
		},
	}
	if !ViewChecked(ticked, "github_enabled", "value", "enabled") {
		t.Error("a ticked checkbox read as unticked")
	}
	// The option's own value is matched rather than "is anything selected", so a
	// group that grows a second option later does not read as the first one.
	if ViewChecked(ticked, "github_enabled", "value", "something_else") {
		t.Error("a different option's value read as ticked")
	}
	// The trap, stated: through ViewInput it is indistinguishable from unticked.
	if got := ViewInput(ticked, "github_enabled", "value"); got != "" {
		t.Errorf("ViewInput on a checkbox = %q; if this ever stops being empty the "+
			"readers could be merged", got)
	}

	unticked := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{
			CallbackID: "github_job",
			State: &slackgo.ViewState{
				Values: map[string]map[string]slackgo.BlockAction{
					"github_enabled": {"value": {SelectedOptions: nil}},
				},
			},
		},
	}
	if ViewChecked(unticked, "github_enabled", "value", "enabled") {
		t.Error("an unticked checkbox read as ticked")
	}
	// And a block the submission never carried is false rather than a panic.
	if ViewChecked(unticked, "gone", "value", "enabled") {
		t.Error("a missing block read as ticked")
	}
}
