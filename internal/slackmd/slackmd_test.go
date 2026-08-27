package slackmd

import (
	"strings"
	"testing"
)

// The trap this package exists for: GitHub and Slack mean the OPPOSITE thing by
// a single asterisk. Copying emphasis across turns every bold run into italics,
// and it does not fail — it just renders wrong.
func TestEmphasisIsSwappedNotCopied(t *testing.T) {
	cases := map[string]string{
		"**bold**":            "*bold*",
		"__bold__":            "*bold*",
		"*italic*":            "_italic_",
		"_italic_":            "_italic_",
		"~~struck~~":          "~struck~",
		"**bold** and *it*":   "*bold* and _it_",
		"a**b**c":             "a*b*c",
		"no emphasis at all":  "no emphasis at all",
		"**one** two *three*": "*one* two _three_",
	}
	for in, want := range cases {
		if got := Convert(in).Text; got != want {
			t.Errorf("Convert(%q) = %q, want %q", in, got, want)
		}
	}
}

// Slack has no heading in mrkdwn, so a `##` renders as a literal hash.
func TestHeadingsBecomeBoldLines(t *testing.T) {
	got := Convert("# Title\n\nbody\n\n### Deeper\n").Text
	if !strings.Contains(got, "*Title*") {
		t.Errorf("h1 did not become bold: %q", got)
	}
	if !strings.Contains(got, "*Deeper*") {
		t.Errorf("h3 did not become bold: %q", got)
	}
	if strings.Contains(got, "#") {
		t.Errorf("a hash survived: %q", got)
	}
}

// A heading that was already bold must not come out as `**text**`.
func TestAlreadyBoldHeadingIsNotDoubled(t *testing.T) {
	if got := Convert("## **Release 1.2**").Text; got != "*Release 1.2*" {
		t.Fatalf("Convert = %q", got)
	}
}

// Links do not render everywhere these strings end up, so they are numbered and
// their targets handed back.
func TestLinksBecomeNumberedReferences(t *testing.T) {
	r := Convert("See [the docs](https://example.test/docs) and [more](https://example.test/2).")

	if !strings.Contains(r.Text, "the docs [1]") || !strings.Contains(r.Text, "more [2]") {
		t.Fatalf("Text = %q", r.Text)
	}
	if strings.Contains(r.Text, "https://") {
		t.Errorf("a raw URL survived in the text: %q", r.Text)
	}
	if len(r.Links) != 2 {
		t.Fatalf("Links = %+v, want two", r.Links)
	}
	if r.Links[0].URL != "https://example.test/docs" || r.Links[0].N != 1 {
		t.Errorf("Links[0] = %+v", r.Links[0])
	}
}

// An image is a link with a `!` in front. Converting links first would leave a
// stray exclamation mark behind.
func TestImagesAreTakenBeforeLinks(t *testing.T) {
	r := Convert("![a badge](https://img.test/b.svg)")

	if strings.Contains(r.Text, "!") {
		t.Errorf("a stray exclamation mark survived: %q", r.Text)
	}
	if got := r.Text; got != "a badge [1]" {
		t.Errorf("Text = %q", got)
	}
	if len(r.Links) != 1 || !r.Links[0].Image {
		t.Fatalf("Links = %+v, want one image", r.Links)
	}
}

// Numbering continues across lines: a footnote list is only useful if the
// numbers are unique in the document, not in the line.
func TestReferenceNumbersRunAcrossLines(t *testing.T) {
	r := Convert("[a](https://x.test/a)\n\n[b](https://x.test/b)\n\n[c](https://x.test/c)")
	if len(r.Links) != 3 {
		t.Fatalf("Links = %+v", r.Links)
	}
	for i, l := range r.Links {
		if l.N != i+1 {
			t.Errorf("Links[%d].N = %d, want %d", i, l.N, i+1)
		}
	}
}

func TestWithFootnotesListsTheTargets(t *testing.T) {
	r := Convert("see [docs](https://example.test/d)")
	got := r.WithFootnotes()

	if !strings.Contains(got, "[1] https://example.test/d") {
		t.Fatalf("WithFootnotes = %q", got)
	}
	// And the bare text stays free of them, because a card body does not want
	// eight lines of URLs under two lines of prose.
	if strings.Contains(r.Text, "https://") {
		t.Errorf("Text carries the footnotes: %q", r.Text)
	}
}

