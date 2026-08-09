package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MergeMethodRebase is the only merge method Riggs will ever use.
//
// Our repositories do not allow squash merges and do not have auto-merge
// enabled. "Approve and Merge" means an immediate rebase merge — never
// --squash, never --auto. This is a deliberate constraint carried over from
// the Python, not an incidental default.
const MergeMethodRebase = "rebase"

// AuthenticatedLogin returns the login the token belongs to.
//
// It is needed to answer "is *our* approval already standing", which is what
// stops a second click re-approving a pull request we have already approved.
func (c *Client) AuthenticatedLogin(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := c.get(ctx, "/user", &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", fmt.Errorf("github: /user returned no login")
	}
	return user.Login, nil
}

// Approve submits an approving review.
func (c *Client) Approve(ctx context.Context, repo string, number int, body string) error {
	payload := map[string]any{"event": "APPROVE"}
	if body != "" {
		payload["body"] = body
	}
	return c.write(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, number), payload, nil)
}

// Merge performs a rebase merge.
//
// The method is not a parameter. Making it one would invite a caller to pass
// "squash", which our repositories reject — and a merge that fails after an
// approval has landed is the worst outcome of this whole flow.
func (c *Client) Merge(ctx context.Context, repo string, number int) error {
	return c.write(ctx, http.MethodPut,
		fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number),
		map[string]any{"merge_method": MergeMethodRebase}, nil)
}

// write performs one authenticated mutating request.
//
// Mutations deliberately do not go through get(): they are never conditional,
// never cached, and never retried. Retrying a write whose response was lost
// could approve twice or merge something that was already merged.
func (c *Client) write(ctx context.Context, method, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("github: encoding %s %s: %w", method, path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github: building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	c.stats.Requests++
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("github: %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, apiMessage(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("github: decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// apiMessage pulls GitHub's own explanation out of an error body, so the
// message reaching Slack says what GitHub said rather than a bare status code.
func apiMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Message == "" {
		return strings.TrimSpace(string(raw))
	}
	if len(e.Errors) > 0 && e.Errors[0].Message != "" {
		return e.Message + ": " + e.Errors[0].Message
	}
	return e.Message
}
