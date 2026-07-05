// Package sqlitestore implements internal/store.Store on SQLite + FTS5, the
// single-file, zero-ops homelab-default persistence backend (see
// docs/design/2026-07-01-nagus-design.md section 8 and section 15 decision 3).
//
// It uses the pure-Go driver modernc.org/sqlite (no cgo), so release binaries
// build without a C toolchain. Semantics MUST match store.MemoryStore, the
// reference implementation: price-bound queries exclude unknown-price (0)
// items, Search returns newest-first by SeenAt with ID as a stable tiebreak,
// Limit 0 means no limit, and an empty Query returns everything.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/store"
)

// Store is a SQLite-backed store.Store implementation.
//
// The underlying *sql.DB pool is capped at a single open connection. This is
// deliberate, not merely a concurrency-safety measure: SQLite's ":memory:"
// DSN gives each connection its own private database, so a pool of more than
// one connection would silently fragment an in-memory Store across
// connections. Capping at one connection makes every DSN (file or memory)
// behave the same and lets database/sql serialize access for us, which is
// sufficient for nagus's read-mostly, single-writer workload.
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// New opens (creating if needed) a SQLite database at dsn and ensures the
// nagus schema exists. dsn may be a file path or ":memory:".
func New(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open: %w", err)
	}
	// See the Store doc comment: one connection keeps ":memory:" DSNs
	// coherent and gives us simple, correct concurrency for free.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitestore: pragma busy_timeout: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id              TEXT PRIMARY KEY,
	category        TEXT NOT NULL,
	class           TEXT NOT NULL,
	title           TEXT NOT NULL,
	canonical_id    TEXT NOT NULL DEFAULT '',
	price_cents     INTEGER NOT NULL DEFAULT 0,
	currency        TEXT NOT NULL DEFAULT '',
	condition       TEXT NOT NULL DEFAULT '',
	source_id       TEXT NOT NULL,
	source_key      TEXT NOT NULL,
	source_url      TEXT NOT NULL DEFAULT '',
	seen_at_ns      INTEGER NOT NULL,
	attributes_json TEXT NOT NULL DEFAULT '{}',
	tokens_json     TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX IF NOT EXISTS idx_items_category ON items(category);
CREATE INDEX IF NOT EXISTS idx_items_class ON items(class);
CREATE INDEX IF NOT EXISTS idx_items_seen_at_ns ON items(seen_at_ns);

CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(
	id UNINDEXED,
	title,
	tokens_text,
	tokenize = 'unicode61'
);
`

// migrate creates the schema if it is absent. It is idempotent.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("sqlitestore: migrate: %w", err)
	}
	return nil
}

// Put validates then stores (insert-or-replace by ID), matching
// store.MemoryStore.Put.
func (s *Store) Put(ctx context.Context, it item.Item) error {
	if err := it.Validate(); err != nil {
		return err
	}

	attrsJSON, err := json.Marshal(it.Attributes)
	if err != nil {
		return fmt.Errorf("sqlitestore: marshal attributes: %w", err)
	}
	tokensJSON, err := json.Marshal(it.Tokens)
	if err != nil {
		return fmt.Errorf("sqlitestore: marshal tokens: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	_, err = tx.ExecContext(ctx, `
		INSERT INTO items (
			id, category, class, title, canonical_id, price_cents, currency,
			condition, source_id, source_key, source_url, seen_at_ns,
			attributes_json, tokens_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			category = excluded.category,
			class = excluded.class,
			title = excluded.title,
			canonical_id = excluded.canonical_id,
			price_cents = excluded.price_cents,
			currency = excluded.currency,
			condition = excluded.condition,
			source_id = excluded.source_id,
			source_key = excluded.source_key,
			source_url = excluded.source_url,
			seen_at_ns = excluded.seen_at_ns,
			attributes_json = excluded.attributes_json,
			tokens_json = excluded.tokens_json
	`,
		it.ID, it.Category, string(it.Class), it.Title, it.CanonicalID,
		it.PriceCents, it.Currency, it.Condition, it.SourceID, it.SourceKey,
		it.SourceURL, it.SeenAt.UTC().UnixNano(), string(attrsJSON), string(tokensJSON),
	)
	if err != nil {
		return fmt.Errorf("sqlitestore: put: %w", err)
	}

	// Keep the FTS index in sync: delete-then-insert since FTS5 has no
	// upsert and id is an external, unindexed key rather than the rowid.
	if _, err := tx.ExecContext(ctx, `DELETE FROM items_fts WHERE id = ?`, it.ID); err != nil {
		return fmt.Errorf("sqlitestore: put: fts delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO items_fts (id, title, tokens_text) VALUES (?, ?, ?)
	`, it.ID, it.Title, strings.Join(it.Tokens, " ")); err != nil {
		return fmt.Errorf("sqlitestore: put: fts insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: commit: %w", err)
	}
	return nil
}

