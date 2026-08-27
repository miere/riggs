// Package slackmd converts GitHub-flavoured Markdown into Slack's mrkdwn.
//
// The two look alike and are not. Slack has no headings, no link syntax that
// survives in every surface, no language tag on a code fence, and — the trap —
// it means the OPPOSITE thing by a single asterisk. Text moved across without
// translation does not fail; it renders wrong, quietly.
//
// Conversion is lossy on purpose, because the target is genuinely smaller than
// the source. What is lost is recorded rather than dropped: a suppressed link
// becomes a numbered reference and comes back in Result.Links, so a caller can
// print a footnote list, or not.
//
// It is deliberately a *simplified* converter, not a Markdown implementation.
// It reads a line at a time and knows nothing about nested structure. That is
// enough for a pull-request description or a release note, and stopping there
// is what keeps it something you can read in one sitting.
package slackmd

import (
	"fmt"
	"regexp"
	"strings"
)

// Link is a reference the conversion suppressed.
type Link struct {
	// N is its footnote number, from 1.
	N int
	// Text is what was shown; URL is where it pointed.
	Text string
	URL  string
	// Image marks a link that was an image rather than a hyperlink.
	Image bool
}

// Result is converted text, plus what had to be set aside.
type Result struct {
	Text  string
	Links []Link
}

// String renders the text alone.
func (r Result) String() string { return r.Text }

// WithFootnotes appends the numbered list of suppressed links.
//
// Separate from Convert because the right answer differs by surface: release
// notes want the list, a two-line card body does not — the references are still
// legible without it, and eight lines of URLs under a two-line excerpt is worse
// than no URLs at all.
func (r Result) WithFootnotes() string {
	if len(r.Links) == 0 {
		return r.Text
	}
	var b strings.Builder
	b.WriteString(r.Text)
	b.WriteString("\n")
	for _, l := range r.Links {
		fmt.Fprintf(&b, "\n[%d] %s", l.N, l.URL)
	}
	return b.String()
}

// sentinel parks converted bold runs so the italic rule cannot match them. It
// is a NUL byte: valid Markdown never contains one.
const sentinel = "\x00"

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	heading     = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$`)
	setextRule  = regexp.MustCompile(`^\s{0,3}(=+|-{2,})\s*$`)
	fence       = regexp.MustCompile("^\\s*(```|~~~)\\s*[A-Za-z0-9_+-]*\\s*$")
	bullet      = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	image       = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	link        = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	autolink    = regexp.MustCompile(`<(https?://[^>\s]+)>`)
	boldStars   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	boldUnders  = regexp.MustCompile(`__([^_]+)__`)
	strike      = regexp.MustCompile(`~~([^~]+)~~`)
	italicStar  = regexp.MustCompile(`(^|[^*\w])\*([^*\n]+)\*($|[^*\w])`)
)

// Convert translates src.
//
// The rules, and why each exists:
//
//   - Headings become a bold line. Slack has no heading in mrkdwn, so a `##`
//     would otherwise render as a literal hash.
//   - Bold and italic are SWAPPED, not copied. GitHub's `**x**` is Slack's
//     `*x*`, and GitHub's `*x*` is Slack's `_x_`. Copying them across turns
//     every bold run into an italic one.
//   - Strikethrough loses a tilde: `~~x~~` is `~x~`.
//   - Links and images become `text [N]`, with the target recorded. Slack's
//     `<url|text>` does not render everywhere these strings end up.
//   - A code fence loses its language tag, which Slack shows as a literal word
//     on the first line of the block.
//   - Bullets become `•`. Slack renders no list markup, so a `-` stays a `-`.
//   - `&`, `<` and `>` are escaped, because Slack reads them as markup.
//
// Fenced code is passed through untouched apart from the fence itself and the
// escaping: emphasis inside a code block is not emphasis.
func Convert(src string) Result {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = htmlComment.ReplaceAllString(src, "")

	var out []string
	var links []Link
	inCode := false

	for _, line := range strings.Split(src, "\n") {
		if fence.MatchString(line) {
			inCode = !inCode
			// The language tag is dropped: Slack prints it as text.
			out = append(out, "```")
			continue
		}
		if inCode {
			out = append(out, escape(line))
			continue
		}
		converted, found := convertLine(line, len(links))
		links = append(links, found...)
		out = append(out, converted)
	}

	text := strings.Join(out, "\n")
	text = collapseBlankRuns(text)
	return Result{Text: strings.TrimSpace(text), Links: links}
}

