package slack

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

var reactTarget = Target{Profile: "riggs", BotToken: "xoxb-riggs"}
var reactRef = Ref{Channel: "C1", TS: "1700.1"}

// reactionFailure answers every reactions call with one application error, so
// the branching in AddReaction and RemoveReaction can be driven without a
// network.
func reactionFailure(code string) func(string, int) (int, http.Header, string) {
	return func(method string, _ int) (int, http.Header, string) {
		if strings.HasPrefix(method, "reactions.") {
			return 200, nil, `{"ok":false,"error":"` + code + `"}`
		}
		return 200, nil, `{"ok":true}`
	}
}

// `reactions.add` takes the bare name. Everywhere a human writes an emoji they
// write the colons, so the colons are absorbed rather than rejected with a
// message about a syntax nobody sees.
func TestEmojiNamesAreNormalised(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{":tada:", "tada"},
		{"tada", "tada"},
		{"  :tada:  ", "tada"},
		{":+1::skin-tone-3:", "+1::skin-tone-3"},
		{"", ""},
		{"::", ""},
	} {
		if got := NormaliseEmojiName(tc.in); got != tc.want {
			t.Errorf("NormaliseEmojiName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The colons are stripped on the way OUT too, not only when the setting is
// read: a name that reached Slack with them would be rejected as invalid, on
// every click, forever.
func TestAddSendsTheBareNameAndTheMessage(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	if err := api.AddReaction(context.Background(), reactTarget, reactRef, ":tada:"); err != nil {
		t.Fatalf("AddReaction: %v", err)
	}
	if len(log) != 1 || log[0].method != "reactions.add" {
		t.Fatalf("calls = %+v, want one reactions.add", log)
	}
	// `timestamp`, not `ts`: the reactions methods are the one corner of the
	// Web API that spells a message id differently from chat.update, and the
	// symptom of getting it wrong is `bad_timestamp` on every click.
	want := map[string]any{"channel": "C1", "timestamp": "1700.1", "name": "tada"}
	for k, v := range want {
		if log[0].body[k] != v {
			t.Errorf("body[%s] = %v, want %v (body = %v)", k, log[0].body[k], v, log[0].body)
		}
	}
}

// An empty name would be a guaranteed Slack error on every click, with a
// message naming no setting. It is refused here instead.
func TestAnEmptyNameIsRefusedLocally(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	if err := api.AddReaction(context.Background(), reactTarget, reactRef, " : : "); err == nil {
		t.Error("an empty name was sent to Slack")
	}
	if err := api.RemoveReaction(context.Background(), reactTarget, reactRef, ""); err == nil {
		t.Error("an empty name was sent to Slack")
	}
	if len(log) != 0 {
		t.Fatalf("%d HTTP calls made: %+v", len(log), log)
	}
}

// The caller's intent is "this emoji is on that message", and it is. Failing
// would make a retried transition report a problem it had already solved.
func TestAlreadyReactedIsSuccess(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, reactionFailure("already_reacted"))
	defer stop()

	if err := api.AddReaction(context.Background(), reactTarget, reactRef, "tada"); err != nil {
		t.Fatalf("AddReaction: %v", err)
	}
}

// The mirror: the caller wants the emoji gone, and it is. That is what makes
// the blind removal in internal/comms safe to run against a message somebody
// has already tidied by hand, and safe to run twice.
func TestNoReactionIsSuccess(t *testing.T) {
	for _, code := range []string{"no_reaction", "message_not_found"} {
		var log []recorded
		api, stop := server(t, &log, reactionFailure(code))
		if err := api.RemoveReaction(context.Background(), reactTarget, reactRef, "tada"); err != nil {
			t.Errorf("RemoveReaction with %s: %v", code, err)
		}
		stop()
	}
}

// An emoji the workspace does not have is the admin's to fix and nobody else's,
// so it arrives typed rather than buried in an error string.
func TestAnUnknownEmojiIsTyped(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, reactionFailure("invalid_name"))
	defer stop()

	err := api.AddReaction(context.Background(), reactTarget, reactRef, "no_such_emoji")
	if !errors.Is(err, ErrInvalidEmoji) {
		t.Fatalf("err = %v, want ErrInvalidEmoji", err)
	}
}

// Anything else is a real failure and must reach the caller, or an app missing
// the scope would look exactly like a message that was already tidy.
func TestOtherReactionFailuresAreReported(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, reactionFailure("missing_scope"))
	defer stop()

	err := api.AddReaction(context.Background(), reactTarget, reactRef, "tada")
	if err == nil || !strings.Contains(err.Error(), "missing_scope") {
		t.Fatalf("err = %v, want the scope failure to surface", err)
	}
}