// DeleteStale removes every item from the given source whose SeenAt is strictly
// before olderThan, keeping the FTS mirror in sync, and returns the count
// deleted. Scoped by source so a retention window (e.g. eBay's 6h content-age
// obligation) applies to one source without touching others.
func (s *Store) DeleteStale(ctx context.Context, sourceID string, olderThan time.Time) (int, error) {
	cutoff := olderThan.UTC().UnixNano()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM items_fts WHERE id IN (
			SELECT id FROM items WHERE source_id = ? AND seen_at_ns < ?
		)`, sourceID, cutoff); err != nil {
		return 0, fmt.Errorf("sqlitestore: delete stale fts: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM items WHERE source_id = ? AND seen_at_ns < ?`, sourceID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: delete stale: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlitestore: delete stale rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlitestore: commit: %w", err)
	}
	return int(n), nil
}

// Get returns the item by ID.
func (s *Store) Get(ctx context.Context, id string) (item.Item, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, category, class, title, canonical_id, price_cents, currency,
			condition, source_id, source_key, source_url, seen_at_ns,
			attributes_json, tokens_json
		FROM items WHERE id = ?
	`, id)

	it, err := scanItem(row)
	if err == sql.ErrNoRows {
		return item.Item{}, false, nil
	}
	if err != nil {
		return item.Item{}, false, fmt.Errorf("sqlitestore: get: %w", err)
	}
	return it, true, nil
}

// rowScanner is the subset of *sql.Row / *sql.Rows used by scanItem.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (item.Item, error) {
	var (
		it                    item.Item
		class                 string
		seenAtNS              int64
		attrsJSON, tokensJSON string
	)
	if err := row.Scan(
		&it.ID, &it.Category, &class, &it.Title, &it.CanonicalID, &it.PriceCents,
		&it.Currency, &it.Condition, &it.SourceID, &it.SourceKey, &it.SourceURL,
		&seenAtNS, &attrsJSON, &tokensJSON,
	); err != nil {
		return item.Item{}, err
	}
	it.Class = item.Class(class)
	it.SeenAt = time.Unix(0, seenAtNS).UTC()

	if attrsJSON != "" && attrsJSON != "null" {
		if err := json.Unmarshal([]byte(attrsJSON), &it.Attributes); err != nil {
			return item.Item{}, fmt.Errorf("unmarshal attributes: %w", err)
		}
	}
	if tokensJSON != "" && tokensJSON != "null" {
		if err := json.Unmarshal([]byte(tokensJSON), &it.Tokens); err != nil {
			return item.Item{}, fmt.Errorf("unmarshal tokens: %w", err)
		}
	}
	return it, nil
}

// Search applies the Query predicates and returns matches newest-first
// (SeenAt desc, ID asc tiebreak), matching store.MemoryStore.Search exactly.
//
// Text matching is case-insensitive substring containment over Title and
// Tokens, same as MemoryStore's strings.Contains-based check -- not FTS5's
// tokenized MATCH, whose word-boundary semantics would diverge from the
// reference contract on partial-word queries. The items_fts virtual table is
// still created and kept in sync (see the schema constant) so it is
// available for future ranked full-text work, but Search itself uses SQL
// LIKE against title and the raw tokens JSON blob, which preserves substring
// semantics for token values too.
func (s *Store) Search(ctx context.Context, q store.Query) ([]item.Item, error) {
	var (
		where []string
		args  []any
	)

	if q.Category != "" {
		where = append(where, "category = ?")
		args = append(args, q.Category)
	}
	if q.Class != "" {
		where = append(where, "class = ?")
		args = append(args, string(q.Class))
	}
	if q.MaxPriceCents > 0 {
		// Unknown-price (0) items are excluded from a price-bounded query,
		// same as MemoryStore.
		where = append(where, "price_cents > 0 AND price_cents <= ?")
		args = append(args, q.MaxPriceCents)
	}
	if !q.Since.IsZero() {
		where = append(where, "seen_at_ns >= ?")
		args = append(args, q.Since.UTC().UnixNano())
	}
	if q.Text != "" {
		needle := "%" + escapeLike(strings.ToLower(q.Text)) + "%"
		where = append(where, "(LOWER(title) LIKE ? ESCAPE '\\' OR LOWER(tokens_json) LIKE ? ESCAPE '\\')")
		args = append(args, needle, needle)
	}

	query := `
		SELECT id, category, class, title, canonical_id, price_cents, currency,
			condition, source_id, source_key, source_url, seen_at_ns,
			attributes_json, tokens_json
		FROM items
	`
	if len(where) > 0 {
		query += "WHERE " + strings.Join(where, " AND ") + " "
	}
	query += "ORDER BY seen_at_ns DESC, id ASC"
	if q.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, q.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: search: %w", err)
	}
	defer rows.Close()

	out := make([]item.Item, 0)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlitestore: search: scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlitestore: search: rows: %w", err)
	}
	return out, nil
}

// escapeLike escapes SQL LIKE metacharacters (% _ \) in a user-supplied
// substring so it is matched literally.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}
