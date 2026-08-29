package slack

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

var threadTarget = Target{Profile: "default", BotToken: "xoxb-t", Channel: "C123"}

var parentRef = Ref{Channel: "C123", TS: "1700.0001"}

// replies builds a fake Slack that identifies us as UBOT and returns the given
// thread messages, parent included.
func replies(msgs ...string) func(string, int) (int, http.Header, string) {
	return func(method string, _ int) (int, http.Header, string) {
		switch method {
		case "auth.test":
			return 200, nil, `{"ok":true,"user_id":"UBOT"}`
		case "conversations.replies":
			body := `{"ts":"1700.0001","user":"UBOT"}` // the parent, posted by us
			for _, m := range msgs {
				body += "," + m
			}
			return 200, nil, fmt.Sprintf(`{"ok":true,"messages":[%s]}`, body)
		}
		return 200, nil, `{"ok":true}`
	}
}

func TestNoRepliesMeansNoConversationToProtect(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, replies())
	defer stop()

	got, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef)
	if err != nil {
		t.Fatalf("HasForeignReplies: %v", err)
	}
	if got {
		t.Error("got true for a thread holding only the card itself")
	}
}

// The point of the whole check. Riggs narrates approvals and reports failed
// clicks into a digest's own thread, so its own replies must not count — every
// digest that ever saw a click would otherwise be kept forever.
func TestOurOwnRepliesDoNotCount(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log,
		replies(`{"ts":"1700.0002","user":"UBOT","bot_id":"B1"}`))
	defer stop()

	got, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef)
	if err != nil {
		t.Fatalf("HasForeignReplies: %v", err)
	}
	if got {
		t.Error("got true for a thread holding only Riggs' own narration")
	}
}

func TestAColleaguesReplyCounts(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log,
		replies(`{"ts":"1700.0002","user":"UBOT","bot_id":"B1"}`,
			`{"ts":"1700.0003","user":"UHUMAN"}`))
	defer stop()

	got, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef)
	if err != nil {
		t.Fatalf("HasForeignReplies: %v", err)
	}
	if !got {
		t.Error("got false, want a human reply to protect the message")
	}
}

// A thread that is already gone has no conversation to lose.
func TestAVanishedThreadHasNoRepliesToProtect(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, func(method string, _ int) (int, http.Header, string) {
		if method == "auth.test" {
			return 200, nil, `{"ok":true,"user_id":"UBOT"}`
		}
		return 200, nil, `{"ok":false,"error":"thread_not_found"}`
	})
	defer stop()

	got, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef)
	if err != nil {
		t.Fatalf("HasForeignReplies: %v", err)
	}
	if got {
		t.Error("got true for a thread Slack says is not there")
	}
}

// Any other failure is reported, never swallowed as "no replies" — that would
// turn a missing scope into silent deletion of live conversations.
func TestAnUnreadableThreadIsAnError(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, func(method string, _ int) (int, http.Header, string) {
		if method == "auth.test" {
			return 200, nil, `{"ok":true,"user_id":"UBOT"}`
		}
		return 200, nil, `{"ok":false,"error":"missing_scope"}`
	})
	defer stop()

	if _, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef); err == nil {
		t.Fatal("HasForeignReplies = nil error, want the failure surfaced")
	}
}

// Our identity is fixed for a token, so it is looked up once however many
// threads are checked.
func TestSelfIsResolvedOnce(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, replies())
	defer stop()

	for i := 0; i < 3; i++ {
		if _, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef); err != nil {
			t.Fatalf("HasForeignReplies: %v", err)
		}
	}
	auths := 0
	for _, r := range log {
		if r.method == "auth.test" {
			auths++
		}
	}
	if auths != 1 {
		t.Errorf("auth.test called %d times, want it cached after the first", auths)
	}
}
