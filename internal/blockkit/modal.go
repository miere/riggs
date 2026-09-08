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
	// Optional inverts Slack's default, which is that an input must be filled
	// in. Only the job timeout uses it: everything else on either modal is a
	// value the handler cannot invent.
	Optional bool `json:"optional,omitempty"`
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

// The job editor: the second modal, and the one that creates something.
//
// It shares the prompt editor's shape — an input block per field, the identity
// in `private_metadata`, a callback_id the router matches exactly — and differs
// in the one way that matters: a NEW job has no identity yet, so the name is a
// field. On an existing job it is not, because a name is what the ledger keys
// on and what the row's block_id carries, and "rename" is a different operation
// from "edit" that nobody has asked for.

const (
	// JobModalCallbackID identifies a submission of the job editor.
	JobModalCallbackID = "job_edit"
	// The input blocks. Slack reports a submission's values under (block_id,
	// action_id), so both are named and both are read back.
	JobModalNameBlockID     = "job_name"
	JobModalCommandBlockID  = "job_command"
	JobModalScheduleBlockID = "job_schedule"
	JobModalTimeoutBlockID  = "job_timeout"
	// JobModalActionID names the input element inside each block.
	JobModalActionID = "value"
)

// JobModal is the editor for one scheduled job.
type JobModal struct {
	// Name is the job being edited, and empty for a new one. It rides in
	// private_metadata, so a submission knows which job it is about even though
	// the name field may not be on the form.
	Name string
	// Command is the argument list as a line: "git pr --bulk miere".
	Command string
	// Schedule is the cadence as written.
	Schedule string
	// Timeout is the bound as written: "2m".
	Timeout string
}

// New reports whether this modal creates a job rather than editing one.
func (m JobModal) New() bool { return strings.TrimSpace(m.Name) == "" }

// View renders the payload `views.open` takes.
//
// Only the timeout is optional. A job with no command runs nothing and a job
// with no schedule runs never, and Slack refusing an empty box is a better
// message than a handler explaining the same thing after the modal has closed.
func (m JobModal) View() any {
	var blocks []any
	if m.New() {
		blocks = append(blocks, jobInput(JobModalNameBlockID, "Name", "",
			"Letters, digits, dot, dash and underscore. It identifies the job everywhere.", false))
	}
	blocks = append(blocks,
		jobInput(JobModalCommandBlockID, "Command", m.Command,
			"Arguments for riggs, e.g. `git pr --bulk miere`. Split on spaces; no quoting.", false),
		jobInput(JobModalScheduleBlockID, "Schedule", m.Schedule,
			"An interval like 3m, or a five-field calendar expression like 0 9 * * 1-5.", false),
		jobInput(JobModalTimeoutBlockID, "Timeout", m.Timeout,
			"How long one run may take, e.g. 2m. Empty uses the default.", true),
	)

	title := "New job"
	if !m.New() {
		title = m.Name
	}
	return modalView{
		Type:            "modal",
		CallbackID:      JobModalCallbackID,
		PrivateMetadata: m.Name,
		Title:           plain(Truncate(title, modalTitleLimit, modalTitleLimit-1)),
		Submit:          plain("Save"),
		Close:           plain("Cancel"),
		Blocks:          blocks,
	}
}

// jobInput builds one single-line input block.
//
// Single-line, unlike the prompt editor's: every one of these is a name, a
// command or a duration, and a multiline box invites a newline that the value
// cannot carry.
func jobInput(blockID, label, value, hint string, optional bool) inputBlock {
	block := inputBlock{
		Type:    "input",
		BlockID: blockID,
		Label:   plain(label),
		Element: plainTextInput{
			Type:         "plain_text_input",
			ActionID:     JobModalActionID,
			InitialValue: value,
		},
		Optional: optional,
	}
	if hint != "" {
		h := plain(hint)
		block.Hint = &h
	}
	return block
}

