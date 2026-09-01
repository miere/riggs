package blockkit

import (
	"fmt"
	"strings"
)

// The App Home surface: Riggs' portrait, the version it is running, and — for
// the admin, and only when there is something to install — the latest release's
// notes with an Update button beside them.
//
// The split is the whole design. Anyone in the workspace may open the app and
// see what Riggs is and what version answers their clicks; everything from the
// divider onwards is machinery, and machinery is the admin's business. A
// non-admin is not shown a disabled button, because a control you cannot use is
// worse than one that was never there.

const (
	// HomePortraitURL is the wide portrait at the top of the Home tab.
	//
	// Served from the repository over `?raw=true` rather than committed to a
	// Slack-hosted asset: the file is already in the repo, it is public, and
	// pinning the Home tab to an image someone has to remember to re-upload is
	// how a broken thumbnail happens.
	HomePortraitURL = "https://github.com/miere/riggs/blob/main/riggs-wide.jpeg?raw=true"
	// HomePortraitAlt is its alt text.
	HomePortraitAlt = "Riggs Photo"

	// HomeMenuActionID is the action_id of the overflow menu beside the
	// version line — Riggs' own controls, as opposed to the Update button,
	// which belongs to a release rather than to the app.
	HomeMenuActionID = "app_menu"
	// HomeRestartIntent is the menu's restart option.
	HomeRestartIntent = "restart"

	// HomePromptActionID is the action_id of the overflow beside each editable
	// prompt. One id for all of them, like a digest's rows: the router matches
	// (action_id, intent) and recovers WHICH prompt from the block_id, because
	// an overflow click reports its own block_id and not its siblings' values
	// (§7b).
	HomePromptActionID = "app_prompt"
	// HomePromptEditIntent opens the editor.
	HomePromptEditIntent = "edit"
	// HomePromptResetIntent drops the override and goes back to the built-in
	// wording. Rendered only on a prompt that HAS an override — there is
	// nothing to reset otherwise, and an option that does nothing is the
	// mistake this file keeps not making.
	HomePromptResetIntent = "reset"
	// HomePromptBlockPrefix namespaces a prompt row's block_id, so a click on
	// one is distinguishable from any other block that might ever carry an id
	// on this surface.
	HomePromptBlockPrefix = "prompt:"

	// HomeUpdateActionID is the action_id of the Update button.
	HomeUpdateActionID = "home_update"
	// HomeUpdateIntent is its value: a bare token, like every other control
	// Riggs renders, so the daemon's routing table can match it exactly.
	//
	// The release tag deliberately does NOT ride in it. The tag would make the
	// value vary, which the router cannot match on — and it would also let a
	// stale Home tab, published days ago and never refreshed, install a version
	// that is no longer the latest. The handler re-resolves what to install at
	// click time instead.
	HomeUpdateIntent = "update"
	// HomeReleaseNotesBlockID names the section carrying the notes.
	HomeReleaseNotesBlockID = "release_notes"

	// homeNotesLimit is Slack's cap on a section's text, in runes. Release
	// notes routinely run past it; a payload that exceeds it is rejected
	// wholesale, so the notes are cut rather than the tab going blank.
	homeNotesLimit = 2900

	// homePromptLimit is how much of a prompt the tab shows, in runes.
	//
	// Enough to recognise which prompt this is and see roughly what it says;
	// not so much that four of them push the update section off the screen. The
	// whole text is in the editor, one click away, which is where it is read
	// properly anyway.
	homePromptLimit = 220
	homePromptKeep  = 217
)

// Home is the App Home view.
type Home struct {
	// Version is the running build, rendered verbatim under the portrait.
	Version string
	// Admin puts the controls menu on the version line. It is a separate flag
	// from Update because the two answer different questions: Update is "is
	// there a release to install", Admin is "may this viewer operate Riggs at
	// all". Restarting is available whether or not anything is out of date.
	Admin bool
	// Prompts are the editable wordings, each on its own row with an overflow.
	// Empty for a non-admin, and for a build with nothing to edit.
	//
	// They sit ABOVE the update divider rather than below it, because they are
	// about how Riggs behaves rather than about which release it is — the same
	// distinction that put the controls menu on the version line.
	Prompts []HomePrompt
	// Update, when set, appends the divider and everything after it. Nil is
	// the ordinary state: up to date, or a viewer with no business seeing it.
	Update *HomeUpdate
}

// HomePrompt is one editable wording on the Home tab.
type HomePrompt struct {
	// ID is the prompt's token. It rides in the row's block_id and comes back
	// on the click.
	ID string
	// Label names it: "AI code review".
	Label string
	// Text is the wording in force — configured, or the built-in default.
	Text string
	// Overridden reports that Text came from the config rather than the
	// default, which is what decides whether Reset is offered and what the row
	// says about itself.
	Overridden bool
}

// HomeUpdate is the available release.
type HomeUpdate struct {
	// Tag is the release, shown in the header.
	Tag string
	// Notes is the release body, ALREADY converted to Slack mrkdwn. This type
	// renders blocks; internal/slackmd translates dialects. Keeping the
	// conversion outside means the view can be asserted on without dragging a
	// Markdown converter into its tests.
	Notes string
}

// --- wire types -----------------------------------------------------------
// Ordered structs, so the encoded bytes are stable — see card.go.

type imageBlock struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	AltText  string `json:"alt_text"`
}

type headerBlock struct {
	Type  string  `json:"type"`
	Text  textObj `json:"text"`
	Level int     `json:"level,omitempty"`
}

