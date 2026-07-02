// Package postgresstore implements internal/store.Store on PostgreSQL, for the
// shared CloudNativePG cluster (see docs/design/2026-07-01-nagus-design.md
// section 8 and section 15 decision 3).
//
// v1 is FTS/plain-text only: the shared CNPG image does not carry the
// pgvector extension, so semantic/vector search is explicitly deferred here.
// A generated tsvector column is created for future ranked full-text work,
// but -- exactly like internal/store/sqlitestore's items_fts virtual table --
// it is NOT used by Search and is not load-bearing for correctness. Search's
// Query.Text predicate must match store.MemoryStore.textMatch exactly, which
// is strings.Contains SUBSTRING matching, not tokenized full-text search; a
// tsvector @@ to_tsquery match would diverge on partial-word queries (e.g. a
// substring like "acre" inside "acres" or "50%"), so Text is implemented as
// case-insensitive ILIKE '%needle%' (with % _ \ escaped) over the title and
// over the tokens JSONB column's text form, mirroring sqlitestore's LIKE
// decision precisely.
package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/store"
)

// Store is a PostgreSQL-backed store.Store implementation using pgx/pgxpool.
// pgxpool.Pool is safe for concurrent use by multiple goroutines, and Store
// holds no other mutable state, so Store is likewise safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// New opens a pgxpool against dsn and ensures the nagus schema exists.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgresstore: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgresstore: ping: %w", err)
	}

	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
	id              TEXT PRIMARY KEY,
	category        TEXT NOT NULL,
	class           TEXT NOT NULL,
	title           TEXT NOT NULL,
	canonical_id    TEXT NOT NULL DEFAULT '',
	price_cents     BIGINT NOT NULL DEFAULT 0,
	currency        TEXT NOT NULL DEFAULT '',
	condition       TEXT NOT NULL DEFAULT '',
	source_id       TEXT NOT NULL,
	source_key      TEXT NOT NULL,
	source_url      TEXT NOT NULL DEFAULT '',
	seen_at         TIMESTAMPTZ NOT NULL,
	-- Nullable: item.Item's Attributes/Tokens are Go nil-able (map/slice), and
	-- pgx encodes a nil map/slice as SQL NULL (not JSON 'null'), which then
	-- round-trips back to nil on scan -- matching item.Item's zero value
	-- exactly. DEFAULT is documentation only; Put always supplies a value.
	attributes      JSONB DEFAULT '{}',
	tokens          JSONB DEFAULT '[]',
	search_tsv      tsvector GENERATED ALWAYS AS (
		to_tsvector('simple', coalesce(title, ''))
	) STORED
);

CREATE INDEX IF NOT EXISTS idx_items_category ON items (category);
CREATE INDEX IF NOT EXISTS idx_items_class ON items (class);
CREATE INDEX IF NOT EXISTS idx_items_seen_at ON items (seen_at);

-- Not load-bearing for Query.Text (see package doc); reserved for future
-- ranked full-text search.
CREATE INDEX IF NOT EXISTS idx_items_search_tsv ON items USING GIN (search_tsv);
`

// migrate creates the schema if it is absent. It is idempotent.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("postgresstore: migrate: %w", err)
	}
	return nil
}

const upsertSQL = `
INSERT INTO items (
	id, category, class, title, canonical_id, price_cents, currency,
	condition, source_id, source_key, source_url, seen_at, attributes, tokens
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (id) DO UPDATE SET
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
	seen_at = excluded.seen_at,
	attributes = excluded.attributes,
	tokens = excluded.tokens
`

// Put validates then stores (insert-or-replace by ID), matching
// store.MemoryStore.Put.
func (s *Store) Put(ctx context.Context, it item.Item) error {
	if err := it.Validate(); err != nil {
		return err
	}

	_, err := s.pool.Exec(ctx, upsertSQL,
		it.ID, it.Category, string(it.Class), it.Title, it.CanonicalID,
		it.PriceCents, it.Currency, it.Condition, it.SourceID, it.SourceKey,
		it.SourceURL, it.SeenAt.UTC(), it.Attributes, it.Tokens,
	)
	if err != nil {
		return fmt.Errorf("postgresstore: put: %w", err)
	}
	return nil
}

const selectColumns = `
	id, category, class, title, canonical_id, price_cents, currency,
	condition, source_id, source_key, source_url, seen_at, attributes, tokens
`

// Get returns the item by ID.
func (s *Store) Get(ctx context.Context, id string) (item.Item, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+selectColumns+" FROM items WHERE id = $1", id)

	it, err := scanItem(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return item.Item{}, false, nil
	}
	if err != nil {
		return item.Item{}, false, fmt.Errorf("postgresstore: get: %w", err)
	}
	return it, true, nil
}

// rowScanner is the subset of pgx.Row / pgx.Rows used by scanItem.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (item.Item, error) {
	var (
		it    item.Item
		class string
	)
	if err := row.Scan(
		&it.ID, &it.Category, &class, &it.Title, &it.CanonicalID, &it.PriceCents,
		&it.Currency, &it.Condition, &it.SourceID, &it.SourceKey, &it.SourceURL,
		&it.SeenAt, &it.Attributes, &it.Tokens,
	); err != nil {
		return item.Item{}, err
	}
	it.Class = item.Class(class)
	it.SeenAt = it.SeenAt.UTC()
	return it, nil
}

// Search applies the Query predicates and returns matches newest-first
// (SeenAt desc, ID asc tiebreak), matching store.MemoryStore.Search exactly.
//
// Text matching is case-insensitive substring containment over Title and
// Tokens via ILIKE, NOT the generated search_tsv column's tokenized
// to_tsquery matching -- see the package doc comment for why: MemoryStore's
// textMatch is strings.Contains, and a tokenized match would diverge from
// that contract on partial-word queries.
func (s *Store) Search(ctx context.Context, q store.Query) ([]item.Item, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if q.Category != "" {
		where = append(where, "category = "+arg(q.Category))
	}
	if q.Class != "" {
		where = append(where, "class = "+arg(string(q.Class)))
	}
	if q.MaxPriceCents > 0 {
		// Unknown-price (0) items are excluded from a price-bounded query,
		// same as MemoryStore.
		where = append(where, fmt.Sprintf("(price_cents > 0 AND price_cents <= %s)", arg(q.MaxPriceCents)))
	}
	if !q.Since.IsZero() {
		where = append(where, "seen_at >= "+arg(q.Since.UTC()))
	}
	if q.Text != "" {
		needle := "%" + escapeLike(q.Text) + "%"
		titlePH := arg(needle)
		tokensPH := arg(needle)
		where = append(where, fmt.Sprintf(
			"(title ILIKE %s ESCAPE '\\' OR tokens::text ILIKE %s ESCAPE '\\')",
			titlePH, tokensPH,
		))
	}

	query := "SELECT " + selectColumns + " FROM items"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY seen_at DESC, id ASC"
	if q.Limit > 0 {
		query += " LIMIT " + arg(q.Limit)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgresstore: search: %w", err)
	}
	defer rows.Close()

	out := make([]item.Item, 0)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("postgresstore: search: scan: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresstore: search: rows: %w", err)
	}
	return out, nil
}

// escapeLike escapes LIKE/ILIKE metacharacters (% _ \) in a user-supplied
// substring so it is matched literally.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	return s
}
