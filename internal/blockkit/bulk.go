package blockkit

import (
	"strings"
)

// This file is the *bulk* block: one message carrying many items, each its own
// row with its own menu. It is how Riggs sends anything in bulk from now on.
//
// It deliberately does not reuse Card (card.go). They look similar and they are
// not the same feature: a Card is one entity's self-updating container, a Digest
// is a list whose membership changes underneath it. They will evolve apart, so
// only what is provably identical is shared — the fingerprint rule, and the
// primitive text/icon objects. Everything with its own lifecycle is here.
//
// Slack's own vocabulary differs too. The container card nests its controls in
// an `actions` row inside a `container`; a digest row is a plain `section` with
// an `overflow` *accessory*, under one `card` header block. The row's options
// can also carry a `url`, which is how "Open on Browser" costs no interaction.

// Digest is a bulk notification: a header, then one row per item.
type Digest struct {
	// Title and Subtitle head the message ("GitHub - Pull Requests").
	Title    string
	Subtitle string
	// IconURL and IconAlt are the header glyph.
	IconURL string
	IconAlt string
	// Rows are the items, in the order they are shown.
	Rows []Row
}

// Row is one item in a digest.
type Row struct {
	// BlockID carries the item's identity — for a pull request, owner/repo#N.
	//
	// This is load-bearing, not decoration. An overflow click's payload reports
	// the block_id of the block it lives on but not its siblings' values, so
	// this is the only place a per-row reference can travel back to the router.
	BlockID string

	// Title is the item's headline. It is truncated for display; the full text
	// is never needed, since the row links out.
	Title string
	// Meta is the second line — the reference and the author.
	Meta string

	// Done strikes the row through and, by convention of the caller, comes with
	// a reduced set of options. A reviewed, merged, closed or stale item stays
	// visible in the message it was announced in rather than vanishing from it.
	Done bool

	// ActionID names the row's overflow menu. Every row in a digest uses the
	// same one; the router keys on (action_id, option value) and recovers the
	// item from BlockID.
	ActionID string
	// Options are the menu entries. Slack permits between one and five; a row
	// with none renders no menu at all.
	Options []MenuOption
}

// MenuOption is one entry in a row's overflow menu.
//
// It is a separate type from card.go's Option because of URL: a container
// card's overflow has never carried one (it renders a sibling link button
// instead), and collapsing the two would couple two lifecycles that are
// about to diverge.
type MenuOption struct {
	// Text is the label, shown verbatim — the leading glyphs are literal
	// characters, not :emoji: codes.
	Text string
	// Value is the bare intent token the router matches on ("approve_merge").
	// It never varies per row; the row's identity is in Row.BlockID.
	Value string
	// URL, when set, makes the option open a link. Slack still delivers an
	// interaction for it, which the router reports as unhandled — that is
	// cheaper than a handler that exists only to do nothing.
	URL string
}

// A digest row is one line in a list of ten, so a full pull-request title would
// push the reference and author onto a wrap and cost more than it tells you.
//
// titleLimit is the longest title shown untouched; a longer one is cut to
// titleKeep runes and given an ellipsis, so the rendered result is never more
// than titleKeep+1.
const (
	titleLimit = 50
	titleKeep  = 47
)

// --- wire types -----------------------------------------------------------
// Ordered structs, so the encoded bytes are stable and the fingerprint means
// something. See fingerprint.go.

// mrkdwnObj is a text object carrying `verbatim`, which the header card
// declares and the shared textObj does not model.
type mrkdwnObj struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Verbatim bool   `json:"verbatim"`
}

type cardBlock struct {
	Type     string    `json:"type"`
	Icon     *iconObj  `json:"icon,omitempty"`
	Title    mrkdwnObj `json:"title"`
	Subtitle mrkdwnObj `json:"subtitle"`
}

type menuOptionObj struct {
	Text    textObj     `json:"text"`
	Value   string      `json:"value"`
	URL     string      `json:"url,omitempty"`
	Confirm *confirmObj `json:"confirm,omitempty"`
}

// confirmObj is Slack's confirmation dialog, attached to an option that cannot
// be undone.
//
// A pointer with omitempty, so an option without one encodes exactly the bytes
// it always did — every existing menu's fingerprint (§7c) depends on that.
type confirmObj struct {
	Title   textObj `json:"title"`
	Text    textObj `json:"text"`
	Confirm textObj `json:"confirm"`
	Deny    textObj `json:"deny"`
	Style   string  `json:"style,omitempty"`
}

