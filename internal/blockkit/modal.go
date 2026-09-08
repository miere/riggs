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

// The job editors: one modal per KIND of job, where there used to be one modal
// for all of them (§9d).
//
// The old one had a Command field — "arguments for riggs" — and it was the
// wrong question to ask a person standing in a Slack modal. It made the admin
// the parser: they had to know that the ticket digest is spelled
// `jira tickets --bulk`, that the JQL is one argument however many spaces are
// in it, and that a typo produces a job which fails every three minutes into a
// log. Riggs knows all three of those things. So each kind gets a form that
// asks for the ONE thing Riggs cannot know — a GitHub login, a query — and
// builds the command itself (schedule.Args).
//
// The two forms are deliberately not one with a kind selector. They are
// different shapes, not different values of the same shape: the GitHub digest
// is a singleton with a checkbox that creates or destroys it, and the ticket
// digest is one job per query with a name of its own. A selector would have to
// swap half the fields on change, which a Slack modal cannot do without a round
// trip, and the shared half is two inputs.
//
// Both keep the prompt editor's shape otherwise — an input block per field, the
// identity in `private_metadata`, a callback_id the router matches exactly.

const (
	// GitHubJobModalCallbackID identifies a submission of the GitHub editor.
	GitHubJobModalCallbackID = "github_job"
	// The input blocks. Slack reports a submission's values under (block_id,
	// action_id), so both are named and both are read back.
	GitHubJobModalLoginBlockID    = "github_login"
	GitHubJobModalEnabledBlockID  = "github_enabled"
	GitHubJobModalScheduleBlockID = "github_schedule"
	// GitHubJobModalEnabledValue is the checkbox option's value. A bare token,
	// matched exactly, like every other value in this package.
	GitHubJobModalEnabledValue = "enabled"

	// JiraJobModalCallbackID identifies a submission of the Jira editor.
	JiraJobModalCallbackID = "jira_job"
	// Its input blocks.
	JiraJobModalNameBlockID     = "jira_name"
	JiraJobModalJQLBlockID      = "jira_jql"
	JiraJobModalScheduleBlockID = "jira_schedule"

	// JobModalActionID names the input element inside each block, on both.
	JobModalActionID = "value"
)

// GitHubJobModal configures the pull-request digest.
//
// One job, not a list of them: there is one review queue and it is the admin's.
// So this form has no name field — the job's name is Riggs' to choose
// (schedule.GitHubJobName), or already decided if one exists — and it has a
// checkbox instead, because the question it is really asking is "should Riggs
// be watching your reviews at all".
type GitHubJobModal struct {
	// Name is the existing job's name, empty when there is none yet. It rides
	// in private_metadata: the singleton adopted from an older ledger may be
	// called anything, and the submission has to act on THAT row rather than on
	// the name this build would have chosen.
	Name string
	// Login is the GitHub username whose review queue is fetched.
	Login string
	// Schedule is the cadence as written.
	Schedule string
	// Enabled is whether the job exists at all. Unticking it deletes the job —
	// see the hint on the field, which says so, because the row's own Disable
	// means something quieter and the two are one click apart.
	Enabled bool
}

// View renders the payload `views.open` takes.
//
// The checkbox is OPTIONAL, and it has to be. Slack refuses to submit a
// required input the user has left empty, and an unticked checkbox group IS
// empty — so a required one could be ticked and never unticked, which is the
// half of this control that destroys something.
//
// The other two are required. A digest with no login fetches nobody's reviews
// and one with no schedule runs never, and Slack refusing an empty box is a
// better message than a handler explaining the same thing after the modal has
// closed.
func (m GitHubJobModal) View() any {
	enabled := checkboxOption{
		Text:        plainVerbatim("Run the Pull Requests — Reviewer job"),
		Value:       GitHubJobModalEnabledValue,
		Description: plainPtr("Unticking this DELETES the job and its history. To pause it instead, use Disable on its row."),
	}
	checkbox := checkboxesElem{
		Type:     "checkboxes",
		ActionID: JobModalActionID,
		Options:  []checkboxOption{enabled},
	}
	if m.Enabled {
		// initial_options must be omitted entirely when nothing is ticked. An
		// empty array is not "none selected" to Slack — it is an invalid
		// element, and the modal does not open at all.
		checkbox.InitialOptions = []checkboxOption{enabled}
	}

	blocks := []any{
		inputBlock{
			Type:     "input",
			BlockID:  GitHubJobModalEnabledBlockID,
			Label:    plain("Job"),
			Element:  checkbox,
			Optional: true,
		},
		jobInput(GitHubJobModalLoginBlockID, "GitHub username", m.Login,
			"Whose review queue Riggs fetches, e.g. miere. Not a URL and not an email.", false),
		jobInput(GitHubJobModalScheduleBlockID, "Frequency", m.Schedule,
			"An interval like 3m, or a five-field calendar expression like 0 9 * * 1-5.", false),
	}

	return modalView{
		Type:            "modal",
		CallbackID:      GitHubJobModalCallbackID,
		PrivateMetadata: m.Name,
		Title:           plain(Truncate("GitHub jobs", modalTitleLimit, modalTitleLimit-1)),
		Submit:          plain("Save"),
		Close:           plain("Cancel"),
		Blocks:          blocks,
	}
}

