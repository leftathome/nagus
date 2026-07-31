// Package sqliteoffer is the SQLite-backed offer.Store adapter.
//
// It is correct when it passes internal/offer/offerstoretest, the same contract
// MemoryStore passes -- that suite is the specification, this file is one
// implementation of it. The driver is modernc.org/sqlite (pure Go, CGO-free) so
// the binary still drops into distroless/static.
//
// # What persistence has to preserve
//
// The lifecycle folding in Put is the reason this layer exists, so it is done in
// a TRANSACTION against the stored row rather than as a blind upsert: FirstSeen
// must survive re-observation, LastSeen must only advance, MinPriceSeen must keep
// the lowest price ever seen (which is what still shows an ended discount), and a
// re-observed offer must revive from expired. A plain INSERT OR REPLACE would
// silently destroy all four.
package sqliteoffer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/leftathome/nagus/internal/offer"
)

// Store is a SQLite-backed offer.Store.
type Store struct {
	db *sql.DB
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
	price_cents      INTEGER NOT NULL DEFAULT 0,
	currency         TEXT NOT NULL DEFAULT '',
	condition        TEXT NOT NULL DEFAULT '',
	seller           TEXT NOT NULL DEFAULT '',
	aspects_json     TEXT NOT NULL DEFAULT '{}',
	provisional_key  TEXT NOT NULL DEFAULT '',
	hint_brand       TEXT NOT NULL DEFAULT '',
	hint_mpn         TEXT NOT NULL DEFAULT '',
	hint_gtin        TEXT NOT NULL DEFAULT '',
	hint_model       TEXT NOT NULL DEFAULT '',
	first_seen_ns    INTEGER NOT NULL DEFAULT 0,
	last_seen_ns     INTEGER NOT NULL DEFAULT 0,
	min_price_cents  INTEGER NOT NULL DEFAULT 0,
	status           TEXT NOT NULL DEFAULT 'active',
	outcome          TEXT NOT NULL DEFAULT '',
	expired_at_ns    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_offers_source ON offers(source_id);
CREATE INDEX IF NOT EXISTS idx_offers_status ON offers(status);
CREATE INDEX IF NOT EXISTS idx_offers_last_seen ON offers(last_seen_ns);
-- The dedup path: "every offer for this product across sellers".
CREATE INDEX IF NOT EXISTS idx_offers_provisional_key ON offers(provisional_key);
`

// New opens (or creates) an offer database at dsn and applies the schema.
func New(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqliteoffer: open %s: %w", dsn, err)
	}
	// SQLite is a single writer, and nagus runs ONE INGEST GOROUTINE PER SOURCE
	// all writing this one database. Capping the pool at a single connection
	// serialises them; without it, two sources ingesting concurrently produce
	// "database is locked (SQLITE_BUSY)" and silently drop offers. It also makes
	// the pragmas below actually stick, since database/sql applies an Exec to
	// whichever pooled connection it lands on -- with a pool of one there is no
	// other connection to miss them. (Matches internal/store/sqlitestore.)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqliteoffer: pragma: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqliteoffer: schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Put inserts or updates an offer, folding lifecycle against the stored row.
//
// The read-modify-write runs inside a transaction because the fold depends on
// the previous row: two concurrent ingests of the same source would otherwise be
// able to interleave and lose a FirstSeen or a MinPriceSeen.
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqliteoffer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var prevFirst, prevLast, prevMin int64
	err = tx.QueryRowContext(ctx,
		`SELECT first_seen_ns, last_seen_ns, min_price_cents FROM offers WHERE id = ?`, o.ID,
	).Scan(&prevFirst, &prevLast, &prevMin)
	switch {
	case err == nil:
		// FirstSeen never moves forward on re-observation.
		if prevFirst != 0 && (o.FirstSeen.IsZero() || prevFirst < o.FirstSeen.UnixNano()) {
			o.FirstSeen = time.Unix(0, prevFirst).UTC()
		}
		// LastSeen only advances, so a late/out-of-order write cannot make a
		// live offer look stale and get expired by the next housekeeping pass.
		if prevLast > o.LastSeen.UnixNano() {
			o.LastSeen = time.Unix(0, prevLast).UTC()
		}
		// Lowest EVER, ignoring 0 which means unknown rather than free.
		if prevMin > 0 && (o.MinPriceSeen == 0 || prevMin < o.MinPriceSeen) {
			o.MinPriceSeen = prevMin
		}
	case err == sql.ErrNoRows:
		// new offer
	default:
		return fmt.Errorf("sqliteoffer: read previous: %w", err)
	}

	if o.Status == offer.StatusActive {
		o.ExpiredAt = time.Time{}
	}
	aspects, err := json.Marshal(nonNilMap(o.Aspects))
	if err != nil {
		return fmt.Errorf("sqliteoffer: encode aspects: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO offers (
  id, source_id, source_key, source_url, title, body,
  price_cents, currency, condition, seller, aspects_json,
  provisional_key, hint_brand, hint_mpn, hint_gtin, hint_model,
  first_seen_ns, last_seen_ns, min_price_cents, status, outcome, expired_at_ns
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  source_url=excluded.source_url, title=excluded.title, body=excluded.body,
  price_cents=excluded.price_cents, currency=excluded.currency,
  condition=excluded.condition, seller=excluded.seller,
  aspects_json=excluded.aspects_json, provisional_key=excluded.provisional_key,
  hint_brand=excluded.hint_brand, hint_mpn=excluded.hint_mpn,
  hint_gtin=excluded.hint_gtin, hint_model=excluded.hint_model,
  first_seen_ns=excluded.first_seen_ns, last_seen_ns=excluded.last_seen_ns,
  min_price_cents=excluded.min_price_cents, status=excluded.status,
  outcome=excluded.outcome, expired_at_ns=excluded.expired_at_ns`,
		o.ID, o.SourceID, o.SourceKey, o.SourceURL, o.Title, o.Body,
		o.PriceCents, o.Currency, o.Condition, o.Seller, string(aspects),
		o.ProvisionalKey, o.ProductHint.Brand, o.ProductHint.MPN, o.ProductHint.GTIN, o.ProductHint.Model,
		o.FirstSeen.UnixNano(), o.LastSeen.UnixNano(), o.MinPriceSeen,
		string(o.Status), string(o.Outcome), nsOrZero(o.ExpiredAt),
	)
	if err != nil {
		return fmt.Errorf("sqliteoffer: upsert: %w", err)
	}
	return tx.Commit()
}

