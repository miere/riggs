package github

import (
	"context"
	"net/http"
	"time"
)

// Cache stores conditional-request state. notify.Store satisfies it.
//
// The body is stored with the ETag because a 304 carries no payload: without
// the cached body a conditional request would save quota and yield nothing.
type Cache interface {
	CachedResponse(ctx context.Context, url string) (etag string, body []byte, ok bool, err error)
	SaveResponse(ctx context.Context, url, etag string, body []byte, at time.Time) error
}

// WithCache enables conditional requests. Without one the client still works;
// it just pays full price for every read.
//
// This is the mechanism the whole GitHub decision rests on (ARCHITECTURE.md
// §8): a 304 does not count against the rate limit, and the steady state of
// the review-queue loop is "nothing changed".
func (c *Client) WithCache(cache Cache, now func() time.Time) *Client {
	c.cache, c.now = cache, now
	return c
}

// Stats reports what the conditional requests bought, for the tool's result.
type Stats struct {
	// Requests is every HTTP call made.
	Requests int `json:"requests"`
	// NotModified counts the 304s — the ones that cost no quota.
	NotModified int `json:"not_modified"`
	// GraphQL counts the point-billed queries (reviewDecision only).
	GraphQL int `json:"graphql"`
}

// Stats returns the counters accumulated by this client.
func (c *Client) Stats() Stats { return c.stats }

// conditional adds If-None-Match when a cached ETag exists, and reports it so
// the caller can recognise a 304 as a cache hit rather than an empty result.
func (c *Client) conditional(ctx context.Context, req *http.Request, url string) (cachedBody []byte, hasCache bool) {
	if c.cache == nil {
		return nil, false
	}
	etag, body, ok, err := c.cache.CachedResponse(ctx, url)
	if err != nil || !ok || etag == "" {
		// A broken cache must never break a read: fall through to a full GET.
		return nil, false
	}
	req.Header.Set("If-None-Match", etag)
	return body, true
}

// remember stores a fresh response for the next conditional request.
func (c *Client) remember(ctx context.Context, url, etag string, body []byte) {
	if c.cache == nil || etag == "" {
		return
	}
	at := time.Now()
	if c.now != nil {
		at = c.now()
	}
	// A cache write failure is not worth failing a read over; the next tick
	// simply pays full price.
	_ = c.cache.SaveResponse(ctx, url, etag, body, at)
}
