package blockkit

// Leading glyphs for status lines and menu labels.
//
// Every one of these is a TEXT-PRESENTATION character. That is the whole point
// of naming them in one place, and it is not the same thing as setting a text
// object's `emoji` field to false.
//
// `emoji: false` controls one thing: whether Slack parses `:shortcode:`
// sequences in the string. It has no effect on a literal codepoint that Unicode
// gives emoji presentation — Slack renders those as a colour image whatever the
// flag says, and normalises them back to a shortcode in the message's fallback
// text, which is how this was caught:
//
//	sent:      "⏺ Approving PR — verifying with GitHub…"   (emoji: false)
//	stored as: ":black_circle_for_record: Approving PR …"
//
// So a status marker has to be chosen, not configured. `✓` U+2713 renders as a
// glyph; `⏺` U+23FA renders as an emoji; both look equally plain in source.
const (
	// MarkerRunning heads a line reporting work in progress.
	//
	// U+2022 BULLET. It replaced U+23FA BLACK CIRCLE FOR RECORD, which is the
	// mistake this file exists to prevent.
	MarkerRunning = "•"
	// MarkerDone heads a line reporting success. U+2713 CHECK MARK — not
	// U+2705, which is the green-box emoji.
	MarkerDone = "✓"
	// MarkerFailed heads a line reporting failure. U+2717 BALLOT X — not
	// U+274C, which is the red-cross emoji.
	MarkerFailed = "✗"
	// MarkerWarning heads a line reporting a partial or uncertain outcome.
	//
	// U+203A SINGLE RIGHT-POINTING ANGLE QUOTATION MARK. The obvious choice,
	// U+26A0 WARNING SIGN, is an emoji.
	MarkerWarning = "›"
	// MarkerOpen heads the "open in a browser" menu label. U+29C9 TWO JOINED
	// SQUARES.
	MarkerOpen = "⧉"
	// MarkerAsk heads the "ask someone" menu label. U+270E LOWER RIGHT PENCIL —
	// not U+270F, which is the pencil emoji.
	MarkerAsk = "✎"
)

// emojiPresentation lists the BMP codepoints Slack renders as a colour image
// despite looking like ordinary symbols in an editor.
//
// It is a curated list, not a Unicode table: the standard's Emoji_Presentation
// property is not in the standard library, and the handful below are the ones
// anybody actually reaches for when labelling a status line. Add to it when
// something new gets caught.
var emojiPresentation = map[rune]string{
	'⏰': "alarm clock",
	'⏳': "hourglass",
	'⏺': "black circle for record",
	'⁉': "exclamation question",
	'‼': "double exclamation",
	'ℹ': "information",
	'↔': "left right arrow",
	'↩': "right hook arrow",
	'⌚': "watch",
	'▶': "play button",
	'◀': "reverse button",
	'⚠': "warning sign",
	'⛔': "no entry",
	'✅': "white heavy check mark",
	'❌': "cross mark",
	'❗': "exclamation mark",
	'⭐': "star",
	'⭕': "heavy large circle",
	'☑': "ballot box with check",
	'✔': "heavy check mark",
	'✖': "heavy multiplication x",
	'⛓': "chains",
	'✨': "sparkles",
}

// EmojiPresentation reports whether r is a codepoint Slack renders as an emoji
// image, and names it. Anything at U+1F000 or above is one by definition.
func EmojiPresentation(r rune) (string, bool) {
	if r >= 0x1F000 {
		return "supplementary emoji plane", true
	}
	name, found := emojiPresentation[r]
	return name, found
}

// ContainsEmoji reports the first emoji-presentation rune in s, if any.
func ContainsEmoji(s string) (rune, string, bool) {
	for _, r := range s {
		if name, isEmoji := EmojiPresentation(r); isEmoji {
			return r, name, true
		}
	}
	return 0, "", false
}