// The delete confirmation: the third modal, and the only one that asks a
// question rather than editing a value.
//
// It exists because Slack has no per-option confirmation on an overflow menu.
// `confirm` is a field of the interactive ELEMENT, not of an option inside it;
// an option carrying one makes the block invalid, which makes the whole Home
// view invalid, which takes the tab down with it (§7e). Moving the confirm up
// to the overflow would have published, and would then have asked "delete this
// job?" when somebody picked Edit.
//
// So the question moves to where a question can be asked about exactly one
// option. The guard is no weaker for it: Delete still cannot happen in one
// click, and the modal can say more than a confirm dialog's 300 characters.

const (
	// JobDeleteModalCallbackID identifies a confirmed delete coming back.
	//
	// A separate callback_id from the editor's, not a mode flag on it: the
	// router matches these exactly, and a submission that deletes something
	// should not be one field's difference from a submission that saves it.
	JobDeleteModalCallbackID = "job_delete"
)

// JobDeleteModal asks before a job and its history are forgotten.
type JobDeleteModal struct {
	// Name is the job about to go. It is both what the modal says and, through
	// private_metadata, what the submission acts on — read back from the modal
	// rather than the row, because the Home tab underneath may have been
	// republished since it was drawn.
	Name string
}

// View renders the payload `views.open` takes.
//
// No input block: there is nothing to type, and the submit button is the
// answer. Slack still requires at least one block, so the question is a
// section — which is also why this modal can spell out what "forgotten" means
// and point at Disable, where the old confirm dialog had one line.
func (m JobDeleteModal) View() any {
	return modalView{
		Type:            "modal",
		CallbackID:      JobDeleteModalCallbackID,
		PrivateMetadata: m.Name,
		Title:           plain(Truncate("Delete job", modalTitleLimit, modalTitleLimit-1)),
		Submit:          plain("Delete"),
		Close:           plain("Keep it"),
		Blocks: []any{sectionBlock{
			Type: "section",
			Text: mrkdwn("*" + escapeMrkdwn(m.name()) + "* will stop running, and its run history will be forgotten.\n\n_Disable keeps both._"),
		}},
	}
}

// name falls back so a modal opened for a job whose row was stale still
// renders: an empty text object is rejected, and a rejected view is a click
// that does nothing and says nothing.
func (m JobDeleteModal) name() string {
	if n := strings.TrimSpace(m.Name); n != "" {
		return n
	}
	return "This job"
}

// The Customisation editor: the fourth modal, and the only one that edits
// several unrelated settings at once.
//
// The other three are about ONE thing — a prompt, a job, a deletion — because
// each of those has a row of its own on the Home tab to be reached from. These
// do not. Four emoji names and a banner switch would be five rows of a surface
// that is already long, every one of them a setting somebody touches once and
// then never again, pushing the jobs and the prompts they touch weekly further
// down the page. So they live behind one menu option instead, and the modal
// carries the lot.
//
// That means it has no `private_metadata`: there is no per-item identity,
// because the item IS the whole form. Every field is read back by (block_id,
// action_id) like the job editor's, and the ids are exported for exactly that.

const (
	// CustomisationModalCallbackID identifies a submission of this modal.
	CustomisationModalCallbackID = "customisation"
	// CustomisationActionID names the input element inside every block. One id
	// across all of them, because the block_id is what distinguishes the
	// fields and a second varying id would only be a second thing to keep in
	// step.
	CustomisationActionID = "value"
	// CustomisationEmojiBlockPrefix namespaces one emoji's input block. The
	// state's own token follows it, so a field is addressed the same way a
	// prompt row is (§7b).
	CustomisationEmojiBlockPrefix = "emoji:"
	// CustomisationBannerBlockID names the banner switch.
	CustomisationBannerBlockID = "banner"
	// The banner switch's two options. Bare tokens, matched exactly, like every
	// other value in this package.
	CustomisationBannerShow = "show"
	CustomisationBannerHide = "hide"
)

