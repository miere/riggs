package pullrequest

import (
	"fmt"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/github"
)

// iconURL is the GitHub glyph shown on the card, carried over from the Python
// so migrated cards keep their appearance.
const iconURL = "https://avatars.slack-edge.com/2020-11-25/1527503386626_319578f21381f9641cd8_512.png"

// Card renders a pull request's card.
//
// Every identifier here is load-bearing and matches the Python exactly: the
// action ids the workflow rules match on, and the actions block_id carrying
// the PR ref — which is the only per-card field an overflow click's payload
// includes.
func Card(d github.Detail, summary string, s State, label string) blockkit.Card {
	ref := d.Ref()
	card := blockkit.Card{
		Title:       d.Title,
		Subtitle:    ref,
		IconURL:     iconURL,
		IconAlt:     "GitHub",
		Body:        summary,
		BodyBlockID: "pr_summary",
	}
	if !s.Reviewable {
		card.Collapsed = true
		card.Context = label
		return card
	}
	card.ActionsBlockID = ref
	card.Actions = []blockkit.Element{
		blockkit.Button{ActionID: "approve_only", Text: "Approve", Value: ref, Primary: true},
		blockkit.LinkButton{Text: "Open in Browser", URL: d.URL},
		blockkit.Overflow{ActionID: "pr_overflow", Options: []blockkit.Option{
			{Text: "Approve & Merge", Value: "approve_merge"},
			{Text: "Run Local Review", Value: "run_local_review"},
		}},
	}
	return card
}

// FallbackText is the notification text: what Slack shows in the sidebar, and
// what an agent reading the thread sees instead of "message updated".
func FallbackText(d github.Detail, s State, label string) string {
	if s.Reviewable {
		return "You have been requested to review this pull request: " + d.URL
	}
	if label == "" {
		label = "No longer reviewable"
	}
	return fmt.Sprintf("Pull request %s — %s", d.URL, label)
}

// TagText is the in-thread ping. The wording distinguishes the first ask from
// a re-review, which is the only signal that a card has come back around.
func TagText(slackUserID string, firstTime bool) string {
	if firstTime {
		return fmt.Sprintf("<@%s> this PR is ready for your review.", slackUserID)
	}
	return fmt.Sprintf("<@%s> you've been asked to re-review this PR.", slackUserID)
}

// Key is the ledger key for a pull request's card.
func Key(ref string) string { return "git.pr:" + ref }
