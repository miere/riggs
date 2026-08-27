package jira

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

type seen struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

func server(t *testing.T, log *[]seen, status int, response string) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*log = append(*log, seen{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), body})
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	return New(srv.URL, "m@x", "tok").WithTransport(srv.Client(), srv.URL), srv.Close
}

// Missing credentials disable the feature with a clear message rather than
// producing a confusing 401 at the API.
func TestUnconfiguredIsTyped(t *testing.T) {
	_, err := New("https://example.atlassian.net", "", "").Search(context.Background(), "project = NYX", 1)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "ATLASSIAN_JIRA_EMAIL") {
		t.Errorf("err = %q, want it to name the settings that would fix it", err)
	}
}

// There is no default tenant. A client with credentials but nowhere to point
// them must refuse rather than reach for a baked-in Atlassian instance — a
// misconfigured machine reading somebody else's Jira is worse than one that
// does not read any.
func TestNoTenantIsTyped(t *testing.T) {
	_, err := New("", "m@x", "tok").Search(context.Background(), "project = NYX", 1)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	for _, want := range []string{"base-url", "no default tenant"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to mention %q", err, want)
		}
	}
}

func TestSearchSendsTheJQL(t *testing.T) {
	var log []seen
	c, stop := server(t, &log, 200, `{"issues":[{"key":"NYX-1","fields":{
		"summary":"Add a health probe","status":{"name":"Ready"},
		"reporter":{"displayName":"Annie"},"updated":"2026-08-09T10:11:12.000+1000",
		"description":{"type":"doc","content":[
			{"type":"paragraph","content":[{"type":"text","text":"Do the thing."}]}]}}}]}`)
	defer stop()

	issues, err := c.Search(context.Background(), `project = NYX AND labels = "ai-able"`, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if log[0].method != http.MethodPost || !strings.HasSuffix(log[0].path, "/rest/api/3/search/jql") {
		t.Errorf("request = %s %s", log[0].method, log[0].path)
	}
	if log[0].body["jql"] != `project = NYX AND labels = "ai-able"` {
		t.Errorf("jql = %v, want the caller's query passed through", log[0].body["jql"])
	}
	if !strings.HasPrefix(log[0].auth, "Basic ") {
		t.Errorf("auth = %q, want Basic", log[0].auth)
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues", len(issues))
	}
	i := issues[0]
	if i.Key != "NYX-1" || i.Summary != "Add a health probe" || i.Status != "Ready" {
		t.Errorf("issue = %+v", i)
	}
	if i.Description != "Do the thing." {
		t.Errorf("description = %q, want the ADF flattened", i.Description)
	}
	if i.Updated.IsZero() {
		t.Error("updated was not parsed")
	}
}

// The card body needs prose; Jira v3 returns a node tree.
func TestADFToText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", ``, ""},
		{"plain paragraph",
			`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello"}]}]}`,
			"Hello"},
		{"two paragraphs",
			`{"type":"doc","content":[
				{"type":"paragraph","content":[{"type":"text","text":"One"}]},
				{"type":"paragraph","content":[{"type":"text","text":"Two"}]}]}`,
			"One\nTwo"},
		{"hard break",
			`{"type":"doc","content":[{"type":"paragraph","content":[
				{"type":"text","text":"a"},{"type":"hardBreak"},{"type":"text","text":"b"}]}]}`,
			"a\nb"},
		{"bullet list",
			`{"type":"doc","content":[{"type":"bulletList","content":[
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"x"}]}]}]}]}`,
			"x"},
		{"unknown node types are skipped, not dumped",
			`{"type":"doc","content":[{"type":"mediaSingle","content":[{"type":"media","attrs":{"id":"1"}}]}]}`,
			""},
		{"malformed json", `not json`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ADFToText(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("ADFToText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAssignAndTransition(t *testing.T) {
	var log []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		log = append(log, seen{r.Method, r.URL.RequestURI(), "", body})
		if strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == http.MethodGet {
			io.WriteString(w, `{"transitions":[{"id":"31","name":"In Progress"}]}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "m@x", "tok").WithTransport(srv.Client(), srv.URL)
	ctx := context.Background()

	if err := c.Assign(ctx, "NYX-1", "acc-1"); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if log[0].method != http.MethodPut || log[0].body["accountId"] != "acc-1" {
		t.Errorf("assign request = %+v", log[0])
	}

	if err := c.Transition(ctx, "NYX-1", "in progress"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	last := log[len(log)-1]
	if last.method != http.MethodPost {
		t.Fatalf("transition request = %+v", last)
	}
	trans, _ := last.body["transition"].(map[string]any)
	if trans["id"] != "31" {
		t.Errorf("transition id = %v, want the one matched case-insensitively", trans["id"])
	}
}

// A missing transition is reported. The Python printed and carried on, which
// left tickets assigned but still sitting in Ready.
func TestMissingTransitionIsAnError(t *testing.T) {
	var log []seen
	c, stop := server(t, &log, 200, `{"transitions":[{"id":"11","name":"Done"}]}`)
	defer stop()

	err := c.Transition(context.Background(), "NYX-1", "In Progress")
	if err == nil {
		t.Fatal("Transition = nil error when the transition does not exist")
	}
	if !strings.Contains(err.Error(), "In Progress") {
		t.Errorf("err = %q, want it to name the missing transition", err)
	}
}

func TestErrorsCarryJirasMessage(t *testing.T) {
	var log []seen
	c, stop := server(t, &log, 400, `{"errorMessages":["Field 'assignee' cannot be set"]}`)
	defer stop()

	err := c.Assign(context.Background(), "NYX-1", "acc-1")
	if err == nil || !strings.Contains(err.Error(), "cannot be set") {
		t.Fatalf("err = %v, want Jira's own explanation", err)
	}
}

func TestFindUserRejectsAnEmptyResult(t *testing.T) {
	var log []seen
	c, stop := server(t, &log, 200, `[]`)
	defer stop()

	if _, err := c.FindUser(context.Background(), "nobody@x"); err == nil {
		t.Fatal("FindUser = nil error for an unknown email")
	}
}

func TestClaimed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue Issue
		want  bool
	}{
		{"ready and unassigned", Issue{Status: "Ready"}, false},
		{"assigned", Issue{Status: "Ready", Assignee: "Annie"}, true},
		{"moved on", Issue{Status: "In Progress"}, true},
		{"status case does not matter", Issue{Status: "ready"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.issue.Claimed("Ready"); got != tc.want {
				t.Errorf("Claimed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatUpdated(t *testing.T) {
	at := time.Date(2026, 5, 14, 15, 42, 0, 0, time.UTC)
	if got := FormatUpdated(at); got != "May 14, 2026 at 3:42 PM" {
		t.Errorf("FormatUpdated = %q", got)
	}
	if FormatUpdated(time.Time{}) != "" {
		t.Error("a zero time should render as empty, not a fake date")
	}
}

func TestBrowseURL(t *testing.T) {
	c := New("https://vmxproperty.atlassian.net/", "m@x", "t")
	if got := c.BrowseURL("NYX-1"); got != "https://vmxproperty.atlassian.net/browse/NYX-1" {
		t.Errorf("BrowseURL = %q", got)
	}
}