// accessorySection is a section with an element on its right. It is separate
// from card.go's sectionBlock rather than an optional field on it: the digest's
// fingerprint is computed over encoded bytes, and a type it shares has no
// business growing fields for a surface it does not render.
type accessorySection struct {
	Type      string  `json:"type"`
	BlockID   string  `json:"block_id,omitempty"`
	Text      textObj `json:"text"`
	Accessory any     `json:"accessory,omitempty"`
}

// homeView is the published view envelope.
type homeView struct {
	Type   string `json:"type"`
	Blocks []any  `json:"blocks"`
}

// Blocks renders the Home tab.
func (h Home) Blocks() []any {
	version := accessorySection{Type: "section", Text: mrkdwn(h.versionLine())}
	// The menu rides on the version line rather than sitting below the
	// divider with the update, because it is not about a release: there is
	// something to restart whether or not there is anything to install. It is
	// still admin-only — a non-admin is shown no menu at all, not a menu whose
	// one option refuses them.
	if h.Admin {
		version.Accessory = menuElem{
			Type:     "overflow",
			ActionID: HomeMenuActionID,
			Options: []menuOptionObj{
				{Text: plainVerbatim("Restart"), Value: HomeRestartIntent},
			},
		}
	}

	blocks := []any{
		imageBlock{Type: "image", ImageURL: HomePortraitURL, AltText: HomePortraitAlt},
		version,
	}
	blocks = append(blocks, h.promptBlocks()...)
	if h.Update == nil {
		return blocks
	}

	update := plainEmoji(fmt.Sprintf("Update Available: %s", h.Update.Tag))
	button := buttonElem{
		Type:     "button",
		ActionID: HomeUpdateActionID,
		Style:    "primary",
		Value:    HomeUpdateIntent,
		Text:     plain("Update"),
	}
	return append(blocks,
		dividerBlock{Type: "divider"},
		headerBlock{Type: "header", Text: update, Level: 1},
		accessorySection{
			Type:      "section",
			BlockID:   HomeReleaseNotesBlockID,
			Text:      mrkdwn(h.notes()),
			Accessory: button,
		},
	)
}

// promptBlocks renders the editable prompts, under their own divider and
// header.
//
// Nothing at all when there are none, rather than an empty heading: a "Prompts"
// header with nothing under it says a feature exists and is broken, which is
// the opposite of what an unconfigured install should read as.
func (h Home) promptBlocks() []any {
	// The admin gate is re-applied here, not left to the caller that filled
	// Prompts. It is the same rule the controls menu on the version line
	// obeys, and one surface with two places to get the audience wrong is one
	// too many.
	if !h.Admin || len(h.Prompts) == 0 {
		return nil
	}
	blocks := []any{
		dividerBlock{Type: "divider"},
		headerBlock{Type: "header", Text: plainEmoji("Prompts"), Level: 1},
	}
	for _, p := range h.Prompts {
		blocks = append(blocks, p.block())
	}
	return blocks
}

// block renders one prompt row: what it is, what it currently says, and the
// menu that changes it.
func (p HomePrompt) block() accessorySection {
	options := []menuOptionObj{
		{Text: plainVerbatim(MarkerAsk + "  Edit"), Value: HomePromptEditIntent},
	}
	if p.Overridden {
		options = append(options,
			menuOptionObj{Text: plainVerbatim(MarkerFailed + "  Reset to default"), Value: HomePromptResetIntent})
	}
	return accessorySection{
		Type:    "section",
		BlockID: HomePromptBlockPrefix + p.ID,
		Text:    mrkdwn(p.text()),
		Accessory: &menuElem{
			Type: "overflow", ActionID: HomePromptActionID, Options: options,
		},
	}
}

// text is the row body: the label, then the wording in force.
//
// Escaped, on the same rule a digest row is: a prompt is prose somebody typed,
// and an `&` or a `<` in it would otherwise re-open the row's own bold run and
// garble everything after it.
func (p HomePrompt) text() string {
	label := "*" + escapeMrkdwn(p.Label) + "*"
	if !p.Overridden {
		// Said explicitly. Without it, a default and an override that happens to
		// match it are indistinguishable — and only one of them follows a later
		// change to the default.
		label += "  _(default)_"
	}
	body := escapeMrkdwn(Truncate(strings.TrimSpace(p.Text), homePromptLimit, homePromptKeep))
	if body == "" {
		return label
	}
	return label + "\n" + body
}

// View renders the payload `views.publish` takes.
func (h Home) View() any { return homeView{Type: "home", Blocks: h.Blocks()} }

// Fingerprint is a stable digest of the rendered view, so the daemon can skip
// a publish that would change nothing. app_home_opened fires on every glance at
// the app, and republishing an identical view is a Slack call bought for
// nothing.
func (h Home) Fingerprint() string { return fingerprint(h.Blocks()) }

// versionLine is the line under the portrait.
func (h Home) versionLine() string {
	v := strings.TrimSpace(h.Version)
	if v == "" {
		v = "unknown"
	}
	return "Version: " + v
}

// notes is the release body, cut to what a section block will accept.
//
// An empty body is a real case — a release published with no notes — and it
// gets a line saying so rather than an empty section, which Slack rejects.
func (h Home) notes() string {
	notes := strings.TrimSpace(h.Update.Notes)
	if notes == "" {
		return fmt.Sprintf("*%s* is available. It was published without release notes.", h.Update.Tag)
	}
	return Truncate(notes, homeNotesLimit, homeNotesLimit)
}
