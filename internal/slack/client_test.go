package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// recorded is one request the fake Slack server saw.
type recorded struct {
	method      string
	auth        string
	contentType string
	body        map[string]any
}

// server spins up a fake Slack Web API. handler returns the JSON body for each
// method in turn; the recorded requests are appended to *log.
//
// The body is decoded according to the Content-Type the client declared, and a
// request this fake cannot parse fails the test. Real Slack reads one encoding
// per method and answers `invalid_arguments` for the other; a fake that
// switches on the method name alone accepts both, which is how a malformed
// call once shipped green.
func server(t *testing.T, log *[]recorded, handler func(method string, n int) (int, http.Header, string)) (*API, func()) {
	t.Helper()
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		raw, _ := io.ReadAll(r.Body)
		ctype := r.Header.Get("Content-Type")

		body := map[string]any{}
		switch {
		case strings.HasPrefix(ctype, "application/json"):
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("%s: declared JSON but sent %q: %v", method, raw, err)
			}
		case strings.HasPrefix(ctype, "application/x-www-form-urlencoded"):
			form, err := url.ParseQuery(string(raw))
			if err != nil {
				t.Errorf("%s: declared a form but sent %q: %v", method, raw, err)
			}
			for k := range form {
				body[k] = form.Get(k)
			}
		default:
			t.Errorf("%s: unsupported Content-Type %q", method, ctype)
		}
		// A body with content in it that yields no parameters is the failure
		// this fake exists to catch: Slack would see an argument-less call and
		// answer `invalid_arguments`. A genuinely empty payload — auth.test
		// takes none — is not that.
		if trimmed := strings.TrimSpace(string(raw)); len(body) == 0 && trimmed != "" && trimmed != "{}" {
			t.Errorf("%s: sent %q, which decoded to no parameters", method, raw)
		}
		*log = append(*log, recorded{
			method: method, auth: r.Header.Get("Authorization"),
			contentType: ctype, body: body,
		})

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

// A failed click is reported to the person who made it and nobody else, which
// is what chat.postEphemeral is for: the same message as a real post, plus the
// user it is visible to.
func TestPostEphemeralTargetsOneUser(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	err := api.PostEphemeral(context.Background(),
		Target{Profile: "default", BotToken: "xoxb-t", Channel: "C123"}, "U42",
		Message{Text: "that did not work", ThreadTS: "1700.0001"})
	if err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	if len(log) != 1 || log[0].method != "chat.postEphemeral" {
		t.Fatalf("calls = %+v, want a single chat.postEphemeral", log)
	}
	if log[0].body["user"] != "U42" {
		t.Errorf("user = %v, want the clicker", log[0].body["user"])
	}
	if log[0].body["channel"] != "C123" || log[0].body["thread_ts"] != "1700.0001" {
		t.Errorf("body = %+v, want it threaded under the message clicked", log[0].body)
	}
}

// The Home tab has no channel, so a failure there is reported by DM — which
// means the ephemeral path has to resolve a DM target like any other.
func TestPostEphemeralToDMOpensConversation(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, okJSON)
	defer stop()

	err := api.PostEphemeral(context.Background(),
		Target{Profile: "default", BotToken: "xoxb-t", AdminUserID: "U42"}, "U42",
		Message{Text: "that did not work"})
	if err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	if len(log) != 2 || log[0].method != "conversations.open" {
		t.Fatalf("calls = %+v, want the conversation opened first", log)
	}
	if log[1].body["channel"] != "D999" {
		t.Errorf("channel = %v, want the opened DM", log[1].body["channel"])
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

// TestReadMethodsAreFormEncoded pins the encoding split.
//
// `conversations.replies` and `users.list` do not read a JSON body: Slack
// ignores it and answers `invalid_arguments`, naming parameters that were in
// the payload. This asserts on the wire format rather than on the method name,
// because the method name is what the old fake matched on while the real call
// had been failing for days.
func TestReadMethodsAreFormEncoded(t *testing.T) {
	for _, method := range []string{"conversations.replies", "users.list"} {
		if !formEncodedMethods[method] {
			t.Errorf("%s must be form-encoded", method)
		}
	}

	payload, ctype, err := encodeRequest("conversations.replies", map[string]any{
		"channel": "C123", "ts": "1700.0001", "limit": 50,
	})
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	if !strings.HasPrefix(ctype, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want form-urlencoded", ctype)
	}
	form, err := url.ParseQuery(string(payload))
	if err != nil {
		t.Fatalf("parsing %q: %v", payload, err)
	}
	for k, want := range map[string]string{"channel": "C123", "ts": "1700.0001", "limit": "50"} {
		if got := form.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// TestWriteMethodsStayJSON keeps the blocks payload on the encoding it needs.
func TestWriteMethodsStayJSON(t *testing.T) {
	payload, ctype, err := encodeRequest("chat.postMessage", map[string]any{
		"channel": "C123", "blocks": []any{map[string]string{"type": "divider"}},
	})
	if err != nil {
		t.Fatalf("encodeRequest: %v", err)
	}
	if !strings.HasPrefix(ctype, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ctype)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decoding %q: %v", payload, err)
	}
	if got["blocks"] == nil {
		t.Error("blocks did not survive encoding")
	}
}

// TestFormValueRejectsStructuredParameters keeps a map out of a form parameter.
//
// Stringifying one would produce a value Slack ignores, which is the silent
// half of the failure this change fixes.
func TestFormValueRejectsStructuredParameters(t *testing.T) {
	if _, _, err := encodeRequest("users.list", map[string]any{
		"blocks": []any{"nope"},
	}); err == nil {
		t.Error("want an error for a structured form parameter, got none")
	}
}

// TestReplyThreadReadReachesSlack drives the real read path through the fake,
// which now rejects a body it cannot decode.
func TestReplyThreadReadReachesSlack(t *testing.T) {
	var log []recorded
	api, stop := server(t, &log, replies(`{"ts":"1700.0002","user":"UHUMAN"}`))
	defer stop()

	got, err := api.HasForeignReplies(context.Background(), threadTarget, parentRef)
	if err != nil {
		t.Fatalf("HasForeignReplies: %v", err)
	}
	if !got {
		t.Error("got false for a thread holding a colleague's reply")
	}
	for _, r := range log {
		if r.method != "conversations.replies" {
			continue
		}
		if !strings.HasPrefix(r.contentType, "application/x-www-form-urlencoded") {
			t.Errorf("Content-Type = %q, want form-urlencoded", r.contentType)
		}
		if r.body["channel"] != "C123" || r.body["ts"] != "1700.0001" {
			t.Errorf("Slack saw %+v, want the channel and ts", r.body)
		}
	}
}
