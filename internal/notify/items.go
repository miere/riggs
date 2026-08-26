package notify

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Item is one entry in a bulk digest, and where it currently lives.
//
// The ledger's original unit is a *card*: one message about one entity, keyed
// by that entity forever. A digest breaks that assumption — one message carries
// many items, and which items it carries changes underneath it. So membership,
// which `cards` never needed, is recorded here.
type Item struct {
	// Stream groups items belonging to the same digest family ("git.pr.bulk"),
	// so one domain's items can be enumerated without colliding with another's.
	Stream string
	// PostKey is the `cards` key of the message this item is currently shown
	// in. It is what lets a later pass find and rewrite the right message.
	PostKey string
	// Position is the item's index within that message, so a rebuild preserves
	// the order the reader last saw.
	Position int
	// Status is the opaque domain label for what the row last said. The ledger
	// never interprets it.
	Status string
	// Done marks a row that is struck through: reviewed, merged, closed or
	// stale. A done item never rotates into a new post.
	Done bool
	// PostedAt is the cooldown anchor: when this item last entered a *new*
	// post.
	//
	// An in-place status refresh deliberately does not move it. Otherwise a
	// busy pull request — whose checks flip red and green all morning — would
	// keep resetting its own clock and could never age out of the message it
	// was first announced in.
	PostedAt time.Time
	// UpdatedAt is the last write of any kind, for diagnostics.
	UpdatedAt time.Time
}

// KeyedItem is an Item with its key, for enumeration.
type KeyedItem struct {
	Key string
	Item
}

// Cooled reports whether item's cooldown has elapsed by now.
func (i Item) Cooled(now time.Time, cooldown time.Duration) bool {
	return !now.Before(i.PostedAt.Add(cooldown))
}

// Item returns the tracked item for key, if there is one.
func (s *Store) Item(ctx context.Context, key string) (Item, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT stream, post_key, position, status, done, posted_at, updated_at
		   FROM items WHERE key = ?`, key)
	it, err := scanItem(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("notify: reading item %s: %w", key, err)
	}
	return it, true, nil
}

// SaveItem inserts or replaces the tracked item for key.
func (s *Store) SaveItem(ctx context.Context, key string, it Item) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO items (key, stream, post_key, position, status, done, posted_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET
		   stream = excluded.stream, post_key = excluded.post_key,
		   position = excluded.position, status = excluded.status,
		   done = excluded.done, posted_at = excluded.posted_at,
		   updated_at = excluded.updated_at`,
		key, it.Stream, it.PostKey, it.Position, it.Status, boolToInt(it.Done),
		it.PostedAt.UTC().Format(time.RFC3339), it.UpdatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("notify: saving item %s: %w", key, err)
	}
	return nil
}

// DeleteItem forgets an item entirely.
func (s *Store) DeleteItem(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM items WHERE key = ?`, key); err != nil {
		return fmt.Errorf("notify: deleting item %s: %w", key, err)
	}
	return nil
}

// ItemsInStream lists every tracked item in a digest family, ordered by the
// post they live in and their position within it — so a caller rebuilding
// several messages gets each one's rows already in reading order.
func (s *Store) ItemsInStream(ctx context.Context, stream string) ([]KeyedItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, stream, post_key, position, status, done, posted_at, updated_at
		   FROM items WHERE stream = ? ORDER BY post_key, position`, stream)
	if err != nil {
		return nil, fmt.Errorf("notify: listing items in %s: %w", stream, err)
	}
	defer rows.Close()

	var out []KeyedItem
	for rows.Next() {
		var key string
		var it Item
		var done int
		var postedAt, updatedAt string
		if err := rows.Scan(&key, &it.Stream, &it.PostKey, &it.Position, &it.Status,
			&done, &postedAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("notify: scanning items in %s: %w", stream, err)
		}
		it.Done = done != 0
		it.PostedAt, _ = time.Parse(time.RFC3339, postedAt)
		it.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
		out = append(out, KeyedItem{Key: key, Item: it})
	}
	return out, rows.Err()
}

// DeleteCard forgets a post. Used when a digest empties out and its message is
// deleted: leaving the row behind would have the next pass try to update a
// message that is no longer there.
func (s *Store) DeleteCard(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cards WHERE key = ?`, key); err != nil {
		return fmt.Errorf("notify: deleting card %s: %w", key, err)
	}
	return nil
}

// NextPostKey allocates the next key in a digest family: "<prefix>1",
// "<prefix>2", and so on.
//
// A counter rather than a timestamp, because two passes in the same second must
// not collide, and because a test that freezes the clock still needs distinct
// posts. The maximum is read from the cards actually present, so keys are
// reused once old posts are deleted — which is fine: nothing outside the ledger
// refers to them.
func (s *Store) NextPostKey(ctx context.Context, prefix string) (string, error) {
	entries, err := s.CardsWithPrefix(ctx, prefix)
	if err != nil {
		return "", err
	}
	highest := 0
	for _, e := range entries {
		n, err := strconv.Atoi(strings.TrimPrefix(e.Key, prefix))
		if err == nil && n > highest {
			highest = n
		}
	}
	return prefix + strconv.Itoa(highest+1), nil
}

// GroupByPost reduces a flat item list to the posts they belong to, each one's
// rows in position order. Post keys come back sorted, so a rebuild touches
// messages in a stable order.
func GroupByPost(items []KeyedItem) (postKeys []string, byPost map[string][]KeyedItem) {
	byPost = map[string][]KeyedItem{}
	for _, it := range items {
		byPost[it.PostKey] = append(byPost[it.PostKey], it)
	}
	for key, group := range byPost {
		sort.SliceStable(group, func(i, j int) bool { return group[i].Position < group[j].Position })
		byPost[key] = group
		postKeys = append(postKeys, key)
	}
	sort.Strings(postKeys)
	return postKeys, byPost
}

// scanItem reads one item row.
func scanItem(row *sql.Row) (Item, error) {
	var it Item
	var done int
	var postedAt, updatedAt string
	if err := row.Scan(&it.Stream, &it.PostKey, &it.Position, &it.Status,
		&done, &postedAt, &updatedAt); err != nil {
		return Item{}, err
	}
	it.Done = done != 0
	it.PostedAt, _ = time.Parse(time.RFC3339, postedAt)
	it.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return it, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
