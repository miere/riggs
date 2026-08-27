// Package blockkit renders the collapsible `container` card: one entity, one
// self-updating message.
//
// Nothing drives it on a schedule any more — the review queue moved to the bulk
// digest in bulk.go (§12c) — but it is retained on purpose rather than left to
// rot: this shape is about to be reused, and the ticket queue still posts it.
//
// Both automations already draw the same card — a title, a subtitle, an icon,
// an AI-written body, and then either an actions row (live) or a single
// context line explaining what happened (resolved). In the Python that shape
// is duplicated in two `blocks.py` files that have drifted in small ways. Here
// it is one type.
//
// Block JSON is built from ordered structs rather than maps so the output byte
// sequence is deterministic. That is what makes Fingerprint meaningful: the
// ledger updates a message only when its fingerprint changes, so an unstable
// encoding would rewrite every card on every tick.
package blockkit

// The fingerprint helper this file's Card.Fingerprint delegates to lives in
// fingerprint.go, shared with the bulk digest.

// Card is a collapsible container card.
type Card struct {
	// Title is the card's headline (the PR title, the ticket summary).
	Title string
	// Subtitle is the identifier under it (owner/repo#N, or the ticket key).
	Subtitle string
	// IconURL and IconAlt are the card glyph.
	IconURL string
	IconAlt string

	// Body is the mrkdwn summary. Empty renders no section block at all,
	// which is how a resolved ticket card collapses to just its status line.
	Body string
	// BodyBlockID names the section block, for parity with the existing
	// cards (`pr_summary`, `ticket_description`).
	BodyBlockID string

	// Collapsed sets the card's initial state.
	Collapsed bool

	// ActionsBlockID is the block_id of the actions row. It carries the
	// per-card identity, because an overflow click's payload includes its
	// block_id but not its sibling elements' values — that is the only place
	// a workflow rule can recover which PR or ticket it is acting on.
	ActionsBlockID string
	// Actions is the row of interactive elements. Empty renders no row.
	Actions []Element

	// Context is the bottom label shown when there are no actions ("Merged
	// at: …", "Assigned to: …").
	Context string
}

// Element is one interactive element in the actions row.
type Element interface{ marshal() any }

// Button is a plain action button. Value is what the interaction payload
// carries back.
type Button struct {
	ActionID string
	Text     string
	Value    string
	Primary  bool
}

// LinkButton opens a URL.
//
// Slack still delivers an interaction for it — the belief that it does not was
// wrong, and cost a ⚠ on every click until the daemon started acking
// unconditionally. ActionID is optional and exists so the click is
// *identifiable* in the log; without one it arrives with an empty action_id,
// indistinguishable from a malformed payload.
type LinkButton struct {
	ActionID string
	Text     string
	URL      string
}

// Overflow is the "…" menu. Each option's value is a bare intent token so a
// workflow rule can match it exactly on selected_option.value.
type Overflow struct {
	ActionID string
	Options  []Option
}

// Option is one entry in an Overflow.
type Option struct {
	Text  string
	Value string
}

// --- wire types -----------------------------------------------------------
// Ordered structs, so the encoded bytes are stable.

type textObj struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji *bool  `json:"emoji,omitempty"`
}

func plain(s string) textObj  { return textObj{Type: "plain_text", Text: s} }
func mrkdwn(s string) textObj { return textObj{Type: "mrkdwn", Text: s} }

func plainEmoji(s string) textObj {
	yes := true
	return textObj{Type: "plain_text", Text: s, Emoji: &yes}
}

type iconObj struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	AltText  string `json:"alt_text"`
}

type sectionBlock struct {
	Type    string  `json:"type"`
	BlockID string  `json:"block_id,omitempty"`
	Text    textObj `json:"text"`
}

type dividerBlock struct {
	Type string `json:"type"`
}

type contextBlock struct {
	Type     string    `json:"type"`
	Elements []textObj `json:"elements"`
}

type actionsBlock struct {
	Type     string `json:"type"`
	BlockID  string `json:"block_id,omitempty"`
	Elements []any  `json:"elements"`
}

type containerBlock struct {
	Type             string   `json:"type"`
	Width            string   `json:"width"`
	Title            textObj  `json:"title"`
	Subtitle         textObj  `json:"subtitle"`
	Icon             *iconObj `json:"icon,omitempty"`
	IsCollapsible    bool     `json:"is_collapsible"`
	DefaultCollapsed bool     `json:"default_collapsed"`
	ChildBlocks      []any    `json:"child_blocks"`
}