func TestWithFootnotesIsANoopWithNoLinks(t *testing.T) {
	r := Convert("plain text")
	if r.WithFootnotes() != "plain text" {
		t.Fatalf("WithFootnotes = %q", r.WithFootnotes())
	}
}

// Slack prints the language tag as a literal word on the block's first line.
func TestCodeFencesLoseTheirLanguage(t *testing.T) {
	got := Convert("```kotlin\nval x = 1\n```").Text

	if strings.Contains(got, "kotlin") {
		t.Errorf("the language tag survived: %q", got)
	}
	if !strings.Contains(got, "val x = 1") {
		t.Errorf("the code was lost: %q", got)
	}
}

// Emphasis inside a code block is not emphasis.
func TestCodeContentIsNotConverted(t *testing.T) {
	got := Convert("```\na := **b**\n```").Text
	if !strings.Contains(got, "a := **b**") {
		t.Fatalf("code content was converted: %q", got)
	}
}

// Slack renders no list markup, so a `-` would stay a `-`.
func TestBulletsBecomeGlyphs(t *testing.T) {
	got := Convert("- one\n- two\n  - nested").Text
	if strings.Count(got, "•") != 3 {
		t.Fatalf("bullets = %q", got)
	}
	if !strings.Contains(got, "  •") {
		t.Errorf("nesting indent was lost: %q", got)
	}
}

// Slack reads these as markup, so text containing them must be escaped.
func TestMarkupCharactersAreEscaped(t *testing.T) {
	got := Convert("a < b & c > d").Text
	for _, want := range []string{"&lt;", "&amp;", "&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from %q", want, got)
		}
	}
}

// The template instructions nobody deletes.
func TestHTMLCommentsAreRemoved(t *testing.T) {
	got := Convert("<!-- describe your change -->\nreal content").Text
	if strings.Contains(got, "describe your change") {
		t.Fatalf("an HTML comment survived: %q", got)
	}
	if !strings.Contains(got, "real content") {
		t.Fatalf("content was lost: %q", got)
	}
}

// --- excerpts ---------------------------------------------------------------

func TestFirstParagraphsTakesN(t *testing.T) {
	src := "one\n\ntwo\n\nthree\n\nfour"
	if got := FirstParagraphs(src, 2); got != "one\n\ntwo" {
		t.Fatalf("FirstParagraphs = %q", got)
	}
}

// A multi-line paragraph is one paragraph.
func TestFirstParagraphsKeepsSoftWraps(t *testing.T) {
	src := "line one\nline two\n\nsecond para"
	if got := FirstParagraphs(src, 1); got != "line one\nline two" {
		t.Fatalf("FirstParagraphs = %q", got)
	}
}

// Both of these are routinely the first thing in a pull-request body, and
// neither says anything.
func TestFirstParagraphsSkipsNoise(t *testing.T) {
	src := "<!-- PR template: describe your change -->\n\n" +
		"![build](https://img.test/badge.svg) ![cov](https://img.test/cov.svg)\n\n" +
		"The actual description.\n\nSecond paragraph."

	got := FirstParagraphs(src, 2)
	if strings.Contains(got, "PR template") {
		t.Errorf("the comment was counted: %q", got)
	}
	if strings.Contains(got, "badge") {
		t.Errorf("a badges-only paragraph was counted: %q", got)
	}
	if !strings.HasPrefix(got, "The actual description.") {
		t.Fatalf("FirstParagraphs = %q", got)
	}
	if !strings.Contains(got, "Second paragraph.") {
		t.Errorf("only one paragraph was taken: %q", got)
	}
}

// A paragraph with an image AND prose is prose, and is kept.
func TestFirstParagraphsKeepsProseBesideAnImage(t *testing.T) {
	src := "![shot](https://img.test/s.png) and here is why it matters"
	if got := FirstParagraphs(src, 1); !strings.Contains(got, "why it matters") {
		t.Fatalf("FirstParagraphs = %q", got)
	}
}

func TestFirstParagraphsOfNothing(t *testing.T) {
	for _, src := range []string{"", "   ", "<!-- only a comment -->"} {
		if got := FirstParagraphs(src, 2); strings.TrimSpace(got) != "" {
			t.Errorf("FirstParagraphs(%q) = %q, want empty", src, got)
		}
	}
}

