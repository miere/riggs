package ticket

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/riggs-mcp/internal/ask"
	"github.com/miere/riggs-mcp/internal/blockkit"
	"github.com/miere/riggs-mcp/internal/config"
	"github.com/miere/riggs-mcp/internal/jira"
	"github.com/miere/riggs-mcp/internal/notify"
	"github.com/miere/riggs-mcp/internal/slack"
	"github.com/miere/riggs-mcp/internal/slackmd"
)

// "Ask for SME Assistance": hand one ticket to a person to scope.
//
// The same shape as the pull-request queue's "Ask for Code Review", and for the
// same reasons: the ask is a CARD about the ticket, with the tag in its thread,
// so the card reads as the subject and the tag as the message about it.
//
// It differs in one thing, which is the whole point of the action. A review
// request asks somebody to look at code that exists; this asks somebody to look
// at work that does not exist yet and say whether it is ready to be picked up.
// So the card carries no verb at all — only the link. Nothing is delegated, no
// agent is started, and Jira is not touched.
//
// It was called "Ask for AI Assistance" until this phase, which was the one
// thing it never did. Everyone who read the label expected an agent to pick the
// ticket up; what actually happened was that a colleague got tagged. Running an
// agent is now a separate option on the same menu (internal/ai), and this one
// says who it reaches.

const (
	// AskActionID names the ask card's link button. Nothing routes it — Slack
	// opens the URL itself — but it is named so the interaction it nevertheless
	// raises is identifiable in the daemon's log (§7b).
	AskActionID = "jira_ask_open"
	// AskKeyPrefix namespaces the ask cards in the ledger, so asking twice about
	// one ticket updates the card rather than posting a second.
	AskKeyPrefix = "jira.tickets.ask:"
	// AskState is recorded on the ledger entry. There is one: unlike a review
	// request, nothing here later settles.
	AskState = "asked"
)

// AskKey is the ledger key for a ticket's assistance-request card.
func AskKey(issueKey string) string { return AskKeyPrefix + issueKey }

// Resolver turns a configured handle into a Slack user id. *slack.API satisfies
// it.
type Resolver interface {
	LookupUserID(ctx context.Context, target slack.Target, handle string) (string, error)
}

// BodyParagraphs is how much of a ticket's description a card shows.
//
// Two: enough for the reporter's own account of what they want, and short
// enough that the card stays a card. Anything past it is on the ticket, one
// click away — the same reasoning, and the same number, as the pull-request
// card's.
const BodyParagraphs = 2

// Asker posts an assistance request.
type Asker struct {
	jira     Source
	notifier *notify.Notifier
	poster   slack.Poster

	// resolver turns a configured handle into an id, when the config holds one.
	resolver Resolver
	// user is the configured recipient, as written: an id, a mention, or a
	// handle. It is normalised at send time, never assumed.
	user string
	// channel is where the card is posted. Empty DMs the recipient.
	channel string
	// prompt is the wording of the tag.
	prompt string
}

// NewAsker builds the asker.
func NewAsker(src Source, n *notify.Notifier, poster slack.Poster,
	user, channel, prompt string) *Asker {
	return &Asker{jira: src, notifier: n, poster: poster,
		user: user, channel: channel, prompt: prompt}
}

// WithResolver supplies the handle lookup. Without one, a configured handle is
// an error rather than a mention that reaches nobody.
func (a *Asker) WithResolver(r Resolver) *Asker {
	a.resolver = r
	return a
}

// userID normalises the configured recipient and resolves a handle if that is
// what it is.
func (a *Asker) userID(ctx context.Context, target slack.Target) (string, error) {
	ref := slack.ParseUserRef(a.user)
	switch {
	case ref.IsID():
		return ref.ID, nil
	case ref.Handle == "":
		return "", fmt.Errorf("cannot ask for assistance: nobody configured (set sme-assistance.user-id)")
	case a.resolver == nil:
		return "", fmt.Errorf("sme-assistance.user-id is %q, which is a handle rather than a Slack id, and nothing is wired to resolve it", a.user)
	}
	id, err := a.resolver.LookupUserID(ctx, target, ref.Handle)
	if err != nil {
		return "", fmt.Errorf("sme-assistance.user-id %q: %w", a.user, err)
	}
	return id, nil
}

// AskResult reports where the ask went.
type AskResult struct {
	Key  string `json:"key"`
	User string `json:"user"`
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
	return fmt.Sprintf("Asked <@%s> to look at %s.", r.User, r.Key)
}