type buttonElem struct {
	Type     string  `json:"type"`
	ActionID string  `json:"action_id,omitempty"`
	Style    string  `json:"style,omitempty"`
	URL      string  `json:"url,omitempty"`
	Value    string  `json:"value,omitempty"`
	Text     textObj `json:"text"`
}

type overflowOption struct {
	Text  textObj `json:"text"`
	Value string  `json:"value"`
}

type overflowElem struct {
	Type     string           `json:"type"`
	ActionID string           `json:"action_id"`
	Options  []overflowOption `json:"options"`
}

func (b Button) marshal() any {
	e := buttonElem{Type: "button", ActionID: b.ActionID, Value: b.Value, Text: plainEmoji(b.Text)}
	if b.Primary {
		e.Style = "primary"
	}
	return e
}

func (b LinkButton) marshal() any {
	return buttonElem{Type: "button", ActionID: b.ActionID, URL: b.URL, Text: plainEmoji(b.Text)}
}

func (o Overflow) marshal() any {
	opts := make([]overflowOption, 0, len(o.Options))
	for _, opt := range o.Options {
		opts = append(opts, overflowOption{Text: plainEmoji(opt.Text), Value: opt.Value})
	}
	return overflowElem{Type: "overflow", ActionID: o.ActionID, Options: opts}
}

// bodyLimit caps the body section's text, in runes.
//
// Slack's section block takes at most 3,000 characters, and going over does not
// truncate the block — it rejects the whole message with invalid_blocks. An
// uncapped body is therefore not a card that renders badly, it is a card that
// never posts, and the click that asked for it looks to the user as though
// nothing happened at all.
//
// The margin under the limit is deliberate. The count Slack applies is its own,
// and a hundred characters of headroom costs nothing on a body that exists to
// be read at a glance.
//
// The cut can land inside a mrkdwn run and leave an unpaired `*`, which Slack
// renders as a literal asterisk. That is a blemish on a card; the alternative
// it replaces is no card.
//
// This is the backstop, not the cure. A body that reaches four figures is
// usually one nobody wanted rendered in full — see slackmd.stripDetails, which
// removes the collapsed HTML that puts a Dependabot description over the line
// in the first place.
const bodyLimit = 2900

// Blocks renders the card as the message's blocks array.
func (c Card) Blocks() []any {
	children := make([]any, 0, 3)
	if c.Body != "" {
		children = append(children, sectionBlock{
			Type: "section", BlockID: c.BodyBlockID, Text: mrkdwn(Truncate(c.Body, bodyLimit, bodyLimit)),
		})
	}
	children = append(children, dividerBlock{Type: "divider"})

	switch {
	case len(c.Actions) > 0:
		elems := make([]any, 0, len(c.Actions))
		for _, a := range c.Actions {
			elems = append(elems, a.marshal())
		}
		children = append(children, actionsBlock{
			Type: "actions", BlockID: c.ActionsBlockID, Elements: elems,
		})
	case c.Context != "":
		children = append(children, contextBlock{
			Type: "context", Elements: []textObj{mrkdwn(c.Context)},
		})
	}

	container := containerBlock{
		Type:             "container",
		Width:            "wide",
		Title:            plain(c.Title),
		Subtitle:         plain(c.Subtitle),
		IsCollapsible:    true,
		DefaultCollapsed: c.Collapsed,
		ChildBlocks:      children,
	}
	if c.IconURL != "" {
		container.Icon = &iconObj{Type: "image", ImageURL: c.IconURL, AltText: c.IconAlt}
	}
	return []any{container}
}

// Fingerprint is a stable digest of the rendered card. The ledger compares it
// to decide whether a Slack update is needed at all, so two renders of an
// unchanged card must produce identical bytes.
func (c Card) Fingerprint() string { return fingerprint(c.Blocks()) }

// TextBlocks renders a bare mrkdwn message — the shape used for threaded
// replies (the reviewer tag, the idle nudge), which are not cards.
func TextBlocks(s string) []any {
	return []any{sectionBlock{Type: "section", Text: mrkdwn(s)}}
}

// ContextBlocks renders a single context line — the shape the approve handler
// posts its outcome in.
func ContextBlocks(s string) []any {
	no := false
	return []any{contextBlock{
		Type:     "context",
		Elements: []textObj{{Type: "plain_text", Text: s, Emoji: &no}},
	}}
}
