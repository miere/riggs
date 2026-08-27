package ask

import (
	"strings"
	"testing"
)

// The two mentions are the point of the feature. A wording that drops one must
// not silently produce a message that reaches nobody.
func TestTagTextGuaranteesBothMentions(t *testing.T) {
	got := TagText("U-them", "U-me", "could someone look at this")
	if want := "<@U-them> could someone look at this c/c <@U-me>"; got != want {
		t.Fatalf("TagText = %q, want %q", got, want)
	}
}

// A wording that places them itself is left alone — no duplicates.
func TestTagTextHonoursPlacement(t *testing.T) {
	got := TagText("U-them", "U-me", "{requester} would like {user} to take a look")
	if want := "<@U-me> would like <@U-them> to take a look"; got != want {
		t.Fatalf("TagText = %q, want %q", got, want)
	}
}

// {reviewer} is the pull-request queue's original spelling and still lives in
// configured prompts, so it stays an alias for {user}.
func TestTagTextAcceptsTheReviewerAlias(t *testing.T) {
	got := TagText("U-them", "U-me", "Hey {reviewer}, mind to review this Pull Request?")
	if want := "Hey <@U-them>, mind to review this Pull Request? c/c <@U-me>"; got != want {
		t.Fatalf("TagText = %q, want %q", got, want)
	}
}

// Copying somebody in on their own request reads as a mistake.
func TestTagTextDropsTheSelfCopy(t *testing.T) {
	got := TagText("U-me", "U-me", "have a look")
	if want := "<@U-me> have a look"; got != want {
		t.Fatalf("TagText = %q, want %q", got, want)
	}
}

// A click Slack could not attribute still produces a usable ask.
func TestTagTextWithNoRequester(t *testing.T) {
	got := TagText("U-them", "", "have a look")
	if want := "<@U-them> have a look"; got != want {
		t.Fatalf("TagText = %q, want %q", got, want)
	}
}

// A prompt that places {requester} inline must never render an empty mention:
// Slack shows `<@>` as exactly that.
func TestTagTextNeverRendersAnEmptyMention(t *testing.T) {
	got := TagText("U-them", "", "{user}, {requester} needs your help")
	if want := "<@U-them>, somebody needs your help"; got != want {
		t.Fatalf("TagText = %q, want %q", got, want)
	}
	if strings.Contains(got, "<@>") {
		t.Fatalf("TagText = %q, want no empty mention", got)
	}
}
