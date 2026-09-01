package blockkit

import "strings"

// The prompt editor: the one modal Riggs opens.
//
// It exists because the App Home tab edits the four prompts (§7e), and a prompt
// is a paragraph. Slack's only surface for typing a paragraph is a modal with a
// multiline input; there is no "edit in place" on a published Home view.
//
// The prompt's id rides in `private_metadata` rather than in the callback_id.
// The callback_id is what the daemon's routing table matches on, and a table
// cannot match a value that varies per prompt — the same constraint that keeps
// every option value in this package a bare token (§7b).

const (
	// PromptModalCallbackID identifies a submission of this modal.
	//
	// It reaches the router as an action_id, because a view submission is
	// dispatched by the same table as a click: it is the same kind of thing —
	// a control Riggs rendered, operated by a human, delivered to the app that
	// drew it.
	PromptModalCallbackID = "prompt_edit"
	// PromptModalBlockID names the input block.
	PromptModalBlockID = "prompt"
	// PromptModalActionID names the input element inside it. Slack reports a
	// submission's values under (block_id, action_id), so both are needed to
	// read the text back out.
	PromptModalActionID = "text"

	// modalTitleLimit is Slack's cap on a modal title, in characters. A title
	// past it is rejected wholesale — the modal simply does not open — so it is
	// cut rather than gambled on.
	modalTitleLimit = 24
)

// PromptModal is the editor for one prompt.
type PromptModal struct {
	// ID is the prompt's token, carried in private_metadata and returned on
	// submission.
	ID string
	// Label names the prompt, used as the modal's title and the input's label.
	Label string
	// Hint explains the placeholders, shown under the input.
	Hint string
	// Value is the wording in force, pre-filled so an edit starts from what is
	// actually running rather than from an empty box.
	Value string
}

// --- wire types -----------------------------------------------------------
// Ordered structs, like every other payload in this package.

type plainTextInput struct {
	Type         string `json:"type"`
	ActionID     string `json:"action_id"`
	Multiline    bool   `json:"multiline"`
	InitialValue string `json:"initial_value,omitempty"`
}

type inputBlock struct {
	Type    string   `json:"type"`
	BlockID string   `json:"block_id"`
	Label   textObj  `json:"label"`
	Element any      `json:"element"`
	Hint    *textObj `json:"hint,omitempty"`
}

type modalView struct {
	Type            string  `json:"type"`
	CallbackID      string  `json:"callback_id"`
	PrivateMetadata string  `json:"private_metadata,omitempty"`
	Title           textObj `json:"title"`
	Submit          textObj `json:"submit"`
	Close           textObj `json:"close"`
	Blocks          []any   `json:"blocks"`
}

// View renders the payload `views.open` takes.
//
// The input is REQUIRED, which is Slack's default for an input block and is
// left that way deliberately. An empty submission would have to mean either
// "reset to the default" or "a prompt that says nothing", and there is already
// an explicit Reset on the row's own menu — so Slack refuses the empty box
// before it ever reaches the handler.
func (m PromptModal) View() any {
	input := plainTextInput{
		Type:         "plain_text_input",
		ActionID:     PromptModalActionID,
		Multiline:    true,
		InitialValue: m.Value,
	}
	block := inputBlock{
		Type:    "input",
		BlockID: PromptModalBlockID,
		Label:   plain(m.label()),
		Element: input,
	}
	if hint := strings.TrimSpace(m.Hint); hint != "" {
		h := plain(hint)
		block.Hint = &h
	}
	return modalView{
		Type:            "modal",
		CallbackID:      PromptModalCallbackID,
		PrivateMetadata: m.ID,
		Title:           plain(Truncate(m.label(), modalTitleLimit, modalTitleLimit-1)),
		Submit:          plain("Save"),
		Close:           plain("Cancel"),
		Blocks:          []any{block},
	}
}

// label is the prompt's name, with a fallback so a modal opened for a prompt
// this build does not know about still renders rather than being rejected for
// an empty text object.
func (m PromptModal) label() string {
	if l := strings.TrimSpace(m.Label); l != "" {
		return l
	}
	return "Prompt"
}
