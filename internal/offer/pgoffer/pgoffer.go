// Package pgoffer is the PostgreSQL-backed offer.Store adapter.
//
// It is correct when it passes internal/offer/offerstoretest -- the same
// contract MemoryStore and the SQLite adapter pass. The suite is the
// specification; this is one more implementation of it.
//
// # Why this exists alongside the SQLite adapter
//
// Offers and items are PEER stores and should have the same adapter matrix.
// Without this, enabling the offer layer would pin a deployment to SQLite while
// items moved to Postgres -- the same split, just inverted. Both stores now
// offer memory / sqlite / postgres.
//
// # One database, separate tables
//
// In Postgres the offer layer is NOT a separate database. It is its own table
// set in the same database as items, loosely related to them. The SQLite
// adapter's separate-file arrangement was a workaround for SQLite-specific
// constraints (a VACUUM takes a database-wide lock; a single-writer pool
// serialises every table) and neither applies here: Postgres autovacuums per
// table and pgxpool is concurrent by design. Keeping them in one database also
// makes backup and restore one unit, so item and offer state cannot be restored
// out of step with each other.
package pgoffer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leftathome/nagus/internal/offer"
)

// Store is a PostgreSQL-backed offer.Store. pgxpool.Pool is safe for concurrent
// use, which matters: nagus runs one ingest goroutine per source, all writing
// here at once.
type Store struct {
	pool *pgxpool.Pool
}

var _ offer.Store = (*Store)(nil)

const schema = `
CREATE TABLE IF NOT EXISTS offers (
	id               TEXT PRIMARY KEY,
	source_id        TEXT NOT NULL,
	source_key       TEXT NOT NULL,
	source_url       TEXT NOT NULL DEFAULT '',
	title            TEXT NOT NULL DEFAULT '',
	body             TEXT NOT NULL DEFAULT '',
	price_cents      BIGINT NOT NULL DEFAULT 0,
	currency         TEXT NOT NULL DEFAULT '',
	condition        TEXT NOT NULL DEFAULT '',
	seller           TEXT NOT NULL DEFAULT '',
	aspects_json     TEXT NOT NULL DEFAULT '{}',
	provisional_key  TEXT NOT NULL DEFAULT '',
	hint_brand       TEXT NOT NULL DEFAULT '',
	hint_mpn         TEXT NOT NULL DEFAULT '',
	hint_gtin        TEXT NOT NULL DEFAULT '',
	hint_model       TEXT NOT NULL DEFAULT '',
	first_seen_ns    BIGINT NOT NULL DEFAULT 0,
	last_seen_ns     BIGINT NOT NULL DEFAULT 0,
	min_price_cents  BIGINT NOT NULL DEFAULT 0,
	status           TEXT NOT NULL DEFAULT 'active',
	outcome          TEXT NOT NULL DEFAULT '',
	expired_at_ns    BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_offers_source ON offers(source_id);
CREATE INDEX IF NOT EXISTS idx_offers_status ON offers(status);
CREATE INDEX IF NOT EXISTS idx_offers_last_seen ON offers(last_seen_ns);
-- The dedup path: "every offer for this product across sellers".
CREATE INDEX IF NOT EXISTS idx_offers_provisional_key ON offers(provisional_key);
`

