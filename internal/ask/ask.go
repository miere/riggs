// Package ask renders the message that hands one item to somebody else.
//
// Both digests have the action — "Ask for Code Review" on a pull request, "Ask
// for AI Assistance" on a ticket — and both work the same way: post a card
// about the thing, then tag a person under it with a configured wording.
//
// The wording is configuration, but two things in it are NOT. The person being
// asked must be mentioned, and the person who asked must be visible. Those are
// the point of the feature, and a prompt edited to drop one of them would fail
// silently — the message would still post, still read fine, and simply never
// reach anybody. So they are guaranteed here rather than left to the wording,
// which is exactly why this is one function and not two.
package ask

import (
	"fmt"
	"strings"
)

// anonymousRequester stands in for a click Slack could not attribute. It is
// vague because the situation is: somebody asked, and the payload did not say
// who. Naming nobody is better than rendering an empty mention.
const anonymousRequester = "somebody"

// TagText renders the in-thread ping.
//
// prompt is the message. `{user}` (or its alias `{reviewer}`) and `{requester}`
// are replaced with the corresponding mentions, so a configured wording can put
// them wherever it likes. With the pull-request default:
//
//	Hey <@U1>, mind to review this Pull Request? c/c <@U2>
//
// Then, whatever the wording did:
//
//   - tagged is mentioned — prefixed if the prompt did not do it;
//   - requester is copied in — appended as `c/c <@…>` if the prompt did not.
//
// The c/c is dropped when there is nobody to copy, or when the requester IS the
// person being asked: copying somebody in on their own request reads as a
// mistake.
//
// It names no tool and claims no authorship. The ask reads as though the person
// who clicked wrote it, because they did: they chose the item and the person,
// and they are named as the asker.
func TagText(tagged, requester, prompt string) string {
	taggedTag := fmt.Sprintf("<@%s>", tagged)
	requesterTag := fmt.Sprintf("<@%s>", requester)
	if requester == "" {
		// A prompt that places {requester} inline — as the ticket default does —
		// would otherwise render a literal `<@>`, which Slack shows as exactly
		// that. There is a real sentence to write here, so it is written rather
		// than left as debris.
		requesterTag = anonymousRequester
	}

	text := strings.NewReplacer(
		"{user}", taggedTag,
		// The pull-request prompt has said {reviewer} since it shipped, and a
		// live config still holds it. Keeping the alias costs one line and means
		// nobody's configured wording breaks under them.
		"{reviewer}", taggedTag,
		"{requester}", requesterTag,
	).Replace(strings.TrimSpace(prompt))

	if !strings.Contains(text, taggedTag) {
		text = taggedTag + " " + text
	}
	copyIn := requester != "" && requester != tagged
	if copyIn && !strings.Contains(text, requesterTag) {
		text += " c/c " + requesterTag
	}
	return strings.TrimSpace(text)
}