// The whole job in one call, which is what a card body uses.
func TestExcerpt(t *testing.T) {
	src := "## Summary\n\nThis **fixes** the [thing](https://example.test/t).\n\n" +
		"Second paragraph.\n\nThird paragraph nobody sees."

	got := Excerpt(src, 2)
	if strings.Contains(got, "Third paragraph") {
		t.Errorf("Excerpt took too much: %q", got)
	}
	if !strings.Contains(got, "*fixes*") {
		t.Errorf("emphasis was not converted: %q", got)
	}
	if !strings.Contains(got, "thing [1]") {
		t.Errorf("the link was not numbered: %q", got)
	}
	// No footnote list: an excerpt is read at a glance.
	if strings.Contains(got, "https://") {
		t.Errorf("Excerpt appended footnotes: %q", got)
	}
}

// The bug this was written for: a Dependabot description is ~99% collapsed
// release notes, and the HTML does not merely take up room — escaping grows it,
// so the body arrives over Slack's 3,000-character section limit and the whole
// message is rejected with invalid_blocks. What survives is the one sentence
// that says what the change does.
func TestDependabotBodyCollapsesToItsOneSentence(t *testing.T) {
	body := "Bumps [pytest](https://github.com/pytest-dev/pytest) from 8.4.2 to 9.0.3.\n" +
		"<details>\n<summary>Release notes</summary>\n<p><em>Sourced from " +
		"<a href=\"https://github.com/pytest-dev/pytest/releases\">releases</a>.</em></p>\n" +
		"<blockquote>\n<h2>9.0.3</h2>\n<ul><li>a bug fix</li></ul>\n</blockquote>\n</details>\n" +
		"<details>\n<summary>Commits</summary>\n<ul><li><code>abc1234</code> a commit</li></ul>\n" +
		"</details>\n<br />\n"

	got := Excerpt(body, 2)

	if !strings.Contains(got, "from 8.4.2 to 9.0.3") {
		t.Errorf("the sentence that matters did not survive: %q", got)
	}
	for _, gone := range []string{"Release notes", "Commits", "blockquote", "abc1234", "&lt;"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived the strip: %q", gone, got)
		}
	}
	// Comfortably inside the section limit, rather than merely under it.
	if len(got) > 200 {
		t.Errorf("excerpt is %d chars, expected the one sentence: %q", len(got), got)
	}
}

// Dependabot folds a details block inside another for its ignore conditions. A
// non-greedy pattern stops at the inner closing tag and leaves a stray
// `</details>` in the body.
func TestNestedDetailsLeaveNoStrayTag(t *testing.T) {
	got := FirstParagraphs("before\n<details><summary>s</summary>outer"+
		"<details><summary>inner</summary>deep</details>tail</details>\nafter", 2)
	if strings.Contains(got, "details") || strings.Contains(got, "deep") {
		t.Errorf("nested block was not fully removed: %q", got)
	}
	for _, want := range []string{"before", "after"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q outside the block was lost: %q", want, got)
		}
	}
}

// An unclosed tag collapses the rest of the body on GitHub too, so taking the
// remainder with it is what the reader already sees. A closing tag with no
// opener is malformed input and is passed through rather than guessed at.
func TestUnbalancedDetails(t *testing.T) {
	if got := FirstParagraphs("keep this\n<details><summary>s</summary>swallowed", 2); strings.Contains(got, "swallowed") {
		t.Errorf("unclosed block did not take the rest: %q", got)
	} else if !strings.Contains(got, "keep this") {
		t.Errorf("text before the block was lost: %q", got)
	}
	if got := FirstParagraphs("orphan</details>", 2); !strings.Contains(got, "orphan") {
		t.Errorf("a stray closing tag ate the body: %q", got)
	}
}

// Stripping a details block routinely leaves the `<br />` that followed it
// alone in a paragraph. Escaped, it reaches the card as a literal `<br />`.
func TestParagraphOfOnlyMarkupIsSkipped(t *testing.T) {
	got := Excerpt("the real text\n\n<br />\n\nsecond paragraph", 2)
	if strings.Contains(got, "br") {
		t.Errorf("a markup-only paragraph was kept: %q", got)
	}
	if !strings.Contains(got, "second paragraph") {
		t.Errorf("the markup-only paragraph was counted against n: %q", got)
	}
}

// Deterministic, which is the other half of why this replaced an LLM call: a
// card body that changes between renders cannot be fingerprinted.
func TestConversionIsDeterministic(t *testing.T) {
	src := "# T\n\n**a** [b](https://c.test) ~~d~~\n\n- e"
	first := Convert(src).Text
	for i := 0; i < 20; i++ {
		if got := Convert(src).Text; got != first {
			t.Fatalf("run %d differed:\n %q\n %q", i, got, first)
		}
	}
}