// CustomisationEmoji is one editable emoji on the modal.
type CustomisationEmoji struct {
	// ID is the state's token. It follows CustomisationEmojiBlockPrefix in the
	// block_id and is how a submission is mapped back to a setting.
	ID string
	// Label names the state: "Acknowledgement".
	Label string
	// Hint says when this emoji appears. It is the only place the state machine
	// is explained to the person changing it, which is why every field has one
	// even though three of them are nearly the same sentence.
	Hint string
	// Value is the shortcode in force, pre-filled so an edit starts from what
	// is actually running rather than from an empty box.
	Value string
}

// CustomisationModal is the editor for how Riggs presents itself.
type CustomisationModal struct {
	// Emojis are the reaction states, in lifecycle order.
	Emojis []CustomisationEmoji
	// ShowBanner is the banner switch's current position.
	ShowBanner bool
}

// staticSelectOption is one choice on a select. It is a separate wire type from
// the overflow's menuOptionObj even though the JSON is identical today: an
// overflow option can carry a `url` (§7c) and this can carry an initial-option
// pointer, and a shared type would mean a change to either re-rendering the
// other's bytes.
type staticSelectOption struct {
	Text  textObj `json:"text"`
	Value string  `json:"value"`
}

type staticSelectElem struct {
	Type          string               `json:"type"`
	ActionID      string               `json:"action_id"`
	Options       []staticSelectOption `json:"options"`
	InitialOption *staticSelectOption  `json:"initial_option,omitempty"`
}

// View renders the payload `views.open` takes.
//
// Every emoji input is OPTIONAL, which is the opposite of the prompt editor and
// deliberate. There, an empty box could only mean "reset" or "say nothing", and
// a Reset option already existed — so Slack was left to refuse it. Here an
// empty box has exactly one sensible reading, "use the built-in emoji", and
// there is nowhere else to express it: a modal that edits four settings at once
// cannot also carry four Reset options without becoming a different, worse
// surface.
//
// The banner select is not optional and has no empty state: it is a switch, and
// a switch is always in one of its positions.
func (m CustomisationModal) View() any {
	blocks := make([]any, 0, len(m.Emojis)+1)
	for _, e := range m.Emojis {
		blocks = append(blocks, inputBlock{
			Type:    "input",
			BlockID: CustomisationEmojiBlockPrefix + e.ID,
			Label:   plain(e.label()),
			Element: plainTextInput{
				Type:         "plain_text_input",
				ActionID:     CustomisationActionID,
				InitialValue: e.Value,
			},
			Hint:     e.hint(),
			Optional: true,
		})
	}

	show := staticSelectOption{Text: plainVerbatim("Show"), Value: CustomisationBannerShow}
	hide := staticSelectOption{Text: plainVerbatim("Hide"), Value: CustomisationBannerHide}
	initial := hide
	if m.ShowBanner {
		initial = show
	}
	bannerHint := plain("The portrait at the top of this tab. Hiding it leaves the version line and everything under it.")
	blocks = append(blocks, inputBlock{
		Type:    "input",
		BlockID: CustomisationBannerBlockID,
		Label:   plain("Banner"),
		Element: staticSelectElem{
			Type:          "static_select",
			ActionID:      CustomisationActionID,
			Options:       []staticSelectOption{show, hide},
			InitialOption: &initial,
		},
		Hint: &bannerHint,
	})

	return modalView{
		Type:       "modal",
		CallbackID: CustomisationModalCallbackID,
		Title:      plain(Truncate("Customisation", modalTitleLimit, modalTitleLimit-1)),
		Submit:     plain("Save"),
		Close:      plain("Cancel"),
		Blocks:     blocks,
	}
}

// label is the field's name, with a fallback so a modal built from a state this
// build does not know about still renders rather than being rejected for an
// empty text object.
func (e CustomisationEmoji) label() string {
	if l := strings.TrimSpace(e.Label); l != "" {
		return l
	}
	return "Emoji"
}

// hint is the field's explanation, absent when there is none.
func (e CustomisationEmoji) hint() *textObj {
	h := strings.TrimSpace(e.Hint)
	if h == "" {
		return nil
	}
	// The shortcode is spelled out because the box takes a NAME and people type
	// the emoji. Saying so at the field is cheaper than rejecting it after.
	t := plain(h + " Type the shortcode, not the emoji.")
	return &t
}
