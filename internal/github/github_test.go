package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runner builds a Runner returning fixed streams and error.
func runner(stdout, stderr string, err error) Runner {
	return func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte(stdout), []byte(stderr), err
	}
}

// gh writes `auth status` to STDERR, not stdout. Parsing only stdout would
// silently find no token on a perfectly healthy machine.
func TestAuthFromGHReadsStderr(t *testing.T) {
	const stderr = `github.com
  ✓ Logged in to github.com as miere (oauth_token)
  ✓ Git operations for github.com configured to use ssh protocol.
  ✓ Token: gho_abc123
`
	auth, err := AuthFromGH(context.Background(), runner("", stderr, nil))
	if err != nil {
		t.Fatalf("AuthFromGH: %v", err)
	}
	if auth.Token != "gho_abc123" {
		t.Errorf("token = %q, want the value from stderr", auth.Token)
	}
	if auth.Login != "miere" {
		t.Errorf("login = %q, want it discovered from the same output", auth.Login)
	}
}

// A token on stderr counts even when gh exits non-zero, because a non-zero
// exit can mean an unrelated host in the same report is unhealthy.
func TestAuthFromGHIgnoresExitCodeWhenTokenPresent(t *testing.T) {
	auth, err := AuthFromGH(context.Background(),
		runner("", "  ✓ Token: gho_xyz\n", errors.New("exit status 1")))
	if err != nil {
		t.Fatalf("AuthFromGH: %v", err)
	}
	if auth.Token != "gho_xyz" {
		t.Errorf("token = %q, want it parsed despite the exit code", auth.Token)
	}
}

func TestAuthFromGHWithoutTokenIsTyped(t *testing.T) {
	_, err := AuthFromGH(context.Background(),
		runner("", "You are not logged into any GitHub hosts.\n", errors.New("exit status 1")))
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("err = %q, want it to say how to fix the problem", err)
	}
}

func TestReviewRequested(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"items":[
			{"number":20069,"title":"Fix the resolver",
			 "html_url":"https://github.com/acme/monolith/pull/20069",
			 "repository_url":"https://api.github.com/repos/acme/monolith",
			 "updated_at":"2026-08-09T01:02:03Z","user":{"login":"alex"}}]}`)
	}))
	defer srv.Close()

	prs, err := New("gho_test").WithTransport(srv.Client(), srv.URL).
		ReviewRequested(context.Background(), "miere", 1)
	if err != nil {
		t.Fatalf("ReviewRequested: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs, want 1", len(prs))
	}
	pr := prs[0]
	// The search payload has no plain repository field; it has to be derived.
	if pr.Repo != "acme/monolith" {
		t.Errorf("repo = %q, want it derived from repository_url", pr.Repo)
	}
	if pr.Ref() != "acme/monolith#20069" {
		t.Errorf("Ref() = %q, want the owner/repo#number form the cards key on", pr.Ref())
	}
	if pr.Author != "alex" || pr.Title != "Fix the resolver" {
		t.Errorf("pr = %+v, want the payload's title and author", pr)
	}
	if gotAuth != "Bearer gho_test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotPath, "review-requested%3Amiere") {
		t.Errorf("query = %q, want it scoped to the reviewer", gotPath)
	}
	// An archived repository is read-only: GitHub rejects a review on it with
	// `422 lock prevents review`, so those must never reach the queue.
	if !strings.Contains(gotPath, "archived%3Afalse") {
		t.Errorf("query = %q, want archived repositories excluded", gotPath)
	}
}

func TestReviewRequestedNeedsALogin(t *testing.T) {
	if _, err := New("t").ReviewRequested(context.Background(), "", 1); err == nil {
		t.Fatal("ReviewRequested with no login = nil error")
	}
}

// Rate limiting gets its own message, because "HTTP 403" alone sends you
// looking for a permissions problem that is not there.
func TestRateLimitIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := New("t").WithTransport(srv.Client(), srv.URL).
		ReviewRequested(context.Background(), "miere", 1)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, want it identified as rate limiting", err)
	}
}

func TestHTTPErrorCarriesTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"Bad credentials"}`)
	}))
	defer srv.Close()

	_, err := New("t").WithTransport(srv.Client(), srv.URL).
		ReviewRequested(context.Background(), "miere", 1)
	if err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("err = %v, want GitHub's own explanation included", err)
	}
}

func TestRepoFromAPIURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://api.github.com/repos/owner/name", "owner/name"},
		{"https://api.github.com/repos/owner/name/", "owner/name"},
		{"nonsense", ""},
	} {
		if got := repoFromAPIURL(tc.in); got != tc.want {
			t.Errorf("repoFromAPIURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The archived flag rides along in the pull request payload
// (base.repo.archived), so a read-only repository is detectable without a
// second call to /repos/{owner}/{repo}.
func TestPullRequestDetailReadsTheArchivedFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"number":43,"title":"Bump pytest","state":"open",
			"head":{"sha":"abc"},"user":{"login":"app/dependabot"},
			"base":{"repo":{"archived":true}}}`)
	}))
	defer srv.Close()

	d, err := New("t").WithTransport(srv.Client(), srv.URL).
		PullRequestDetail(context.Background(), "o/archived-repo", 43)
	if err != nil {
		t.Fatalf("PullRequestDetail: %v", err)
	}
	if !d.Archived {
		t.Error("Archived = false, want it read from base.repo.archived")
	}
}

func TestPullRequestDetailDefaultsToNotArchived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"number":1,"state":"open","head":{"sha":"abc"},
			"user":{"login":"alex"},"base":{"repo":{"archived":false}}}`)
	}))
	defer srv.Close()

	d, err := New("t").WithTransport(srv.Client(), srv.URL).
		PullRequestDetail(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatalf("PullRequestDetail: %v", err)
	}
	if d.Archived {
		t.Error("Archived = true, want false for a live repository")
	}
}