// convertLine converts one line outside a code fence. start is how many links
// have already been numbered.
func convertLine(line string, start int) (string, []Link) {
	// A setext underline belongs to the line above, which was already emitted;
	// dropping it is closer than rendering a row of equals signs.
	if setextRule.MatchString(line) && strings.TrimSpace(line) != "" {
		if strings.HasPrefix(strings.TrimSpace(line), "=") {
			return "", nil
		}
	}

	isHeading := false
	if m := heading.FindStringSubmatch(line); m != nil {
		line = m[1]
		isHeading = true
	}

	indent := ""
	if m := bullet.FindStringSubmatch(line); m != nil {
		indent, line = m[1], m[2]
		indent += "•  "
	}

	line = escape(line)

	var found []Link
	next := start
	take := func(text, url string, isImage bool) string {
		next++
		found = append(found, Link{N: next, Text: text, URL: url, Image: isImage})
		text = strings.TrimSpace(text)
		if text == "" {
			text = url
		}
		return fmt.Sprintf("%s [%d]", text, next)
	}

	// Images first: an image is a link with a `!` in front, so the link pattern
	// would otherwise claim it and leave a stray exclamation mark.
	line = replaceAll(line, image, func(m []string) string { return take(m[1], m[2], true) })
	line = replaceAll(line, link, func(m []string) string { return take(m[1], m[2], false) })
	line = replaceAll(line, autolink, func(m []string) string { return take(m[1], m[1], false) })

	// Bold is parked behind a sentinel before italics are touched.
	//
	// Doing it in the obvious order does not work: `**x**` becomes `*x*`, and
	// the italic rule — whose Slack output is `_x_` — then matches its own
	// predecessor's output and turns every bold run into an italic one. The
	// sentinel is a byte no Markdown source can contain, so nothing else can
	// match it in between.
	line = boldStars.ReplaceAllString(line, sentinel+"$1"+sentinel)
	line = boldUnders.ReplaceAllString(line, sentinel+"$1"+sentinel)
	line = strike.ReplaceAllString(line, "~$1~")
	line = italicStar.ReplaceAllString(line, "${1}_${2}_${3}")
	line = strings.ReplaceAll(line, sentinel, "*")

	if isHeading {
		if t := strings.TrimSpace(stripEmphasis(line)); t != "" {
			line = "*" + t + "*"
		}
	}
	return indent + line, found
}

// replaceAll applies fn to every match, which ReplaceAllStringFunc cannot do
// because it hands back the whole match rather than its groups.
func replaceAll(s string, re *regexp.Regexp, fn func([]string) string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		return fn(re.FindStringSubmatch(match))
	})
}

// stripEmphasis removes markers already applied, so a heading that was bold in
// the source does not come out as `**text**`.
func stripEmphasis(s string) string {
	return strings.NewReplacer("*", "", "_", "", sentinel, "").Replace(s)
}

// escape neutralises the three characters Slack reads as markup.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// collapseBlankRuns squeezes three or more newlines down to two. Markdown
// tolerates loose spacing; a Slack block renders every one of them.
func collapseBlankRuns(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// FirstParagraphs returns the first n paragraphs of src, before conversion.
//
// A paragraph is a run of non-blank lines. Two things are skipped rather than
// counted, because both are routinely the first thing in a pull-request body
// and neither says anything: an HTML comment (the template instructions nobody
// deletes), and a paragraph that is nothing but images or badges.
func FirstParagraphs(src string, n int) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = htmlComment.ReplaceAllString(src, "")

	var kept []string
	for _, para := range strings.Split(src, "\n\n") {
		para = strings.Trim(para, "\n ")
		if para == "" || isOnlyImages(para) {
			continue
		}
		kept = append(kept, para)
		if len(kept) == n {
			break
		}
	}
	return strings.Join(kept, "\n\n")
}

// isOnlyImages reports whether a paragraph is nothing but images and badges.
func isOnlyImages(para string) bool {
	stripped := strings.TrimSpace(image.ReplaceAllString(para, ""))
	stripped = strings.TrimSpace(link.ReplaceAllString(stripped, ""))
	return stripped == "" && strings.Contains(para, "![")
}

// Excerpt is the whole job in one call: the first n paragraphs, converted.
//
// Footnotes are deliberately NOT appended. An excerpt is read at a glance, and
// a list of URLs under two lines of text costs more attention than the links
// were worth.
func Excerpt(src string, n int) string {
	return Convert(FirstParagraphs(src, n)).Text
}
