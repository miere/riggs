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
			 "html_url":"https://github.com/UpsideRealty/upside/pull/20069",
			 "repository_url":"https://api.github.com/repos/UpsideRealty/upside",
			 "updated_at":"2026-08-09T01:02:03Z","user":{"login":"hjed"}}]}`)
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
	if pr.Repo != "UpsideRealty/upside" {
		t.Errorf("repo = %q, want it derived from repository_url", pr.Repo)
	}
	if pr.Ref() != "UpsideRealty/upside#20069" {
		t.Errorf("Ref() = %q, want the owner/repo#number form the cards key on", pr.Ref())
	}
	if pr.Author != "hjed" || pr.Title != "Fix the resolver" {
		t.Errorf("pr = %+v, want the payload's title and author", pr)
	}
	if gotAuth != "Bearer gho_test" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.Contains(gotPath, "review-requested%3Amiere") {
		t.Errorf("query = %q, want it scoped to the reviewer", gotPath)
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
