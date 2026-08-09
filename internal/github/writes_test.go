package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captured is one request the fake GitHub saw.
type captured struct {
	method      string
	path        string
	body        map[string]any
	ifNoneMatch string
}

func writeServer(t *testing.T, log *[]captured, status int, response string) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*log = append(*log, captured{
			method: r.Method, path: r.URL.Path, body: body,
			ifNoneMatch: r.Header.Get("If-None-Match"),
		})
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	return New("t").WithTransport(srv.Client(), srv.URL), srv.Close
}

func TestApprovePostsAReview(t *testing.T) {
	var log []captured
	c, stop := writeServer(t, &log, 200, `{}`)
	defer stop()

	if err := c.Approve(context.Background(), "o/r", 7, "Approved via Riggs."); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("requests = %+v, want one", log)
	}
	got := log[0]
	if got.method != http.MethodPost || got.path != "/repos/o/r/pulls/7/reviews" {
		t.Errorf("request = %s %s", got.method, got.path)
	}
	if got.body["event"] != "APPROVE" {
		t.Errorf("event = %v, want APPROVE", got.body["event"])
	}
	if got.body["body"] != "Approved via Riggs." {
		t.Errorf("body = %v, want the review comment", got.body["body"])
	}
}

// Our repositories do not allow squash and have no auto-merge. This is a hard
// constraint, so it is asserted rather than assumed.
func TestMergeIsAlwaysRebase(t *testing.T) {
	var log []captured
	c, stop := writeServer(t, &log, 200, `{"merged":true}`)
	defer stop()

	if err := c.Merge(context.Background(), "o/r", 7); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := log[0]
	if got.method != http.MethodPut || got.path != "/repos/o/r/pulls/7/merge" {
		t.Errorf("request = %s %s", got.method, got.path)
	}
	if got.body["merge_method"] != MergeMethodRebase {
		t.Errorf("merge_method = %v, want %q", got.body["merge_method"], MergeMethodRebase)
	}
	// Belt and braces: neither forbidden mode may appear anywhere.
	encoded, _ := json.Marshal(got.body)
	for _, forbidden := range []string{"squash", "auto"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("merge payload mentions %q: %s", forbidden, encoded)
		}
	}
}

// A merge conflict must reach Slack as GitHub's own words, not "HTTP 405".
func TestWriteErrorCarriesGitHubsMessage(t *testing.T) {
	var log []captured
	c, stop := writeServer(t, &log, 405, `{"message":"Base branch was modified. Review and try the merge again."}`)
	defer stop()

	err := c.Merge(context.Background(), "o/r", 7)
	if err == nil {
		t.Fatal("Merge = nil error on a 405")
	}
	if !strings.Contains(err.Error(), "Base branch was modified") {
		t.Errorf("err = %q, want GitHub's explanation", err)
	}
}

func TestWriteErrorIncludesNestedDetail(t *testing.T) {
	var log []captured
	c, stop := writeServer(t, &log, 422,
		`{"message":"Validation Failed","errors":[{"message":"Can not approve your own pull request"}]}`)
	defer stop()

	err := c.Approve(context.Background(), "o/r", 7, "")
	if err == nil || !strings.Contains(err.Error(), "Can not approve your own pull request") {
		t.Fatalf("err = %v, want the nested reason surfaced", err)
	}
}

// Mutations are never conditional: an If-None-Match on a write would be
// meaningless, and a 304 unanswerable.
func TestWritesAreNotConditional(t *testing.T) {
	var log []captured
	c, stop := writeServer(t, &log, 200, `{}`)
	defer stop()
	cache := newMemCache()
	cache.etags[c.baseURL+"/repos/o/r/pulls/7/reviews"] = `W/"abc"`
	c.WithCache(cache, time.Now)

	if err := c.Approve(context.Background(), "o/r", 7, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if log[0].ifNoneMatch != "" {
		t.Errorf("write sent If-None-Match %q", log[0].ifNoneMatch)
	}
	if cache.saves != 0 {
		t.Errorf("a write populated the response cache (%d saves)", cache.saves)
	}
}

// A write is never retried: a retried approval could approve twice, and a
// retried merge could act on something already merged.
func TestWritesAreNotRetried(t *testing.T) {
	var log []captured
	c, stop := writeServer(t, &log, 500, `{"message":"server error"}`)
	defer stop()

	if err := c.Approve(context.Background(), "o/r", 7, ""); err == nil {
		t.Fatal("Approve = nil error on a 500")
	}
	if len(log) != 1 {
		t.Errorf("attempts = %d, want exactly one — writes must not be retried", len(log))
	}
}

func TestAuthenticatedLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"login":"miere"}`)
	}))
	defer srv.Close()

	login, err := New("t").WithTransport(srv.Client(), srv.URL).
		AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatalf("AuthenticatedLogin: %v", err)
	}
	if login != "miere" {
		t.Errorf("login = %q", login)
	}
}

func TestAuthenticatedLoginRejectsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	if _, err := New("t").WithTransport(srv.Client(), srv.URL).
		AuthenticatedLogin(context.Background()); err == nil {
		t.Fatal("AuthenticatedLogin = nil error for a response with no login")
	}
}
