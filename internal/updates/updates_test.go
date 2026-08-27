package updates

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// stubGet answers a fixed set of URLs, and counts how often it was called.
type stubGet struct {
	bodies map[string]string
	err    error
	calls  int
}

func (s *stubGet) get(_ context.Context, url string) ([]byte, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	body, ok := s.bodies[url]
	if !ok {
		return nil, fmt.Errorf("no stub for %s", url)
	}
	return []byte(body), nil
}

const latestURL = "https://api.github.com/repos/miere/riggs/releases/latest"

func releaseJSON(tag, notes string) string {
	return fmt.Sprintf(`{"tag_name":%q,"body":%q,"html_url":"https://github.com/miere/riggs/releases/tag/%s"}`,
		tag, notes, tag)
}

func checkerFor(t *testing.T, current string, s *stubGet) *Checker {
	t.Helper()
	return New(Deps{Current: current, HTTPGet: s.get})
}

func TestANewerTagIsAvailable(t *testing.T) {
	s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.2.0", "## Fixes\n- a thing")}}
	rel, err := checkerFor(t, "v0.1.0", s).Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !rel.Available || rel.Tag != "v0.2.0" {
		t.Fatalf("rel = %+v, want v0.2.0 available", rel)
	}
	if rel.Notes == "" || rel.URL == "" {
		t.Errorf("release notes and URL not carried: %+v", rel)
	}
}

func TestTheSameTagIsNotAnUpdate(t *testing.T) {
	s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.2.0", "")}}
	rel, _ := checkerFor(t, "v0.2.0", s).Check(context.Background())
	if rel.Available {
		t.Fatalf("offered an update to the version already running: %+v", rel)
	}
}

// A tag older than what is running is not an update either — that is a
// re-tagged or yanked release, and downgrading onto it silently would be worse
// than doing nothing.
func TestAnOlderTagIsNotAnUpdate(t *testing.T) {
	s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.1.0", "")}}
	rel, _ := checkerFor(t, "v0.9.0", s).Check(context.Background())
	if rel.Available {
		t.Fatalf("offered a downgrade: %+v", rel)
	}
}

// The specified divergence from Murtaugh, and the reason this package does not
// simply copy it: a Riggs built from a working tree must still be able to take
// the latest stable. Murtaugh refuses exactly this case.
func TestADevBuildIsOfferedTheLatestStable(t *testing.T) {
	for _, current := range []string{"dev", "a1b2c3d4e5f6", "a1b2c3d4e5f6-dirty", ""} {
		s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.2.0", "")}}
		rel, err := checkerFor(t, current, s).Check(context.Background())
		if err != nil {
			t.Fatalf("Check(%q): %v", current, err)
		}
		if !rel.Available {
			t.Errorf("current %q was not offered v0.2.0: %+v", current, rel)
		}
	}
}

// app_home_opened fires on every glance at the app. Against GitHub's
// 60-an-hour unauthenticated quota, one lookup per glance is a rate limit
// waiting to happen.
func TestTheLookupIsCached(t *testing.T) {
	s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.2.0", "")}}
	c := checkerFor(t, "v0.1.0", s)

	for range 5 {
		if _, err := c.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	if s.calls != 1 {
		t.Fatalf("made %d GitHub calls, want 1", s.calls)
	}

	c.Invalidate()
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check after Invalidate: %v", err)
	}
	if s.calls != 2 {
		t.Fatalf("Invalidate did not force a fetch: %d calls", s.calls)
	}
}

func TestTheCacheExpires(t *testing.T) {
	s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.2.0", "")}}
	now := time.Unix(1_700_000_000, 0)
	c := New(Deps{
		Current: "v0.1.0", HTTPGet: s.get, TTL: time.Hour,
		Now: func() time.Time { return now },
	})

	c.Check(context.Background())
	now = now.Add(59 * time.Minute)
	c.Check(context.Background())
	if s.calls != 1 {
		t.Fatalf("fetched %d times inside the TTL, want 1", s.calls)
	}
	now = now.Add(2 * time.Minute)
	c.Check(context.Background())
	if s.calls != 2 {
		t.Fatalf("did not re-fetch after the TTL: %d calls", s.calls)
	}
}

