package pullrequest

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/github"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slackmd"
)

// "Ask for Code Review": hand one pull request to somebody else.
//
// The ask is a CARD, not a line of text — the same container shape the per-PR
// queue used, which is why that renderer was retained rather than retired
// (§12c). It is not identical, and the differences are the point:
//
//   - No overflow menu. The reviewer is being asked one question, and a menu of
//     alternatives is a worse way to ask it. Approve and Open in Browser stay.
//   - Approving from here leaves NO comment on GitHub. The reviewer's approval
//     is their own; a body attached to it would be words they did not write.
//
// Nothing is delegated and no review is started. Riggs posts the card, tags the
// reviewer in its thread, and stops.

const (
	// AskActionID names the ask card's actions row.
	//
	// Distinct from the legacy card's `approve_only`, which Murtaugh's workflow
	// rules still route for the cards its own app posted. These are Riggs'
	// cards, answered by Riggs' daemon, and giving them their own id keeps the
	// two dispatch tables from ever having to agree.
	AskActionID = "pr_ask_review"
	// AskOpenActionID names the link button. No handler is registered for it —
	// Slack opens the URL itself — but it is named so the interaction it
	// nevertheless raises is identifiable in the daemon's log.
	AskOpenActionID = "pr_ask_open"
	// AskKeyPrefix namespaces the ask cards in the ledger.
	//
	// They are tracked, which they were not at first. The reasoning then — "an
	// ask is a one-off, not a card to maintain" — held right up until an
	// approval needed to change one: a card still offering Approve for a pull
	// request that is already merged is worse than no card.
	AskKeyPrefix = "git.pr.ask:"
	// IntentApprove is the bare token the approve button carries.
	//
	// Bare, because the router matches it exactly (§7b). The pull request
	// reference rides in the actions block_id, as everywhere else.
	IntentApprove = "approve"
)

// AskKey is the ledger key for a pull request's review-request card.
func AskKey(ref string) string { return AskKeyPrefix + ref }

// Detailer reads one pull request. github.Client satisfies it.
type Detailer interface {
	PullRequestDetail(ctx context.Context, repo string, number int) (github.Detail, error)
}

// Resolver turns a configured handle into a Slack user id. *slack.API
// satisfies it.
type Resolver interface {
	LookupUserID(ctx context.Context, target slack.Target, handle string) (string, error)
}

// BodyParagraphs is how much of a pull-request description a card shows.
//
// Two: enough for the author's own summary of what they did, and short enough
// that the card stays a card. Anything past it is on the pull request, one
// click away.
const BodyParagraphs = 2

// Ask card states, recorded on the ledger entry so a pass can tell an open
// request from a settled one without re-reading GitHub.
const (
	AskStateOpen = "open"
	AskStateDone = "done"
)

// Asker posts a review request.
type Asker struct {
	gh       Detailer
	store    *notify.Store
	notifier *notify.Notifier
	poster   slack.Poster

	// resolver turns a configured handle into an id, when the config holds one.
	resolver Resolver
	// reviewer is the configured reviewer, as written: an id, a mention, or a
	// handle. It is normalised at send time, never assumed.
	reviewer string
	// channel is where the card is posted. Empty DMs the reviewer.
	channel string
	// prompt is the wording of the tag.
	prompt string
}

// NewAsker builds the asker.
func NewAsker(gh Detailer, store *notify.Store, n *notify.Notifier, poster slack.Poster,
	reviewer, channel, prompt string) *Asker {
	return &Asker{gh: gh, store: store, notifier: n, poster: poster,
		reviewer: reviewer, channel: channel, prompt: prompt}
}

// WithResolver supplies the handle lookup. Without one, a configured handle is
// an error rather than a mention that reaches nobody.
func (a *Asker) WithResolver(r Resolver) *Asker {
	a.resolver = r
	return a
}

