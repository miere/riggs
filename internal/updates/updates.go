// Package updates reports whether a newer Riggs release is published on
// GitHub, and carries the release notes that go with it.
//
// It backs the App Home tab (§7e): the Home surface shows the running version
// to everyone, and — for the admin, and only when there is something to install
// — the latest release's notes with an Update button beside them.
//
// The check is deliberately failure-tolerant. Every path returns a usable
// Release; the error is informational, so the Home tab renders the version line
// on its own rather than an apology. Results are cached for a TTL because
// app_home_opened fires every time the admin so much as glances at the app, and
// GitHub's unauthenticated quota is 60 requests an hour.
//
// One rule is deliberately the OPPOSITE of Murtaugh's. Murtaugh refuses to
// offer an update to a "dev" build, on the grounds that silently overwriting a
// local checkout would surprise the developer. Riggs is specified the other
// way: a Riggs running a dev build must always be able to take the latest
// stable. The daemon under launchd is the ordinary case, and it is routinely
// running a binary built from a working tree — refusing it would mean the
// button is missing precisely on the machine that needs it.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// DefaultOwner and DefaultRepo are where Riggs' releases are published.
const (
	DefaultOwner = "miere"
	DefaultRepo  = "riggs"
)

// defaultTTL is how long a successful lookup is reused before the next Home
// open triggers a fresh fetch.
const defaultTTL = time.Hour

// HTTPGet performs a GET against url and returns the body. It is the seam that
// keeps the network out of the tests.
type HTTPGet func(ctx context.Context, url string) ([]byte, error)

// Deps is the explicit dependency bundle passed to New.
type Deps struct {
	// Current is the running binary's version ("v0.1.0", "dev", a bare VCS
	// revision — whatever version.String() reported).
	Current string
	// Owner and Repo identify the repository whose releases are polled. Empty
	// selects the Riggs defaults.
	Owner, Repo string
	// HTTPGet fetches a URL; nil disables checking entirely, which renders the
	// Home tab as a version line and nothing else.
	HTTPGet HTTPGet
	// TTL overrides the cache lifetime; <= 0 selects defaultTTL.
	TTL time.Duration
	// Now supplies the current time; nil selects time.Now.
	Now func() time.Time
}

// Release is the outcome of a check.
type Release struct {
	// Current is the running version, echoed back so a renderer needs only
	// this one value.
	Current string
	// Tag is the latest published release, or "" when the lookup failed.
	Tag string
	// Notes is that release's body, as GitHub-flavoured Markdown. It is NOT
	// converted here: this package knows about releases, and internal/slackmd
	// knows about Slack.
	Notes string
	// URL is the release's page, for a footer link.
	URL string
	// Available is true when Tag is something the running binary does not
	// already have.
	Available bool
}

// Checker compares the running version against the latest GitHub release and
// caches the answer. Safe for concurrent use.
type Checker struct {
	current string
	owner   string
	repo    string
	httpGet HTTPGet
	ttl     time.Duration
	now     func() time.Time

	mu        sync.Mutex
	cached    Release
	fetchedAt time.Time
	hasCache  bool
}

// New constructs a Checker from the supplied dependencies.
func New(deps Deps) *Checker {
	owner, repo := deps.Owner, deps.Repo
	if owner == "" {
		owner = DefaultOwner
	}
	if repo == "" {
		repo = DefaultRepo
	}
	ttl := deps.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Checker{
		current: strings.TrimSpace(deps.Current),
		owner:   owner,
		repo:    repo,
		httpGet: deps.HTTPGet,
		ttl:     ttl,
		now:     now,
	}
}