// A GitHub blip must not replace the Home tab with an error. The caller gets a
// renderable Release and the error alongside it.
func TestAFailedLookupStillYieldsARenderableResult(t *testing.T) {
	s := &stubGet{err: errors.New("no network")}
	rel, err := checkerFor(t, "v0.1.0", s).Check(context.Background())
	if err == nil {
		t.Fatal("a failed fetch reported success")
	}
	if rel.Current != "v0.1.0" || rel.Available {
		t.Fatalf("rel = %+v, want the current version and no update", rel)
	}
}

// Once a good answer is known, a later failure falls back to it rather than
// making the update section flicker out of existence.
func TestAFailedLookupFallsBackToTheLastGoodAnswer(t *testing.T) {
	s := &stubGet{bodies: map[string]string{latestURL: releaseJSON("v0.2.0", "")}}
	now := time.Unix(1_700_000_000, 0)
	c := New(Deps{
		Current: "v0.1.0", HTTPGet: s.get, TTL: time.Minute,
		Now: func() time.Time { return now },
	})
	c.Check(context.Background())

	now = now.Add(2 * time.Minute)
	s.err = errors.New("no network")
	rel, err := c.Check(context.Background())
	if err == nil {
		t.Fatal("the failure was swallowed")
	}
	if !rel.Available || rel.Tag != "v0.2.0" {
		t.Fatalf("rel = %+v, want the cached answer", rel)
	}
}

// No HTTP client at all is a configuration, not a failure: the Home tab renders
// as a version line.
func TestNoHTTPClientMeansNoUpdate(t *testing.T) {
	rel, err := New(Deps{Current: "v0.1.0"}).Check(context.Background())
	if err != nil || rel.Available {
		t.Fatalf("rel = %+v, err = %v; want a quiet no-update", rel, err)
	}
}

func TestAssetURLMatchesThePlatformAsset(t *testing.T) {
	const tagURL = "https://api.github.com/repos/miere/riggs/releases/tags/v0.2.0"
	s := &stubGet{bodies: map[string]string{tagURL: `{
		"tag_name":"v0.2.0",
		"assets":[
			{"name":"riggs-v0.2.0-linux-amd64","browser_download_url":"https://example.test/linux"},
			{"name":"riggs-v0.2.0-darwin-arm64","browser_download_url":"https://example.test/mac"}
		]}`}}

	got, err := checkerFor(t, "v0.1.0", s).AssetURL(context.Background(), "v0.2.0", "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetURL: %v", err)
	}
	if got != "https://example.test/mac" {
		t.Fatalf("AssetURL = %q", got)
	}
}

// A release built before an architecture was added to the matrix has no asset
// for it. Saying so beats downloading whatever came first.
func TestAssetURLReportsAMissingPlatform(t *testing.T) {
	const tagURL = "https://api.github.com/repos/miere/riggs/releases/tags/v0.2.0"
	s := &stubGet{bodies: map[string]string{tagURL: `{
		"tag_name":"v0.2.0",
		"assets":[{"name":"riggs-v0.2.0-linux-amd64","browser_download_url":"https://example.test/linux"}]}`}}

	_, err := checkerFor(t, "v0.1.0", s).AssetURL(context.Background(), "v0.2.0", "darwin", "arm64")
	if err == nil {
		t.Fatal("a missing asset was not reported")
	}
}

// The asset name is a contract with .github/workflows/release.yml. If this
// changes, that changes.
func TestAssetNameIsTheReleaseWorkflowsNaming(t *testing.T) {
	if got := AssetName("v0.2.0", "darwin", "arm64"); got != "riggs-v0.2.0-darwin-arm64" {
		t.Fatalf("AssetName = %q", got)
	}
}
