// Package github reads GitHub over its REST API.
//
// Riggs holds no GitHub credential of its own: the token comes from the
// authenticated `gh` CLI, which stays the credential provider. Everything
// after that is Riggs' own HTTP, so it can use conditional requests — see
// ARCHITECTURE.md §8 for why that matters (the GraphQL path `gh` generates
// runs roughly 2x over the hourly quota).
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL is the GitHub REST API root.
const DefaultBaseURL = "https://api.github.com"

// ErrNoToken is returned when `gh` is absent or not logged in.
var ErrNoToken = errors.New("github: no token from the gh CLI")

// Runner executes an external command, returning stdout and stderr
// separately. It is the seam that keeps `gh` out of the call sites.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

// ExecRunner runs commands for real.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return []byte(out.String()), []byte(errBuf.String()), err
}

var (
	tokenPattern = regexp.MustCompile(`Token:\s*(\S+)`)
	loginPattern = regexp.MustCompile(`Logged in to \S+ as (\S+)`)
)

// Auth is what the gh CLI knows: the token, and who it belongs to.
type Auth struct {
	Token string
	Login string
}

// AuthFromGH extracts the token and login from `gh auth status --show-token`.
//
// The output goes to **stderr**, not stdout, on the gh versions we support
// (2.14.x), and a non-zero exit only means "not logged in" — so both streams
// are parsed regardless of the exit code, and the absence of a token is what
// decides failure.
func AuthFromGH(ctx context.Context, run Runner) (Auth, error) {
	if run == nil {
		run = ExecRunner
	}
	stdout, stderr, runErr := run(ctx, "gh", "auth", "status", "--show-token")
	combined := string(stderr) + "\n" + string(stdout)

	m := tokenPattern.FindStringSubmatch(combined)
	if m == nil {
		if runErr != nil {
			return Auth{}, fmt.Errorf("%w: %v (run `gh auth login`)", ErrNoToken, runErr)
		}
		return Auth{}, fmt.Errorf("%w: no token in `gh auth status` output (run `gh auth login`)", ErrNoToken)
	}
	auth := Auth{Token: m[1]}
	if l := loginPattern.FindStringSubmatch(combined); l != nil {
		auth.Login = l[1]
	}
	return auth, nil
}

// Doer is the HTTP seam, so tests drive the client without a network.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client reads the GitHub REST API.
type Client struct {
	http    Doer
	baseURL string
	token   string
	cache   Cache
	now     func() time.Time
	stats   Stats
}

// New builds a client for the given token.
func New(token string) *Client {
	return &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: DefaultBaseURL,
		token:   token,
	}
}

// WithTransport overrides the HTTP seam and base URL; intended for tests.
func (c *Client) WithTransport(d Doer, baseURL string) *Client {
	c.http, c.baseURL = d, baseURL
	return c
}

// PullRequest is the subset of a PR the cards need.
type PullRequest struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Author    string    `json:"author"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Ref renders the owner/repo#number form used as a card's identity and as the
// value carried by its buttons.
func (p PullRequest) Ref() string { return fmt.Sprintf("%s#%d", p.Repo, p.Number) }

// ReviewRequested lists open PRs where login is a requested reviewer,
// most-recently-updated first.
//
// This is the search API, which has its own much tighter bucket (30
// requests/minute) than the core one — so it is called once per tick and never
// fanned out.
func (c *Client) ReviewRequested(ctx context.Context, login string, limit int) ([]PullRequest, error) {
	if login == "" {
		return nil, fmt.Errorf("github: no reviewer login given")
	}
	if limit <= 0 {
		limit = 30
	}
	q := url.Values{}
	q.Set("q", fmt.Sprintf("is:open is:pr review-requested:%s", login))
	q.Set("per_page", fmt.Sprint(limit))
	q.Set("sort", "updated")

	var payload struct {
		Items []struct {
			Number        int       `json:"number"`
			Title         string    `json:"title"`
			HTMLURL       string    `json:"html_url"`
			RepositoryURL string    `json:"repository_url"`
			UpdatedAt     time.Time `json:"updated_at"`
			User          struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/search/issues?"+q.Encode(), &payload); err != nil {
		return nil, err
	}

	out := make([]PullRequest, 0, len(payload.Items))
	for _, it := range payload.Items {
		out = append(out, PullRequest{
			Repo:      repoFromAPIURL(it.RepositoryURL),
			Number:    it.Number,
			Title:     it.Title,
			URL:       it.HTMLURL,
			Author:    it.User.Login,
			UpdatedAt: it.UpdatedAt,
		})
	}
	return out, nil
}

// repoFromAPIURL reduces https://api.github.com/repos/owner/name to
// owner/name. The search payload carries no plain repository field.
func repoFromAPIURL(apiURL string) string {
	const marker = "/repos/"
	if i := strings.Index(apiURL, marker); i >= 0 {
		return strings.Trim(apiURL[i+len(marker):], "/")
	}
	return ""
}

// ErrNotFound is returned for a 404, which for a PR read means "not visible to
// this token" as often as it means "gone".
var ErrNotFound = errors.New("github: not found")

// get performs one authenticated, conditional GET and decodes the JSON body.
//
// A 304 is served from the cache and costs no rate-limit quota, which is the
// entire reason this client exists rather than shelling `gh` (§8).
func (c *Client) get(ctx context.Context, path string, out any) error {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("github: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	cachedBody, hasCache := c.conditional(ctx, req, url)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("github: reading %s: %w", path, err)
	}
	c.stats.Requests++

	switch {
	case resp.StatusCode == http.StatusNotModified && hasCache:
		c.stats.NotModified++
		body = cachedBody
	case resp.StatusCode == http.StatusNotModified:
		// A 304 with nothing cached means our own bookkeeping is wrong;
		// there is no body to fall back on.
		return fmt.Errorf("github: GET %s: 304 with no cached response", path)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return fmt.Errorf("github: rate limited on %s (resets at %s)",
			path, resp.Header.Get("X-RateLimit-Reset"))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("github: GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	default:
		c.remember(ctx, url, resp.Header.Get("ETag"), body)
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("github: decoding %s: %w", path, err)
	}
	return nil
}
