package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorded is one request the fake Slack server saw.
type recorded struct {
	method string
	auth   string
	body   map[string]any
}

// server spins up a fake Slack Web API. handler returns the JSON body for each
// method in turn; the recorded requests are appended to *log.
func server(t *testing.T, log *[]recorded, handler func(method string, n int) (int, http.Header, string)) (*API, func()) {
	t.Helper()
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*log = append(*log, recorded{method: method, auth: r.Header.Get("Authorization"), body: body})

		status, hdr, payload := handler(method, counts[method])
		counts[method]++
		for k, vs := range hdr {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		io.WriteString(w, payload)
	}))
	api := NewAPI().WithTransport(srv.Client(), srv.URL, func(time.Duration) {})
	return api, srv.Close
}

func okJSON(method string, _ int) (int, http.Header, string) {
	switch method {
	case "chat.postMessage":
		return 200, nil, `{"ok":true,"channel":"C123","ts":"1700.0001"}`
	case "chat.update":
		return 200, nil, `{"ok":true,"channel":"C123","ts":"1700.0001"}`
	case "conversations.open":
		return 200, nil, `{"ok":true,"channel":{"id":"D999"}}`
	}
	return 200, nil, `{"ok":true}`
}

func TestPostToChannel(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	ref, err := api.Post(context.Background(),
		Target{Profile: "default", BotToken: "xoxb-t", Channel: "C123"},
		Message{Text: "fallback", Blocks: []any{map[string]string{"type": "divider"}}})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if ref.Channel != "C123" || ref.TS != "1700.0001" {
		t.Errorf("ref = %+v, want the posted channel and ts", ref)
	}
	if len(log) != 1 || log[0].method != "chat.postMessage" {
		t.Fatalf("calls = %+v, want a single chat.postMessage", log)
	}
	if log[0].auth != "Bearer xoxb-t" {
		t.Errorf("auth = %q, want the profile's bot token", log[0].auth)
	}
	if log[0].body["text"] != "fallback" {
		t.Errorf("text = %v, want the fallback text sent", log[0].body["text"])
	}
	if _, ok := log[0].body["thread_ts"]; ok {
		t.Error("thread_ts sent on a top-level post")
	}
}

// A DM target has no channel, so the client opens the conversation first.
func TestPostToDMOpensConversation(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	_, err := api.Post(context.Background(),
		Target{Profile: "default", BotToken: "xoxb-t", AdminUserID: "U1"},
		Message{Text: "hi"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(log) != 2 {
		t.Fatalf("calls = %d, want conversations.open then chat.postMessage", len(log))
	}
	if log[0].method != "conversations.open" || log[0].body["users"] != "U1" {
		t.Errorf("first call = %+v, want conversations.open for U1", log[0])
	}
	if log[1].body["channel"] != "D999" {
		t.Errorf("posted to %v, want the opened DM channel", log[1].body["channel"])
	}
}

func TestPostThreaded(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	if _, err := api.Post(context.Background(),
		Target{BotToken: "t", Channel: "C1"},
		Message{Text: "reply", ThreadTS: "1699.5"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if log[0].body["thread_ts"] != "1699.5" {
		t.Errorf("thread_ts = %v, want the parent ts", log[0].body["thread_ts"])
	}
}

// Slack reports application errors with HTTP 200 and ok:false, so the status
// code alone never tells you whether a post succeeded.
func TestApplicationErrorDespiteHTTP200(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, func(string, int) (int, http.Header, string) {
		return 200, nil, `{"ok":false,"error":"channel_not_found"}`
	})
	defer stop()

	_, err := api.Post(context.Background(), Target{BotToken: "t", Channel: "C1"}, Message{Text: "x"})
	if err == nil {
		t.Fatal("Post = nil error on ok:false")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("error = %q, want Slack's own error code", err)
	}
}

// A vanished message is not a failure — it is the ledger's cue to re-post.
func TestUpdateMessageNotFoundIsTyped(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, func(string, int) (int, http.Header, string) {
		return 200, nil, `{"ok":false,"error":"message_not_found"}`
	})
	defer stop()

	err := api.Update(context.Background(), Target{BotToken: "t"},
		Ref{Channel: "C1", TS: "1"}, Message{Text: "x"})
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("err = %v, want ErrMessageNotFound", err)
	}
}

// A per-minute job must survive Slack's rate limiter rather than dropping the
// tick.
func TestRetriesOnRateLimit(t *testing.T) {
	var log []recorded
	var slept []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		log = append(log, recorded{method: strings.TrimPrefix(r.URL.Path, "/"), body: body})
		if len(log) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"ok":true,"channel":"C1","ts":"1700.1"}`)
	}))
	defer srv.Close()

	api := NewAPI().WithTransport(srv.Client(), srv.URL, func(d time.Duration) { slept = append(slept, d) })
	ref, err := api.Post(context.Background(), Target{BotToken: "t", Channel: "C1"}, Message{Text: "x"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if ref.TS != "1700.1" {
		t.Errorf("ref = %+v, want the retry's result", ref)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept = %v, want Slack's Retry-After honoured", slept)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, func(string, int) (int, http.Header, string) {
		return 500, nil, ``
	})
	defer stop()

	_, err := api.Post(context.Background(), Target{BotToken: "t", Channel: "C1"}, Message{Text: "x"})
	if err == nil {
		t.Fatal("Post = nil error against a server that never recovers")
	}
	if len(log) != 3 {
		t.Errorf("attempts = %d, want 3", len(log))
	}
}

// Blocks are omitted entirely when absent, rather than sent as null.
func TestOmitsBlocksWhenUnset(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	if _, err := api.Post(context.Background(), Target{BotToken: "t", Channel: "C1"},
		Message{Text: "plain"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, present := log[0].body["blocks"]; present {
		t.Error("blocks key sent for a message with no blocks")
	}
}
