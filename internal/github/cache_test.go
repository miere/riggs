package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// memCache is an in-memory Cache.
type memCache struct {
	etags   map[string]string
	bodies  map[string][]byte
	readErr error
	saveErr error
	saves   int
}

func newMemCache() *memCache {
	return &memCache{etags: map[string]string{}, bodies: map[string][]byte{}}
}

func (m *memCache) CachedResponse(_ context.Context, url string) (string, []byte, bool, error) {
	if m.readErr != nil {
		return "", nil, false, m.readErr
	}
	e, ok := m.etags[url]
	if !ok {
		return "", nil, false, nil
	}
	return e, m.bodies[url], true, nil
}

func (m *memCache) SaveResponse(_ context.Context, url, etag string, body []byte, _ time.Time) error {
	m.saves++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.etags[url], m.bodies[url] = etag, body
	return nil
}

// searchServer serves the review-requested search, honouring If-None-Match.
func searchServer(t *testing.T, etag string, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		io.WriteString(w, `{"items":[{"number":1,"title":"T","html_url":"u",
			"repository_url":"https://api.github.com/repos/o/r","user":{"login":"a"}}]}`)
	}))
}

// The behaviour the whole GitHub decision rests on: the second read is a 304,
// which costs no rate-limit quota, and still yields the data.
func TestConditionalRequestServesFromCache(t *testing.T) {
	var seen []string
	srv := searchServer(t, `W/"abc"`, &seen)
	defer srv.Close()

	cache := newMemCache()
	c := New("t").WithTransport(srv.Client(), srv.URL).WithCache(cache, time.Now)
	ctx := context.Background()

	first, err := c.ReviewRequested(ctx, "miere", 1)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := c.ReviewRequested(ctx, "miere", 1)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if len(first) != 1 || len(second) != 1 || first[0].Ref() != second[0].Ref() {
		t.Errorf("results differ: %+v vs %+v", first, second)
	}
	if seen[0] != "" {
		t.Errorf("first request sent If-None-Match %q, want none", seen[0])
	}
	if seen[1] != `W/"abc"` {
		t.Errorf("second request sent If-None-Match %q, want the stored etag", seen[1])
	}
	if got := c.Stats(); got.Requests != 2 || got.NotModified != 1 {
		t.Errorf("stats = %+v, want 2 requests of which 1 was a 304", got)
	}
}

// Without a cache the client still works; it just pays full price.
func TestWithoutCacheNoConditionalHeader(t *testing.T) {
	var seen []string
	srv := searchServer(t, `W/"abc"`, &seen)
	defer srv.Close()

	c := New("t").WithTransport(srv.Client(), srv.URL)
	ctx := context.Background()
	if _, err := c.ReviewRequested(ctx, "miere", 1); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := c.ReviewRequested(ctx, "miere", 1); err != nil {
		t.Fatalf("second: %v", err)
	}
	for i, h := range seen {
		if h != "" {
			t.Errorf("request %d sent If-None-Match %q with no cache configured", i, h)
		}
	}
	if got := c.Stats(); got.NotModified != 0 {
		t.Errorf("stats = %+v, want no 304s", got)
	}
}

// A broken cache must never break a read: it degrades to a full request.
func TestUnreadableCacheFallsThroughToAFullGet(t *testing.T) {
	var seen []string
	srv := searchServer(t, `W/"abc"`, &seen)
	defer srv.Close()

	cache := newMemCache()
	cache.readErr = errors.New("ledger is locked")
	c := New("t").WithTransport(srv.Client(), srv.URL).WithCache(cache, time.Now)

	if _, err := c.ReviewRequested(context.Background(), "miere", 1); err != nil {
		t.Fatalf("ReviewRequested: %v", err)
	}
	if seen[0] != "" {
		t.Errorf("sent If-None-Match %q despite an unreadable cache", seen[0])
	}
}

// Nor must an unwritable one.
func TestUnwritableCacheDoesNotFailTheRead(t *testing.T) {
	var seen []string
	srv := searchServer(t, `W/"abc"`, &seen)
	defer srv.Close()

	cache := newMemCache()
	cache.saveErr = errors.New("disk full")
	c := New("t").WithTransport(srv.Client(), srv.URL).WithCache(cache, time.Now)

	if _, err := c.ReviewRequested(context.Background(), "miere", 1); err != nil {
		t.Fatalf("ReviewRequested: %v", err)
	}
}

// A 304 we cannot satisfy from the cache is a bookkeeping bug, and returning
// an empty result would look like "no PRs awaiting review".
func TestUnexpectedNotModifiedIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := New("t").WithTransport(srv.Client(), srv.URL)
	_, err := c.ReviewRequested(context.Background(), "miere", 1)
	if err == nil {
		t.Fatal("a 304 with nothing cached returned no error")
	}
}

// A response with no ETag is simply not cached, rather than cached under an
// empty key that would then be sent back as a header.
func TestResponseWithoutETagIsNotCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()

	cache := newMemCache()
	c := New("t").WithTransport(srv.Client(), srv.URL).WithCache(cache, time.Now)
	if _, err := c.ReviewRequested(context.Background(), "miere", 1); err != nil {
		t.Fatalf("ReviewRequested: %v", err)
	}
	if cache.saves != 0 {
		t.Errorf("cache saves = %d, want none for a response with no ETag", cache.saves)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New("t").WithTransport(srv.Client(), srv.URL)
	_, err := c.PullRequestDetail(context.Background(), "o/r", 1)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