// reviewerID normalises the configured reviewer and resolves a handle if that
// is what it is.
//
// A handle costs one users.list call, and only on a machine whose config holds
// one — which after `riggs install` it does not, because the installer resolves
// it once and stores the id.
func (a *Asker) reviewerID(ctx context.Context, target slack.Target) (string, error) {
	ref := slack.ParseUserRef(a.reviewer)
	switch {
	case ref.IsID():
		return ref.ID, nil
	case ref.Handle == "":
		return "", fmt.Errorf("cannot ask for a review: no reviewer configured (set review-request.user-id or admin.slack-user-id)")
	case a.resolver == nil:
		return "", fmt.Errorf("review-request.user-id is %q, which is a handle rather than a Slack id, and nothing is wired to resolve it", a.reviewer)
	}
	id, err := a.resolver.LookupUserID(ctx, target, ref.Handle)
	if err != nil {
		return "", fmt.Errorf("review-request.user-id %q: %w", a.reviewer, err)
	}
	return id, nil
}

// AskResult reports where the ask went.
type AskResult struct {
	Ref      string `json:"ref"`
	Reviewer string `json:"reviewer"`
	// Requester is whoever clicked, copied in on the tag.
	Requester string `json:"requester,omitempty"`
	Channel   string `json:"channel"`
	TS        string `json:"ts"`
	// Tagged reports whether the thread reply landed. The card is the ask; the
	// tag is the notification, and one can succeed without the other.
	Tagged bool `json:"tagged"`
}

// String renders the result for the CLI.
func (r AskResult) String() string {
	return fmt.Sprintf("Asked <@%s> to review %s.", r.Reviewer, r.Ref)
}

// Ask posts the review-request card for ref and tags the reviewer under it.
//
// requester is whoever clicked. They are copied in on the tag, because a review
// request with no visible asker leaves the reviewer with nobody to reply to —
// and in a shared channel, no way to tell whose request it was.
//
// target supplies the credentials; the destination is this asker's own
// configuration, not the caller's — the point of the action is to reach
// somebody who is not looking at the digest it was clicked from.
func (a *Asker) Ask(ctx context.Context, ref, requester string, target slack.Target) (AskResult, error) {
	repo, number, err := SplitRef(ref)
	if err != nil {
		return AskResult{}, err
	}
	// Resolved before anything is posted: a card whose tag cannot name anybody
	// is a request nobody receives.
	reviewer, err := a.reviewerID(ctx, target)
	if err != nil {
		return AskResult{}, err
	}

	detail, err := a.gh.PullRequestDetail(ctx, repo, number)
	if err != nil {
		return AskResult{}, fmt.Errorf("reading %s: %w", ref, err)
	}

	dest := target
	dest.Channel = a.channel
	if dest.Channel == "" {
		// A DM to the reviewer, who is not necessarily the admin — so the DM
		// target is overridden rather than left to fall through to admin.
		dest.AdminUserID = reviewer
	}

	// Through the ledger, so a later approval can find this card and rewrite
	// it. Asking twice for the same pull request therefore updates the existing
	// card rather than posting a second one, which is the better answer anyway.
	card := AskCard(detail, Body(detail))
	if _, err := a.notifier.Upsert(ctx, AskKey(ref), dest, card,
		AskFallbackText(detail), AskStateOpen); err != nil {
		return AskResult{}, fmt.Errorf("asking %s for a review of %s: %w", reviewer, ref, err)
	}
	entry, found, err := a.notifier.Card(ctx, AskKey(ref))
	if err != nil {
		return AskResult{}, err
	}
	if !found || entry.TS == "" {
		return AskResult{}, fmt.Errorf("posted the review request for %s but the ledger has no record of where", ref)
	}
	posted := slack.Ref{Channel: entry.Channel, TS: entry.TS}

	result := AskResult{Ref: ref, Reviewer: reviewer, Requester: requester,
		Channel: posted.Channel, TS: posted.TS}

	// The tag goes in the card's thread, so the card reads as the subject and
	// the ask as the message about it. A failure here is reported but does not
	// undo the card: an untagged ask is still visible, and re-posting the card
	// to retry the tag would be worse.
	thread := dest
	thread.Channel = posted.Channel
	tag := AskTagText(reviewer, requester, a.prompt)
	if _, err := a.poster.Post(ctx, thread, slack.Message{
		Text: tag, Blocks: blockkit.TextBlocks(tag), ThreadTS: posted.TS,
	}); err != nil {
		return result, fmt.Errorf("posted the review request for %s but could not tag <@%s>: %w",
			ref, reviewer, err)
	}
	result.Tagged = true
	return result, nil
}

