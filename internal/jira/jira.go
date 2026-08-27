// Package jira reads and updates Atlassian Jira over its REST v3 API.
//
// Credentials are Basic auth over email + API token, taken from the config
// (which falls back to the ATLASSIAN_JIRA_* variables Murtaugh's .env already
// exports, so nothing needs re-provisioning at cutover).
package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ErrNotConfigured is returned when no credentials are available. Like every
// other capability gap it disables a feature; it never crashes the binary.
var ErrNotConfigured = errors.New("jira: not configured")

// Doer is the HTTP seam, so tests drive the client without a network.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client talks to Jira.
type Client struct {
	http    Doer
	baseURL string
	auth    string
}

// New builds a client for the given tenant and credentials. All three are
// required; with any of them missing every call fails with ErrNotConfigured
// rather than a confusing 401 — or, in the tenant's case, a request to
// somebody else's Jira.
//
// There is deliberately NO default tenant. One used to be baked in here, which
// meant a machine with no `jira.base-url` silently talked to whichever
// Atlassian instance this source happened to name. That is a fine default right
// up until it is the wrong one, and nothing about the failure would have said
// so. The tenant is now configuration, like the credentials that authenticate
// against it.
//
// This is not the same kind of setting as github.DefaultBaseURL or
// slack.DefaultBaseURL. Those are vendor API roots — one address for everyone.
// A Jira tenant is per-customer.
func New(baseURL, email, token string) *Client {
	c := &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
	if email != "" && token != "" {
		c.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
	}
	return c
}

// WithTransport overrides the HTTP seam and base URL; intended for tests.
func (c *Client) WithTransport(d Doer, baseURL string) *Client {
	c.http, c.baseURL = d, strings.TrimRight(baseURL, "/")
	return c
}

// BrowseURL is the human link for a ticket.
func (c *Client) BrowseURL(key string) string { return c.baseURL + "/browse/" + key }

// Issue is the subset of a ticket the cards need.
type Issue struct {
	Key         string `json:"key"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Assignee    string `json:"assignee"`
	Reporter    string `json:"reporter"`
	Parent      string `json:"parent,omitempty"`
	// Created orders the bulk digest: FIFO is by how long the ticket has been
	// waiting, not by when Riggs first saw it.
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Claimed reports whether the ticket is no longer up for grabs — assigned, or
// moved out of the status the query selected on.
func (i Issue) Claimed(readyStatus string) bool {
	return i.Assignee != "" || !strings.EqualFold(i.Status, readyStatus)
}

// Search runs a JQL query.
//
// The JQL is supplied by the caller rather than hardcoded, which is what makes
// this one tool able to serve any ticket queue rather than only the `ai-able`
// board the Python was pinned to.
func (c *Client) Search(ctx context.Context, jql string, max int) ([]Issue, error) {
	if max <= 0 {
		max = 20
	}
	body := map[string]any{
		"jql":        jql,
		"maxResults": max,
		"fields":     []string{"summary", "status", "assignee", "reporter", "parent", "created", "updated", "description"},
	}
	var payload struct {
		Issues []rawIssue `json:"issues"`
	}
	if err := c.call(ctx, http.MethodPost, "/rest/api/3/search/jql", body, &payload); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(payload.Issues))
	for _, raw := range payload.Issues {
		out = append(out, raw.issue())
	}
	return out, nil
}

// Get fetches one ticket.
func (c *Client) Get(ctx context.Context, key string) (Issue, error) {
	var raw rawIssue
	path := "/rest/api/3/issue/" + key +
		"?fields=summary,description,created,updated,assignee,reporter,status,parent"
	if err := c.call(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return Issue{}, err
	}
	return raw.issue(), nil
}

// rawIssue mirrors the API shape.
type rawIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Created     string          `json:"created"`
		Updated     string          `json:"updated"`
		Assignee    *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Reporter *struct {
			DisplayName string `json:"displayName"`
		} `json:"reporter"`
		Status *struct {
			Name string `json:"name"`
		} `json:"status"`
		Parent *struct {
			Key string `json:"key"`
		} `json:"parent"`
	} `json:"fields"`
}

func (r rawIssue) issue() Issue {
	i := Issue{Key: r.Key, Summary: r.Fields.Summary}
	if r.Fields.Assignee != nil {
		i.Assignee = r.Fields.Assignee.DisplayName
	}
	if r.Fields.Reporter != nil {
		i.Reporter = r.Fields.Reporter.DisplayName
	}
	if r.Fields.Status != nil {
		i.Status = r.Fields.Status.Name
	}
	if r.Fields.Parent != nil {
		i.Parent = r.Fields.Parent.Key
	}
	i.Description = ADFToText(r.Fields.Description)
	// Jira stamps "2026-08-09T10:11:12.000+1000".
	i.Created = parseStamp(r.Fields.Created)
	i.Updated = parseStamp(r.Fields.Updated)
	return i
}

// User is a Jira account.
type User struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
}

// FindUser resolves an email to an account.
func (c *Client) FindUser(ctx context.Context, email string) (User, error) {
	var users []User
	if err := c.call(ctx, http.MethodGet, "/rest/api/3/user/search?query="+email, nil, &users); err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, fmt.Errorf("jira: no account for %s", email)
	}
	return users[0], nil
}

// Assign sets the ticket's assignee.
func (c *Client) Assign(ctx context.Context, key, accountID string) error {
	return c.call(ctx, http.MethodPut, "/rest/api/3/issue/"+key+"/assignee",
		map[string]any{"accountId": accountID}, nil)
}

// Transition moves a ticket to the named status, matched case-insensitively.
//
// A missing transition is reported rather than silently skipped: the Python
// printed and carried on, which left tickets assigned but still sitting in
// Ready with nobody the wiser.
func (c *Client) Transition(ctx context.Context, key, name string) error {
	var payload struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"transitions"`
	}
	if err := c.call(ctx, http.MethodGet, "/rest/api/3/issue/"+key+"/transitions", nil, &payload); err != nil {
		return err
	}
	for _, t := range payload.Transitions {
		if strings.EqualFold(t.Name, name) {
			return c.call(ctx, http.MethodPost, "/rest/api/3/issue/"+key+"/transitions",
				map[string]any{"transition": map[string]any{"id": t.ID}}, nil)
		}
	}
	return fmt.Errorf("jira: %s has no %q transition available", key, name)
}