type menuElem struct {
	Type     string          `json:"type"`
	ActionID string          `json:"action_id"`
	Options  []menuOptionObj `json:"options"`
}

// rowBlock is a section with an accessory. The shared sectionBlock has no
// accessory field, and giving it one would change the bytes every container
// card renders.
type rowBlock struct {
	Type      string    `json:"type"`
	BlockID   string    `json:"block_id,omitempty"`
	Text      textObj   `json:"text"`
	Accessory *menuElem `json:"accessory,omitempty"`
}

// plainVerbatim is a plain_text object with emoji interpretation off, which is
// what the menu labels need: their leading glyphs are literal characters.
func plainVerbatim(s string) textObj {
	no := false
	return textObj{Type: "plain_text", Text: s, Emoji: &no}
}

// Empty reports whether the digest has nothing to show.
//
// A digest that empties out is deleted rather than updated to an empty shell —
// a header with no items under it is worse than no message, since it reads as
// "nothing needs you" while occupying the same space as when something did.
func (d Digest) Empty() bool { return len(d.Rows) == 0 }

// Text renders one row's body: the title, then the reference line.
//
// A done row's title is struck through. Only the title is struck: the reference
// and author stay legible, because they are what you read to find the thing
// again.
func (r Row) Text() string {
	title := "*" + escapeMrkdwn(Truncate(r.Title, titleLimit, titleKeep)) + "*"
	if r.Done {
		title = "~" + title + "~"
	}
	if r.Meta == "" {
		return title
	}
	return title + "\n" + r.Meta
}

// Blocks renders the digest as the message's blocks array.
func (d Digest) Blocks() []any {
	header := cardBlock{
		Type:     "card",
		Title:    mrkdwnObj{Type: "mrkdwn", Text: d.Title},
		Subtitle: mrkdwnObj{Type: "mrkdwn", Text: d.Subtitle},
	}
	if d.IconURL != "" {
		header.Icon = &iconObj{Type: "image", ImageURL: d.IconURL, AltText: d.IconAlt}
	}

	blocks := make([]any, 0, len(d.Rows)+1)
	blocks = append(blocks, header)
	for _, row := range d.Rows {
		blocks = append(blocks, row.block())
	}
	return blocks
}

// block renders one row.
func (r Row) block() rowBlock {
	b := rowBlock{
		Type:    "section",
		BlockID: r.BlockID,
		Text:    mrkdwn(r.Text()),
	}
	if len(r.Options) == 0 {
		return b
	}
	opts := make([]menuOptionObj, 0, len(r.Options))
	for _, o := range r.Options {
		opts = append(opts, menuOptionObj{Text: plainVerbatim(o.Text), Value: o.Value, URL: o.URL})
	}
	b.Accessory = &menuElem{Type: "overflow", ActionID: r.ActionID, Options: opts}
	return b
}

// Fingerprint is a stable digest of the rendered message, on the same rule the
// container card uses.
func (d Digest) Fingerprint() string { return fingerprint(d.Blocks()) }

// Items lists the rows' block ids, in order. The ledger records this as the
// message's membership: it is what lets a later pass find which message an item
// is currently shown in.
func (d Digest) Items() []string {
	out := make([]string, 0, len(d.Rows))
	for _, r := range d.Rows {
		out = append(out, r.BlockID)
	}
	return out
}

// Truncate returns s unchanged when it is at most limit runes, and otherwise
// cuts it to keep runes plus a single-character ellipsis.
//
// It counts runes, not bytes: a title cut mid-rune renders as a replacement
// character. A trailing space is dropped before the ellipsis, so a cut that
// lands just after a word does not read as "word …".
func Truncate(s string, limit, keep int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if keep <= 0 {
		return "…"
	}
	if keep > len(runes) {
		keep = len(runes)
	}
	return strings.TrimRight(string(runes[:keep]), " ") + "…"
}

// escapeMrkdwn neutralises the three characters Slack reads as markup inside a
// mrkdwn string. A pull request title containing an underscore or an ampersand
// is ordinary, and letting it re-open the row's own bold run would garble every
// row after it.
func escapeMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