// New opens a pgxpool against dsn and ensures the offer schema exists.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgoffer: open pool: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgoffer: schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Put inserts or updates an offer, folding lifecycle against the stored row.
//
// The fold is expressed IN SQL rather than as a read-then-write, so concurrent
// ingests of the same offer cannot interleave and lose a FirstSeen or a
// MinPriceSeen. GREATEST/LEAST do the work the SQLite adapter does in a
// transaction:
//   - first_seen: the EARLIEST of stored and incoming (never resets)
//   - last_seen:  the LATEST (only advances, so a late write cannot make a live
//     offer look stale and get expired by the next housekeeping pass)
//   - min_price:  the LOWEST ever seen, ignoring 0 which means unknown, not free
//     -- this is what keeps an ended discount visible
func (s *Store) Put(ctx context.Context, o offer.Offer) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if o.ID == "" {
		o.ID = offer.DeterministicID(o.SourceID, o.SourceKey)
	}
	if o.Status == "" {
		o.Status = offer.StatusActive
	}
	if o.FirstSeen.IsZero() {
		o.FirstSeen = o.LastSeen
	}
	if o.MinPriceSeen == 0 || (o.PriceCents > 0 && o.PriceCents < o.MinPriceSeen) {
		o.MinPriceSeen = o.PriceCents
	}
	aspects, err := json.Marshal(nonNilMap(o.Aspects))
	if err != nil {
		return fmt.Errorf("pgoffer: encode aspects: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO offers (
  id, source_id, source_key, source_url, title, body,
  price_cents, currency, condition, seller, aspects_json,
  provisional_key, hint_brand, hint_mpn, hint_gtin, hint_model,
  first_seen_ns, last_seen_ns, min_price_cents, status, outcome, expired_at_ns
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
ON CONFLICT (id) DO UPDATE SET
  source_url = EXCLUDED.source_url,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  price_cents = EXCLUDED.price_cents,
  currency = EXCLUDED.currency,
  condition = EXCLUDED.condition,
  seller = EXCLUDED.seller,
  aspects_json = EXCLUDED.aspects_json,
  provisional_key = EXCLUDED.provisional_key,
  hint_brand = EXCLUDED.hint_brand,
  hint_mpn = EXCLUDED.hint_mpn,
  hint_gtin = EXCLUDED.hint_gtin,
  hint_model = EXCLUDED.hint_model,
  outcome = EXCLUDED.outcome,
  -- earliest wins, treating 0 as "unset" rather than as the year 1970
  first_seen_ns = CASE
    WHEN offers.first_seen_ns = 0 THEN EXCLUDED.first_seen_ns
    WHEN EXCLUDED.first_seen_ns = 0 THEN offers.first_seen_ns
    ELSE LEAST(offers.first_seen_ns, EXCLUDED.first_seen_ns) END,
  -- last_seen only ever advances
  last_seen_ns = GREATEST(offers.last_seen_ns, EXCLUDED.last_seen_ns),
  -- lowest ever, ignoring 0 = unknown price
  min_price_cents = CASE
    WHEN offers.min_price_cents = 0 THEN EXCLUDED.min_price_cents
    WHEN EXCLUDED.min_price_cents = 0 THEN offers.min_price_cents
    ELSE LEAST(offers.min_price_cents, EXCLUDED.min_price_cents) END,
  status = EXCLUDED.status,
  -- a re-observed offer is purchasable again, so the expiry stamp is cleared
  expired_at_ns = CASE WHEN EXCLUDED.status = 'active' THEN 0 ELSE EXCLUDED.expired_at_ns END`,
		o.ID, o.SourceID, o.SourceKey, o.SourceURL, o.Title, o.Body,
		o.PriceCents, o.Currency, o.Condition, o.Seller, string(aspects),
		o.ProvisionalKey, o.ProductHint.Brand, o.ProductHint.MPN, o.ProductHint.GTIN, o.ProductHint.Model,
		o.FirstSeen.UnixNano(), o.LastSeen.UnixNano(), o.MinPriceSeen,
		string(o.Status), string(o.Outcome), nsOrZero(o.ExpiredAt),
	)
	if err != nil {
		return fmt.Errorf("pgoffer: upsert: %w", err)
	}
	return nil
}

// Get returns one offer by id.
func (s *Store) Get(ctx context.Context, id string) (offer.Offer, bool, error) {
	rows, err := s.pool.Query(ctx, selectCols+` FROM offers WHERE id = $1`, id)
	if err != nil {
		return offer.Offer{}, false, fmt.Errorf("pgoffer: get: %w", err)
	}
	defer rows.Close()
	out, err := scanOffers(rows)
	if err != nil {
		return offer.Offer{}, false, err
	}
	if len(out) == 0 {
		return offer.Offer{}, false, nil
	}
	return out[0], true, nil
}

// Query returns offers matching q, most-recently-seen first. Expired offers are
// EXCLUDED unless q.IncludeExpired is set -- the safety default that keeps a dead
// listing out of anything that recommends a purchase.
func (s *Store) Query(ctx context.Context, q offer.Query) ([]offer.Offer, error) {
	var where []string
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if !q.IncludeExpired {
		add(`status = $%d`, string(offer.StatusActive))
	}
	if q.SourceID != "" {
		add(`source_id = $%d`, q.SourceID)
	}
	if q.ProvisionalKey != "" {
		add(`provisional_key = $%d`, q.ProvisionalKey)
	}
	if q.Seller != "" {
		add(`seller = $%d`, q.Seller)
	}
	if !q.Since.IsZero() {
		add(`last_seen_ns >= $%d`, q.Since.UnixNano())
	}
	sqlStr := selectCols + ` FROM offers`
	if len(where) > 0 {
		sqlStr += ` WHERE ` + strings.Join(where, ` AND `)
	}
	sqlStr += ` ORDER BY last_seen_ns DESC, id ASC`
	if q.Limit > 0 {
		sqlStr += fmt.Sprintf(` LIMIT %d`, q.Limit)
	}
	rows, err := s.pool.Query(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("pgoffer: query: %w", err)
	}
	defer rows.Close()
	return scanOffers(rows)
}

// MarkExpired transitions a source's unseen offers to expired. It RETAINS them:
// an expired offer is still evidence about what a vendor charged and when.
func (s *Store) MarkExpired(ctx context.Context, sourceID string, notSeenSince time.Time, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE offers SET status = $1, expired_at_ns = $2
WHERE status = $3 AND last_seen_ns < $4 AND ($5 = '' OR source_id = $5)`,
		string(offer.StatusExpired), now.UnixNano(),
		string(offer.StatusActive), notSeenSince.UnixNano(), sourceID)
	if err != nil {
		return 0, fmt.Errorf("pgoffer: mark expired: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ApplyRetention enforces a source's retention policy. It is the only operation
// here that deletes.
func (s *Store) ApplyRetention(ctx context.Context, sourceID string, r offer.Retention, now time.Time) (int, error) {
	if err := r.Validate(); err != nil {
		return 0, err
	}
	if r.Policy == offer.RetainFull {
		return 0, nil
	}
	cutoff := now.Add(-r.Window)
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM offers WHERE last_seen_ns < $1 AND ($2 = '' OR source_id = $2)`,
		cutoff.UnixNano(), sourceID)
	if err != nil {
		return 0, fmt.Errorf("pgoffer: apply retention: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// --- scanning -----------------------------------------------------------------

const selectCols = `SELECT
  id, source_id, source_key, source_url, title, body,
  price_cents, currency, condition, seller, aspects_json,
  provisional_key, hint_brand, hint_mpn, hint_gtin, hint_model,
  first_seen_ns, last_seen_ns, min_price_cents, status, outcome, expired_at_ns`

func scanOffers(rows pgx.Rows) ([]offer.Offer, error) {
	var out []offer.Offer
	for rows.Next() {
		var o offer.Offer
		var aspects, status, outcome string
		var firstNS, lastNS, expiredNS int64
		if err := rows.Scan(
			&o.ID, &o.SourceID, &o.SourceKey, &o.SourceURL, &o.Title, &o.Body,
			&o.PriceCents, &o.Currency, &o.Condition, &o.Seller, &aspects,
			&o.ProvisionalKey, &o.ProductHint.Brand, &o.ProductHint.MPN, &o.ProductHint.GTIN, &o.ProductHint.Model,
			&firstNS, &lastNS, &o.MinPriceSeen, &status, &outcome, &expiredNS,
		); err != nil {
			return nil, fmt.Errorf("pgoffer: scan: %w", err)
		}
		if aspects != "" {
			if err := json.Unmarshal([]byte(aspects), &o.Aspects); err != nil {
				return nil, fmt.Errorf("pgoffer: decode aspects: %w", err)
			}
		}
		o.FirstSeen = timeOrZero(firstNS)
		o.LastSeen = timeOrZero(lastNS)
		o.ExpiredAt = timeOrZero(expiredNS)
		o.Status = offer.Status(status)
		o.Outcome = offer.Outcome(outcome)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("pgoffer: rows: %w", err)
	}
	return out, nil
}

func timeOrZero(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func nsOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