// Body is the card's body: the opening of the pull request's own description,
// translated into Slack's mrkdwn.
//
// It replaced an LLM summary, and the speed was the least of it. That call took
// ~8.6s on the click path with somebody waiting; it bound the card to a local
// `claude` binary being present and authenticated; and its output was
// non-deterministic, so the same card rendered differently on two passes and
// could not be honestly fingerprinted.
//
// A description is also not obviously improved by being summarised. The author
// already wrote it to explain the change.
//
// An empty description falls back to the title: a card with no body renders no
// section at all, which reads as though something failed.
func Body(d github.Detail) string {
	if excerpt := strings.TrimSpace(slackmd.Excerpt(d.Body, BodyParagraphs)); excerpt != "" {
		return excerpt
	}
	return slackmd.Convert(d.Title).Text
}

// AskCard renders the review request.
//
// Deliberately the legacy container shape, minus the overflow. The reviewer is
// being asked one question; a menu of alternatives is a worse way to ask it.
func AskCard(d github.Detail, summary string) blockkit.Card {
	ref := d.Ref()
	return blockkit.Card{
		Title:       d.Title,
		Subtitle:    ref,
		IconURL:     iconURL,
		IconAlt:     "GitHub",
		Body:        summary,
		BodyBlockID: "pr_summary",
		// The reference rides here, because that is the only per-card field a
		// click's payload carries back.
		ActionsBlockID: ref,
		Actions: []blockkit.Element{
			blockkit.Button{ActionID: AskActionID, Text: "Approve", Value: IntentApprove, Primary: true},
			// Named so the click is identifiable in the log. Nothing routes it:
			// Slack opens the link itself.
			blockkit.LinkButton{ActionID: AskOpenActionID, Text: "Open in Browser", URL: d.URL},
		},
	}
}

// AskSettledCard is the ask card once the pull request has been approved or
// merged.
//
// Collapsed, and with the Approve button gone. A card still offering Approve
// for a pull request that is already settled is worse than no card: it invites
// a click that can only fail. The link stays, because "where was that again"
// outlives the review.
func AskSettledCard(d github.Detail, summary, label string) blockkit.Card {
	card := AskCard(d, summary)
	card.Collapsed = true
	card.Actions = []blockkit.Element{
		blockkit.LinkButton{ActionID: AskOpenActionID, Text: "Open in Browser", URL: d.URL},
	}
	if label != "" {
		card.Context = label
	}
	return card
}

// AskFallbackText is the notification text: what Slack shows in the sidebar,
// where blocks are not rendered at all.
func AskFallbackText(d github.Detail) string {
	return fmt.Sprintf("Code review requested: %s — %s", d.Ref(), d.URL)
}

// AskTagText renders the in-thread ping. With the default prompt:
//
//	Hey <@reviewer>, mind to review this Pull Request? c/c <@requester>
//
// The prompt is the message. `{reviewer}` and `{requester}` in it are replaced
// with the corresponding mentions, so a configured wording can put them
// wherever it likes.
//
// Two things are then GUARANTEED rather than left to the wording, because they
// are the point of the feature and a config typo must not silently lose them:
//
//   - the reviewer is mentioned — prefixed if the prompt did not do it;
//   - the requester is copied in — appended as `c/c <@…>` if the prompt did
//     not do it.
//
// The c/c is dropped when there is nobody to copy, or when the requester IS the
// reviewer: copying somebody in on their own request reads as a mistake.
//
// It names no tool and claims no authorship. The ask reads as though the person
// who clicked wrote it, because they did: they chose the pull request and the
// reviewer, and they are named as the asker.
func AskTagText(reviewer, requester, prompt string) string {
	reviewerTag := fmt.Sprintf("<@%s>", reviewer)
	requesterTag := fmt.Sprintf("<@%s>", requester)

	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = config.DefaultReviewPrompt
	}
	tag := strings.NewReplacer("{reviewer}", reviewerTag, "{requester}", requesterTag).Replace(prompt)

	if !strings.Contains(tag, reviewerTag) {
		tag = reviewerTag + " " + tag
	}
	copyIn := requester != "" && requester != reviewer
	if copyIn && !strings.Contains(tag, requesterTag) {
		tag += " c/c " + requesterTag
	}
	return strings.TrimSpace(tag)
}

// RefURL renders the browser URL for owner/repo#number.
//
// Derived rather than fetched, for callers that have only a reference.
func RefURL(ref string) string {
	repo, number, err := SplitRef(ref)
	if err != nil {
		return ref
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}