// Ask posts the assistance-request card for issueKey and tags the recipient
// under it.
//
// requester is whoever clicked. They are copied in on the tag, because a
// request with no visible asker leaves the recipient with nobody to reply to —
// and in a shared channel, no way to tell whose request it was.
//
// target supplies the credentials; the destination is this asker's own
// configuration, not the caller's. The point of the action is to reach somebody
// who is not looking at the digest it was clicked from.
func (a *Asker) Ask(ctx context.Context, issueKey, requester string, target slack.Target) (AskResult, error) {
	issueKey = TicketKeyFromBlockID(issueKey)
	if issueKey == "" {
		return AskResult{}, fmt.Errorf("no ticket to ask about")
	}
	// Resolved before anything is posted: a card whose tag cannot name anybody
	// is a request nobody receives.
	user, err := a.userID(ctx, target)
	if err != nil {
		return AskResult{}, err
	}

	issue, err := a.jira.Get(ctx, issueKey)
	if err != nil {
		return AskResult{}, fmt.Errorf("reading %s: %w", issueKey, err)
	}

	dest := target
	dest.Channel = a.channel
	if dest.Channel == "" {
		// A DM to the recipient, who is not necessarily the admin — so the DM
		// target is overridden rather than left to fall through to admin.
		dest.AdminUserID = user
	}

	card := AskCard(issue, Body(issue), a.jira.BrowseURL(issueKey))
	if _, err := a.notifier.Upsert(ctx, AskKey(issueKey), dest, card,
		AskFallbackText(issue, a.jira.BrowseURL(issueKey)), AskState); err != nil {
		return AskResult{}, fmt.Errorf("asking %s to look at %s: %w", user, issueKey, err)
	}
	entry, found, err := a.notifier.Card(ctx, AskKey(issueKey))
	if err != nil {
		return AskResult{}, err
	}
	if !found || entry.TS == "" {
		return AskResult{}, fmt.Errorf("posted the assistance request for %s but the ledger has no record of where", issueKey)
	}

	result := AskResult{Key: issueKey, User: user, Requester: requester,
		Channel: entry.Channel, TS: entry.TS}

	// The tag goes in the card's thread. A failure here is reported but does not
	// undo the card: an untagged ask is still visible, and re-posting the card to
	// retry the tag would be worse.
	thread := dest
	thread.Channel = entry.Channel
	tag := AskTagText(user, requester, a.prompt)
	if _, err := a.poster.Post(ctx, thread, slack.Message{
		Text: tag, Blocks: blockkit.TextBlocks(tag), ThreadTS: entry.TS,
	}); err != nil {
		return result, fmt.Errorf("posted the assistance request for %s but could not tag <@%s>: %w",
			issueKey, user, err)
	}
	result.Tagged = true
	return result, nil
}

// Body is a card's body: the opening of the ticket's own description,
// translated into Slack's mrkdwn.
//
// It replaced an LLM summary for the same three reasons the pull-request card
// did (§7d) — the seconds it cost on a per-ticket loop, the dependency on a
// local `claude` binary, and output that changed between renders and so could
// not be honestly fingerprinted. A reporter's description is not obviously
// improved by being paraphrased.
//
// An empty description falls back to the summary: a card with no body renders
// no section at all, which reads as though something failed.
func Body(issue jira.Issue) string {
	if body := strings.TrimSpace(slackmd.Excerpt(issue.Description, BodyParagraphs)); body != "" {
		return body
	}
	return slackmd.Convert(issue.Summary).Text
}

// AskCard renders the assistance request.
//
// The legacy container shape with no verb on it: a link out, and nothing else.
// Assign is not offered here for the same reason it is not offered on a digest
// row — it is specified but not for this pass — and adding it later means
// adding a button and a route, not reshaping the card.
func AskCard(issue jira.Issue, summary, url string) blockkit.Card {
	return blockkit.Card{
		Title:       issue.Summary,
		Subtitle:    issue.Key,
		IconURL:     iconURL,
		IconAlt:     "Jira",
		Body:        summary,
		BodyBlockID: "ticket_description",
		// The key rides here, because that is the only per-card field a click's
		// payload carries back.
		ActionsBlockID: ActionsBlockID(issue.Key),
		Actions: []blockkit.Element{
			blockkit.LinkButton{ActionID: AskActionID, Text: "Open in Browser", URL: url},
		},
	}
}

// AskFallbackText is the notification text: what Slack shows in the sidebar,
// where blocks are not rendered at all.
func AskFallbackText(issue jira.Issue, url string) string {
	return fmt.Sprintf("Assistance requested: %s — %s", issue.Key, url)
}

// AskTagText renders the in-thread ping. With the default prompt:
//
//	Hey <@user>, mind to take a look at the scope of this ticket? c/c <@requester>
//
// The wording is configuration; the two mentions in it are not. See
// internal/ask, which is shared with the pull-request queue's equivalent action
// so the guarantee cannot drift between them.
func AskTagText(user, requester, prompt string) string {
	if prompt == "" {
		prompt = config.DefaultSMEPrompt
	}
	return ask.TagText(user, requester, prompt)
}