// Get returns one offer by id.
func (s *Store) Get(ctx context.Context, id string) (offer.Offer, bool, error) {
	rows, err := s.db.QueryContext(ctx, selectCols+` FROM offers WHERE id = ?`, id)
	if err != nil {
		return offer.Offer{}, false, fmt.Errorf("sqliteoffer: get: %w", err)
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
// EXCLUDED unless q.IncludeExpired is set -- the safety default.
func (s *Store) Query(ctx context.Context, q offer.Query) ([]offer.Offer, error) {
	var where []string
	var args []any
	if !q.IncludeExpired {
		where = append(where, `status = ?`)
		args = append(args, string(offer.StatusActive))
	}
	if q.SourceID != "" {
		where = append(where, `source_id = ?`)
		args = append(args, q.SourceID)
	}
	if q.ProvisionalKey != "" {
		where = append(where, `provisional_key = ?`)
		args = append(args, q.ProvisionalKey)
	}
	if q.Seller != "" {
		where = append(where, `seller = ?`)
		args = append(args, q.Seller)
	}
	if !q.Since.IsZero() {
		where = append(where, `last_seen_ns >= ?`)
		args = append(args, q.Since.UnixNano())
	}
	sqlStr := selectCols + ` FROM offers`
	if len(where) > 0 {
		sqlStr += ` WHERE ` + strings.Join(where, ` AND `)
	}
	sqlStr += ` ORDER BY last_seen_ns DESC, id ASC`
	if q.Limit > 0 {
		sqlStr += fmt.Sprintf(` LIMIT %d`, q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("sqliteoffer: query: %w", err)
	}
	defer rows.Close()
	return scanOffers(rows)
}

// MarkExpired transitions a source's unseen offers to expired. It RETAINS them:
// an expired offer is still evidence about what a vendor charged and when.
func (s *Store) MarkExpired(ctx context.Context, sourceID string, notSeenSince time.Time, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE offers SET status = ?, expired_at_ns = ?
WHERE status = ? AND last_seen_ns < ? AND (? = '' OR source_id = ?)`,
		string(offer.StatusExpired), now.UnixNano(),
		string(offer.StatusActive), notSeenSince.UnixNano(), sourceID, sourceID)
	if err != nil {
		return 0, fmt.Errorf("sqliteoffer: mark expired: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
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
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM offers WHERE last_seen_ns < ? AND (? = '' OR source_id = ?)`,
		cutoff.UnixNano(), sourceID, sourceID)
	if err != nil {
		return 0, fmt.Errorf("sqliteoffer: apply retention: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// --- scanning -----------------------------------------------------------------

const selectCols = `SELECT
  id, source_id, source_key, source_url, title, body,
  price_cents, currency, condition, seller, aspects_json,
  provisional_key, hint_brand, hint_mpn, hint_gtin, hint_model,
  first_seen_ns, last_seen_ns, min_price_cents, status, outcome, expired_at_ns`

func scanOffers(rows *sql.Rows) ([]offer.Offer, error) {
	var out []offer.Offer
	for rows.Next() {
		var o offer.Offer
		var aspects string
		var firstNS, lastNS, expiredNS int64
		var status, outcome string
		if err := rows.Scan(
			&o.ID, &o.SourceID, &o.SourceKey, &o.SourceURL, &o.Title, &o.Body,
			&o.PriceCents, &o.Currency, &o.Condition, &o.Seller, &aspects,
			&o.ProvisionalKey, &o.ProductHint.Brand, &o.ProductHint.MPN, &o.ProductHint.GTIN, &o.ProductHint.Model,
			&firstNS, &lastNS, &o.MinPriceSeen, &status, &outcome, &expiredNS,
		); err != nil {
			return nil, fmt.Errorf("sqliteoffer: scan: %w", err)
		}
		if aspects != "" {
			if err := json.Unmarshal([]byte(aspects), &o.Aspects); err != nil {
				return nil, fmt.Errorf("sqliteoffer: decode aspects: %w", err)
			}
		}
		o.FirstSeen = timeOrZero(firstNS)
		o.LastSeen = timeOrZero(lastNS)
		o.ExpiredAt = timeOrZero(expiredNS)
		o.Status = offer.Status(status)
		o.Outcome = offer.Outcome(outcome)
		out = append(out, o)
	}
	return out, rows.Err()
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