// call performs one authenticated request.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	if c.baseURL == "" {
		return fmt.Errorf("%w: set jira.base-url (or ATLASSIAN_BASE_URL) — there is no default tenant", ErrNotConfigured)
	}
	if c.auth == "" {
		return fmt.Errorf("%w: set jira.email and jira.token (or ATLASSIAN_JIRA_EMAIL/_TOKEN)", ErrNotConfigured)
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("jira: encoding %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("jira: building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("jira: %s %s: HTTP %d: %s", method, path, resp.StatusCode, errorMessage(raw))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("jira: decoding %s %s: %w", method, path, err)
	}
	return nil
}

// errorMessage extracts Jira's own complaint so a failure says what Jira said.
func errorMessage(raw []byte) string {
	var e struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if err := json.Unmarshal(raw, &e); err == nil {
		if len(e.ErrorMessages) > 0 {
			return strings.Join(e.ErrorMessages, "; ")
		}
		if len(e.Errors) > 0 {
			var parts []string
			for k, v := range e.Errors {
				parts = append(parts, k+": "+v)
			}
			return strings.Join(parts, "; ")
		}
	}
	return strings.TrimSpace(string(raw))
}

// blankLines collapses runs of blank lines left by the ADF walk.
var blankLines = regexp.MustCompile(`\n{3,}`)

// ADFToText flattens an Atlassian Document Format value to plain text.
//
// Jira v3 returns descriptions as a node tree, not a string. The summariser
// needs prose, so the tree is walked rather than JSON-dumped.
func ADFToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	return strings.TrimSpace(blankLines.ReplaceAllString(walkADF(node), "\n\n"))
}

// walkADF renders one ADF node.
func walkADF(node any) string {
	switch n := node.(type) {
	case []any:
		var b strings.Builder
		for _, child := range n {
			b.WriteString(walkADF(child))
		}
		return b.String()
	case map[string]any:
		switch n["type"] {
		case "text":
			if s, ok := n["text"].(string); ok {
				return s
			}
			return ""
		case "hardBreak":
			return "\n"
		}
		inner := walkADF(n["content"])
		switch n["type"] {
		case "paragraph", "heading", "listItem", "bulletList", "orderedList", "codeBlock":
			return inner + "\n"
		}
		return inner
	default:
		return ""
	}
}

// FormatUpdated renders a timestamp as the cards have always shown it:
// "May 14, 2026 at 3:42 PM".
func FormatUpdated(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	hour := t.Hour() % 12
	if hour == 0 {
		hour = 12
	}
	return fmt.Sprintf("%s %d, %d at %d:%02d %s",
		t.Format("Jan"), t.Day(), t.Year(), hour, t.Minute(), t.Format("PM"))
}

// parseStamp reads Jira's timestamp format. An unparseable or absent value
// yields the zero time, which every caller already treats as "unknown" — a
// ticket with no readable date sorts first under FIFO, which is the safe end of
// the queue to be at.
func parseStamp(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		return time.Time{}
	}
	return t
}