// Check reports the latest release and whether it is worth installing.
//
// It returns a usable Release in every case. A failed fetch falls back to the
// last good answer when there is one, and to a bare "nothing available"
// otherwise; the error rides alongside so the caller can log it without
// branching its render path.
func (c *Checker) Check(ctx context.Context) (Release, error) {
	if c.httpGet == nil {
		return Release{Current: c.current}, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasCache && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}

	rel, err := c.fetchLatest(ctx)
	if err != nil {
		if c.hasCache {
			return c.cached, err
		}
		return Release{Current: c.current}, err
	}

	rel.Current = c.current
	rel.Available = isNewer(c.current, rel.Tag)
	c.cached, c.fetchedAt, c.hasCache = rel, c.now(), true
	return rel, nil
}

// Invalidate drops the cache, so the next Check fetches.
//
// It exists for the moment immediately after an install: the Home tab is
// republished, and a cached "v0.2.0 is available" would still be sitting there
// telling the admin to install what they have just installed.
func (c *Checker) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hasCache = false
}

// AssetURL returns the download URL for this platform's binary in the given
// release, using the naming the release workflow produces.
func (c *Checker) AssetURL(ctx context.Context, tag, goos, goarch string) (string, error) {
	if c.httpGet == nil {
		return "", fmt.Errorf("updates: no HTTP client configured")
	}
	body, err := c.httpGet(ctx, c.releaseAPIURL(tag))
	if err != nil {
		return "", fmt.Errorf("fetch release %s: %w", displayTag(tag), err)
	}
	var doc struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse release JSON: %w", err)
	}
	want := AssetName(doc.TagName, goos, goarch)
	for _, a := range doc.Assets {
		if a.Name == want {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s has no asset for %s/%s (looked for %q)", doc.TagName, goos, goarch, want)
}

// AssetName is the release asset naming contract, in one place. The release
// workflow builds these names and this package looks them up; a mismatch means
// the Update button fails on a release nobody tested by hand, so the two sides
// are pinned to one function.
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("riggs-%s-%s-%s", tag, goos, goarch)
}

// ReleaseURL is the human-facing page for a tag, for a "what changed" link.
func (c *Checker) ReleaseURL(tag string) string {
	if tag = strings.TrimSpace(tag); tag == "" {
		return fmt.Sprintf("https://github.com/%s/%s/releases/latest", c.owner, c.repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", c.owner, c.repo, tag)
}

// releaseAPIURL addresses either a specific tag or the latest release.
func (c *Checker) releaseAPIURL(tag string) string {
	if tag = strings.TrimSpace(tag); tag == "" {
		return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", c.owner, c.repo)
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", c.owner, c.repo, tag)
}

// fetchLatest reads the latest release's tag, body and page URL.
func (c *Checker) fetchLatest(ctx context.Context) (Release, error) {
	body, err := c.httpGet(ctx, c.releaseAPIURL(""))
	if err != nil {
		return Release{}, fmt.Errorf("fetch latest release: %w", err)
	}
	var doc struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Release{}, fmt.Errorf("parse release JSON: %w", err)
	}
	tag := strings.TrimSpace(doc.TagName)
	if tag == "" {
		return Release{}, fmt.Errorf("release JSON has no tag_name")
	}
	return Release{Tag: tag, Notes: doc.Body, URL: doc.HTMLURL}, nil
}

// isNewer reports whether tag is something the running build does not have.
//
// Two release versions are compared by semver. A running version that is NOT a
// release — "dev", or the bare VCS revision version.String() falls back to —
// counts as behind any published release, which is the specified behaviour: a
// dev Riggs must always be able to take the latest stable.
func isNewer(current, tag string) bool {
	ct := canonical(tag)
	if ct == "" {
		return false
	}
	cc := canonical(current)
	if cc == "" {
		return true
	}
	return semver.Compare(ct, cc) > 0
}

// canonical normalises a tag into something x/mod/semver accepts, which
// requires a leading "v". It returns "" when the value is not a semantic
// version at all.
func canonical(v string) string {
	if v = strings.TrimSpace(v); v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// displayTag renders a tag for an error message, naming the latest release
// when the caller asked for no tag in particular.
func displayTag(tag string) string {
	if t := strings.TrimSpace(tag); t != "" {
		return t
	}
	return "latest"
}