// JiraJobModal creates or edits one ticket digest.
//
// Unlike the GitHub form this one makes instances: a JQL is a question, an
// install has several worth asking, and each one is its own job with its own
// cadence and its own row. Which is why the name is a field here and is not
// there.
type JiraJobModal struct {
	// Name is the job being edited, and empty for a new one. It rides in
	// private_metadata, so a submission knows which job it is about even when
	// the name field is not on the form.
	Name string
	// JQL is the query, pre-filled on an edit.
	JQL string
	// Schedule is the cadence as written.
	Schedule string
}

// New reports whether this modal creates a job rather than editing one.
func (m JiraJobModal) New() bool { return strings.TrimSpace(m.Name) == "" }

// View renders the payload `views.open` takes.
//
// The name is shown only when creating. On an existing job it is not a field,
// because a name is what the ledger keys on and what the row's block_id
// carries, and "rename" is a different operation from "edit" that nobody has
// asked for.
//
// The JQL box is MULTILINE, alone among every job field. A real query runs to
// several clauses and a single-line input shows about forty characters of it,
// which is how somebody ends up editing the wrong half of their own filter. The
// newlines it invites are harmless here: JQL treats them as whitespace, and the
// value is stored as one argument either way.
func (m JiraJobModal) View() any {
	var blocks []any
	if m.New() {
		blocks = append(blocks, jobInput(JiraJobModalNameBlockID, "Name", "",
			"Letters, digits, dot, dash and underscore. It identifies the job everywhere.", false))
	}
	blocks = append(blocks,
		jqlInput(JiraJobModalJQLBlockID, "JQL", m.JQL,
			"The query deciding which tickets are advertised. Paste it exactly as Jira accepts it."),
		jobInput(JiraJobModalScheduleBlockID, "Frequency", m.Schedule,
			"An interval like 3m, or a five-field calendar expression like 0 9 * * 1-5.", false),
	)

	title := "New Jira job"
	if !m.New() {
		title = m.Name
	}
	return modalView{
		Type:            "modal",
		CallbackID:      JiraJobModalCallbackID,
		PrivateMetadata: m.Name,
		Title:           plain(Truncate(title, modalTitleLimit, modalTitleLimit-1)),
		Submit:          plain("Save"),
		Close:           plain("Cancel"),
		Blocks:          blocks,
	}
}

// checkboxOption is one tick box. It is a third option wire type beside
// menuOptionObj and staticSelectOption, for the reason those are separate from
// each other: this one carries a `description`, which neither of the others
// accepts, and a shared type would re-render their bytes.
type checkboxOption struct {
	Text        textObj  `json:"text"`
	Value       string   `json:"value"`
	Description *textObj `json:"description,omitempty"`
}

type checkboxesElem struct {
	Type     string           `json:"type"`
	ActionID string           `json:"action_id"`
	Options  []checkboxOption `json:"options"`
	// InitialOptions is omitted when nothing is ticked. An empty array is not a
	// valid element and the view is rejected wholesale.
	InitialOptions []checkboxOption `json:"initial_options,omitempty"`
}

// jobInput builds one single-line input block.
//
// Single-line, unlike the prompt editor's: every one of these is a name, a
// login or a cadence, and a multiline box invites a newline that the value
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

