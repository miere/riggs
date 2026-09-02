// Package notify owns everything Riggs has already said: which card is where,
// what it looked like, and which one-off messages have already fired.
//
// Every notification is stateful. A reply can only be threaded onto a message
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
	"strings"
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
	// State is an opaque domain label for what the card last showed (for the
	// review queue, the derived reason). The ledger never interprets it; it
	// exists so a caller can decide whether a card is worth re-fetching at all
	// without re-deriving it from the upstream API.
	State     string
	UpdatedAt time.Time
}

// KeyedEntry is an Entry with its key, for enumeration.
type KeyedEntry struct {
	Key string
	Entry
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

-- latches and summaries are vestigial. The reviewer tag that wrote a latch and
-- the LLM summary that wrote a summary both belonged to the per-item card loop,
-- which is gone (§9c); ClearLatches still deletes from latches on a repost, and
-- nothing writes either table any more.
--
-- The CREATE statements stay. Dropping a table needs a migration on every
-- existing ledger, and buys nothing: an empty table costs a page.
CREATE TABLE IF NOT EXISTS latches (
	key        TEXT NOT NULL,
	name       TEXT NOT NULL,
	fired_at   TEXT NOT NULL,
	PRIMARY KEY (key, name)
);

CREATE TABLE IF NOT EXISTS summaries (
	key        TEXT PRIMARY KEY,
	text       TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS http_cache (
	url        TEXT PRIMARY KEY,
	etag       TEXT NOT NULL,
	body       BLOB NOT NULL,
	updated_at TEXT NOT NULL
);

-- items belong to a bulk digest (§9b). A row in cards is one message about one
-- entity; a digest is one message about many, so the membership that cards
-- never needed lives here: which post an item is currently shown in, and when
-- it last entered a new one.
CREATE TABLE IF NOT EXISTS items (
	key        TEXT PRIMARY KEY,
	stream     TEXT NOT NULL,
	post_key   TEXT NOT NULL,
	position   INTEGER NOT NULL,
	status     TEXT NOT NULL,
	done       INTEGER NOT NULL DEFAULT 0,
	posted_at  TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS items_post   ON items(post_key);
CREATE INDEX IF NOT EXISTS items_stream ON items(stream);

-- jobs are Riggs' own schedule, which Murtaugh used to own.
--
-- They live in the ledger rather than in config.yaml because they are not a
-- setting: half of every row is what HAPPENED — when it last ran, for how long,
-- whether it worked — and that has no business in a hand-edited file that a
-- human is expected to read. The definition and its outcome are one record
-- because the Home tab shows them as one line.
CREATE TABLE IF NOT EXISTS jobs (
	name        TEXT PRIMARY KEY,
	args        TEXT NOT NULL,
	spec        TEXT NOT NULL,
	timeout_ms  INTEGER NOT NULL,
	enabled     INTEGER NOT NULL DEFAULT 1,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	last_run_at TEXT NOT NULL DEFAULT '',
	last_ok     INTEGER NOT NULL DEFAULT 0,
	last_ms     INTEGER NOT NULL DEFAULT 0,
	last_error  TEXT NOT NULL DEFAULT '',
	last_output TEXT NOT NULL DEFAULT ''
);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("notify: applying schema: %w", err)
	}
	// Columns added after the first release. CREATE TABLE IF NOT EXISTS will
	// not add them to an existing table, and a duplicate-column error just
	// means this ledger already has it.
	for _, alter := range []string{
		`ALTER TABLE cards ADD COLUMN state TEXT NOT NULL DEFAULT ''`,
		// A digest row must be renderable from the ledger alone. Without these
		// a rebuild that had no fresh upstream read fell back to the bare
		// reference — losing the title and, worse, the link.
		`ALTER TABLE items ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE items ADD COLUMN author TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE items ADD COLUMN url TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.ExecContext(ctx, alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("notify: %s: %w", alter, err)
		}
	}
	return nil
}

// Card returns the tracked card for key, if there is one.
func (s *Store) Card(ctx context.Context, key string) (Entry, bool, error) {
	var e Entry
	var updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT profile, channel, ts, fingerprint, state, updated_at FROM cards WHERE key = ?`, key,
	).Scan(&e.Profile, &e.Channel, &e.TS, &e.Fingerprint, &e.State, &updated)
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
		`INSERT INTO cards (key, profile, channel, ts, fingerprint, state, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   profile = excluded.profile, channel = excluded.channel, ts = excluded.ts,
		   fingerprint = excluded.fingerprint, state = excluded.state,
		   updated_at = excluded.updated_at`,
		key, e.Profile, e.Channel, e.TS, e.Fingerprint, e.State, e.UpdatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("notify: saving card %s: %w", key, err)
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

// CardsWithPrefix returns every tracked card whose key starts with prefix, in
// key order. The review queue uses it to re-include cards it is still
// responsible for finalising, without re-deriving each one first.
func (s *Store) CardsWithPrefix(ctx context.Context, prefix string) ([]KeyedEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, profile, channel, ts, fingerprint, state, updated_at
		 FROM cards WHERE key LIKE ? ORDER BY key`, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("notify: listing cards under %q: %w", prefix, err)
	}
	defer rows.Close()

	var out []KeyedEntry
	for rows.Next() {
		var ke KeyedEntry
		var updated string
		if err := rows.Scan(&ke.Key, &ke.Profile, &ke.Channel, &ke.TS,
			&ke.Fingerprint, &ke.State, &updated); err != nil {
			return nil, fmt.Errorf("notify: scanning cards: %w", err)
		}
		ke.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		out = append(out, ke)
	}
	return out, rows.Err()
}
