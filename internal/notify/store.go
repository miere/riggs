// Package notify owns everything Riggs has already said: which card is where,
// what it looked like, and which one-off messages have already fired.
//
// Every notification is stateful. A nudge can only be threaded onto a message
// that was already posted, and a card must be updated in place rather than
// re-announced — so "post", "update" and "thread" are one mechanism, not three
// programs. That mechanism is Notifier (see notify.go); this file is the
// storage under it.
package notify

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays portable
)

// Entry is one tracked card.
type Entry struct {
	// Profile and Channel record where the card was posted, so a later update
	// reaches the same place even if the caller's defaults have changed.
	Profile string
	Channel string
	// TS is the Slack message timestamp: the card's identity and its thread
	// anchor.
	TS string
	// Fingerprint is the digest of the card as last rendered. An update is
	// skipped when it has not changed.
	Fingerprint string
	UpdatedAt   time.Time
}

// Store is the SQLite-backed ledger.
//
// SQLite rather than a JSON file because the every-1m reconcile and a button
// press are separate processes that can now write the same card. The Python's
// read-modify-write JSON has no answer for that; here the database does the
// arbitration.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the ledger at path, and applies the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("notify: creating %s: %w", dir, err)
		}
	}
	// WAL lets a reader (the reconcile loop) and a writer (a button press)
	// proceed concurrently; busy_timeout makes a contended write wait rather
	// than fail the tick.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("notify: opening %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS cards (
	key         TEXT PRIMARY KEY,
	profile     TEXT NOT NULL,
	channel     TEXT NOT NULL,
	ts          TEXT NOT NULL,
	fingerprint TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS latches (
	key        TEXT NOT NULL,
	name       TEXT NOT NULL,
	fired_at   TEXT NOT NULL,
	PRIMARY KEY (key, name)
);

CREATE TABLE IF NOT EXISTS http_cache (
	url        TEXT PRIMARY KEY,
	etag       TEXT NOT NULL,
	body       BLOB NOT NULL,
	updated_at TEXT NOT NULL
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("notify: applying schema: %w", err)
	}
	return nil
}

// Card returns the tracked card for key, if there is one.
func (s *Store) Card(ctx context.Context, key string) (Entry, bool, error) {
	var e Entry
	var updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT profile, channel, ts, fingerprint, updated_at FROM cards WHERE key = ?`, key,
	).Scan(&e.Profile, &e.Channel, &e.TS, &e.Fingerprint, &updated)
	if err == sql.ErrNoRows {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("notify: reading card %s: %w", key, err)
	}
	e.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return e, true, nil
}

// SaveCard inserts or replaces the tracked card for key.
func (s *Store) SaveCard(ctx context.Context, key string, e Entry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cards (key, profile, channel, ts, fingerprint, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   profile = excluded.profile, channel = excluded.channel, ts = excluded.ts,
		   fingerprint = excluded.fingerprint, updated_at = excluded.updated_at`,
		key, e.Profile, e.Channel, e.TS, e.Fingerprint, e.UpdatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("notify: saving card %s: %w", key, err)
	}
	return nil
}

// LatchFiredAt reports when the named latch last fired for key.
func (s *Store) LatchFiredAt(ctx context.Context, key, name string) (time.Time, bool, error) {
	var fired string
	err := s.db.QueryRowContext(ctx,
		`SELECT fired_at FROM latches WHERE key = ? AND name = ?`, key, name).Scan(&fired)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("notify: reading latch %s/%s: %w", key, name, err)
	}
	t, err := time.Parse(time.RFC3339, fired)
	if err != nil {
		// An unparseable timestamp means "it fired, at an unknown time" —
		// which is the safe reading: a once-latch stays closed, and a min-gap
		// latch is allowed to fire again.
		return time.Time{}, true, nil
	}
	return t, true, nil
}

// SetLatch records the named latch as having fired at t.
func (s *Store) SetLatch(ctx context.Context, key, name string, t time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO latches (key, name, fired_at) VALUES (?, ?, ?)
		 ON CONFLICT(key, name) DO UPDATE SET fired_at = excluded.fired_at`,
		key, name, t.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("notify: setting latch %s/%s: %w", key, name, err)
	}
	return nil
}

// ClearLatch forgets the named latch, so a once-per-episode message can fire
// again on the next episode.
func (s *Store) ClearLatch(ctx context.Context, key, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM latches WHERE key = ? AND name = ?`, key, name)
	if err != nil {
		return fmt.Errorf("notify: clearing latch %s/%s: %w", key, name, err)
	}
	return nil
}

// ClearLatches forgets every latch for key. Used when a card is re-posted: the
// new message has none of the old one's threaded replies, so anything
// "already said" must be said again.
func (s *Store) ClearLatches(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM latches WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("notify: clearing latches for %s: %w", key, err)
	}
	return nil
}

// CachedResponse returns the stored ETag and body for url.
//
// The body is stored alongside the ETag on purpose: a 304 carries no payload,
// so without the cached body a conditional request saves quota but yields
// nothing to work with.
func (s *Store) CachedResponse(ctx context.Context, url string) (etag string, body []byte, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT etag, body FROM http_cache WHERE url = ?`, url).
		Scan(&etag, &body)
	if err == sql.ErrNoRows {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("notify: reading cache for %s: %w", url, err)
	}
	return etag, body, true, nil
}

// SaveResponse stores the ETag and body for url.
func (s *Store) SaveResponse(ctx context.Context, url, etag string, body []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO http_cache (url, etag, body, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(url) DO UPDATE SET
		   etag = excluded.etag, body = excluded.body, updated_at = excluded.updated_at`,
		url, etag, body, at.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("notify: saving cache for %s: %w", url, err)
	}
	return nil
}

// CountCards reports how many cards the ledger is tracking. It exists for
// `riggs capabilities`, so an operator can see the ledger is live without
// reading it with a SQLite client.
func (s *Store) CountCards(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cards`).Scan(&n); err != nil {
		return 0, fmt.Errorf("notify: counting cards: %w", err)
	}
	return n, nil
}