// jqlInput builds the one multiline job field. See JiraJobModal.View.
func jqlInput(blockID, label, value, hint string) inputBlock {
	block := jobInput(blockID, label, value, hint, false)
	block.Element = plainTextInput{
		Type:         "plain_text_input",
		ActionID:     JobModalActionID,
		Multiline:    true,
		InitialValue: value,
	}
	return block
}

// plainPtr is plain(), addressable — for the optional text objects on a wire
// type that distinguishes "absent" from "empty".
func plainPtr(s string) *textObj {
	t := plain(s)
	return &t
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

// The Configuration editor: the sixth modal, and the second that edits several
// settings at once.
//
// It carries the per-kind job timeouts, and it is a separate modal from
// Customisation rather than two more fields on it. The two are pointed at
// different things by different people at different times: Customisation is how
// Riggs LOOKS — the emoji on a message, the portrait on the tab — and is opened
// out of taste; Configuration is how Riggs BEHAVES when nobody is watching, and
// is opened because a digest has started timing out. That is the same call §10
// made about `review-request` and `sme-assistance`, and the reason is the same:
// a shared surface means the next setting has to pick a side, and the wrong side
// is only discovered when changing one silently moves the other.
//
// Like Customisation it has no `private_metadata` — there is no per-item
// identity, because the item IS the whole form — and every field is read back by
// (block_id, action_id).

const (
	// ConfigurationModalCallbackID identifies a submission of this modal.
	ConfigurationModalCallbackID = "configuration"
	// ConfigurationActionID names the input element inside every block. One id
	// across all of them, because the block_id is what distinguishes the fields.
	ConfigurationActionID = "value"
	// ConfigurationTimeoutBlockPrefix namespaces one timeout's input block. The
	// kind's own token follows it, so a field is addressed the same way a
	// prompt row is (§7b).
	ConfigurationTimeoutBlockPrefix = "timeout:"
)

// ConfigurationTimeout is one editable timeout on the modal.
type ConfigurationTimeout struct {
	// ID is the kind's token. It follows ConfigurationTimeoutBlockPrefix in the
	// block_id and is how a submission is mapped back to a setting.
	ID string
	// Label names it: "Jira tickets timeout".
	Label string
	// Hint says what the bound applies to.
	Hint string
	// Value is the bound in force, pre-filled so an edit starts from what is
	// actually running rather than from an empty box.
	Value string
}

// ConfigurationModal is the editor for how Riggs' jobs behave.
type ConfigurationModal struct {
	// Timeouts are the per-kind bounds, in the order the Home tab lists the
	// kinds.
	Timeouts []ConfigurationTimeout
}

// View renders the payload `views.open` takes.
//
// Every field is OPTIONAL, like Customisation's and for the same reason: an
// empty box here has exactly one sensible reading — "use the built-in bound" —
// and there is nowhere else on a modal that edits several settings at once to
// express a reset without it becoming a different, worse surface.
func (m ConfigurationModal) View() any {
	blocks := make([]any, 0, len(m.Timeouts))
	for _, t := range m.Timeouts {
		block := inputBlock{
			Type:    "input",
			BlockID: ConfigurationTimeoutBlockPrefix + t.ID,
			Label:   plain(t.label()),
			Element: plainTextInput{
				Type:         "plain_text_input",
				ActionID:     ConfigurationActionID,
				InitialValue: t.Value,
			},
			Optional: true,
		}
		if hint := strings.TrimSpace(t.Hint); hint != "" {
			h := plain(hint + " Empty uses the default.")
			block.Hint = &h
		}
		blocks = append(blocks, block)
	}
	return modalView{
		Type:       "modal",
		CallbackID: ConfigurationModalCallbackID,
		Title:      plain(Truncate("Configuration", modalTitleLimit, modalTitleLimit-1)),
		Submit:     plain("Save"),
		Close:      plain("Cancel"),
		Blocks:     blocks,
	}
}

// label is the field's name, with a fallback so a modal built from a kind this
// build does not know about still renders rather than being rejected for an
// empty text object.
func (t ConfigurationTimeout) label() string {
	if l := strings.TrimSpace(t.Label); l != "" {
		return l
	}
	return "Timeout"
}
