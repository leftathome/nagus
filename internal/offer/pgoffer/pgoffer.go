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
	"github.com/jackc/pgx/v5/pgconn"
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
	-- retired 2026-09-13: was the output of ComputeProvisionalKey, superseded by
-- the quark product_id. Kept, unwritten, so a fresh schema matches a migrated one;
-- existing rows keep their old values and nothing reads them.
	provisional_key  TEXT NOT NULL DEFAULT '',
	hint_brand       TEXT NOT NULL DEFAULT '',
	hint_mpn         TEXT NOT NULL DEFAULT '',
	hint_gtin        TEXT NOT NULL DEFAULT '',
	hint_model       TEXT NOT NULL DEFAULT '',
	hint_text        TEXT NOT NULL DEFAULT '',
	first_seen_ns    BIGINT NOT NULL DEFAULT 0,
	last_seen_ns     BIGINT NOT NULL DEFAULT 0,
	min_price_cents  BIGINT NOT NULL DEFAULT 0,
	status           TEXT NOT NULL DEFAULT 'active',
	outcome          TEXT NOT NULL DEFAULT '',
	expired_at_ns    BIGINT NOT NULL DEFAULT 0,
	resolution_state      TEXT NOT NULL DEFAULT 'unattempted',
	product_id            TEXT NOT NULL DEFAULT '',
	resolution_generation BIGINT NOT NULL DEFAULT 0,
	resolved_at_ns        BIGINT NOT NULL DEFAULT 0,
	price_changed_at_ns   BIGINT NOT NULL DEFAULT 0,
	previous_price_cents  BIGINT NOT NULL DEFAULT 0,
	ends_at_ns            BIGINT NOT NULL DEFAULT 0
);

-- quark resolution columns, added after the table first shipped. ADD COLUMN IF
-- NOT EXISTS makes this idempotent on every startup and additive on an existing
-- database: every stored offer defaults to unattempted, which is exactly true.
ALTER TABLE offers ADD COLUMN IF NOT EXISTS resolution_state TEXT NOT NULL DEFAULT 'unattempted';
ALTER TABLE offers ADD COLUMN IF NOT EXISTS product_id TEXT NOT NULL DEFAULT '';
ALTER TABLE offers ADD COLUMN IF NOT EXISTS resolution_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE offers ADD COLUMN IF NOT EXISTS resolved_at_ns BIGINT NOT NULL DEFAULT 0;
-- QUARK-02 text hints: additive; '' keeps every existing fingerprint unchanged.
ALTER TABLE offers ADD COLUMN IF NOT EXISTS hint_text TEXT NOT NULL DEFAULT '';
-- QUARK-04 wine vintage mode, from quark's resolution specs: additive; ''
-- (quark said nothing) is exactly true of every existing row.
ALTER TABLE offers ADD COLUMN IF NOT EXISTS vintage_mode TEXT NOT NULL DEFAULT '';
-- Offer timing (nagus-417, docs/design/2026-10-03-new-deals-offer-timing.md).
-- A constant DEFAULT makes each ADD COLUMN catalog-only (PostgreSQL 11+): no
-- table rewrite, whatever the row count.
ALTER TABLE offers ADD COLUMN IF NOT EXISTS price_changed_at_ns BIGINT NOT NULL DEFAULT 0;
ALTER TABLE offers ADD COLUMN IF NOT EXISTS previous_price_cents BIGINT NOT NULL DEFAULT 0;
ALTER TABLE offers ADD COLUMN IF NOT EXISTS ends_at_ns BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_offers_source ON offers(source_id);
CREATE INDEX IF NOT EXISTS idx_offers_status ON offers(status);
CREATE INDEX IF NOT EXISTS idx_offers_last_seen ON offers(last_seen_ns);
-- The dedup path: "every offer for this product across sellers".
DROP INDEX IF EXISTS idx_offers_provisional_key;
CREATE INDEX IF NOT EXISTS idx_offers_resolution_state ON offers(resolution_state);
CREATE INDEX IF NOT EXISTS idx_offers_product_id ON offers(product_id);

-- Price history (nagus-0ta, docs/design/2026-10-03-offer-price-history.md):
-- append-only, one row per price CHANGE (plus the first known price), written
-- in Put's transaction. The primary key is the read path (one offer's points in
-- time order) and the conflict target. A new table: creating it is
-- catalog-only, whatever the size of offers, and nothing is backfilled. No
-- foreign key, matching the SQLite adapter: ApplyRetention deletes an offer's
-- rows explicitly, in the same transaction as the offer.
CREATE TABLE IF NOT EXISTS offer_price_history (
	offer_id       TEXT NOT NULL,
	observed_at_ns BIGINT NOT NULL,
	price_cents    BIGINT NOT NULL,
	currency       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (offer_id, observed_at_ns)
);
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
	if _, err := pool.Exec(ctx, mailSchema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgoffer: mail state schema: %w", err)
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
//
// Since price history (nagus-0ta) the upsert runs in a transaction that first
// locks the stored row (SELECT ... FOR UPDATE): the history row needs the
// price the upsert REPLACED, which RETURNING cannot give on PostgreSQL 16
// (no OLD), and the lock keeps a concurrent Put from changing it in between.
// The fold itself is still the single statement below.
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
	// A first insert that loses a race with a concurrent first insert of the
	// same offer did not read the row it then updated, so it cannot tell what
	// price it replaced. That attempt is rolled back and retried; the retry
	// finds (and locks) the row. A deadlock or serialization failure reported
	// by the server (40P01 / 40001) is retried the same way: the attempt was
	// rolled back whole, so repeating it is safe.
	for attempt := 1; ; attempt++ {
		retry, err := s.put(ctx, o, string(aspects))
		if err != nil && retryableTxError(err) {
			retry, err = true, fmt.Errorf("pgoffer: put %s: %w", o.ID, err)
		}
		if !retry {
			return err
		}
		if attempt >= putAttempts {
			if err != nil {
				return err
			}
			return fmt.Errorf("pgoffer: put %s: lost the insert race repeatedly", o.ID)
		}
	}
}

// putAttempts bounds Put's retries of a rolled-back attempt.
const putAttempts = 4

// retryableTxError reports a transaction the server rolled back on its own
// and that may simply be repeated: deadlock_detected (40P01) and
// serialization_failure (40001).
func retryableTxError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// put is one attempt of Put, in one transaction: lock the stored row (when
// there is one) to know the price it held, upsert, then append the price
// history HistoryRows decides. retry is true when the attempt was rolled back
// because a concurrent insert created the row between the read and the upsert.
func (s *Store) put(ctx context.Context, o offer.Offer, aspects string) (retry bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("pgoffer: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prior := offer.PriorPrice{}
	var prevPrice, prevChanged int64
	var prevCurrency string
	err = tx.QueryRow(ctx, `SELECT price_cents, currency, `+storedPriceChangedAtBare+` FROM offers WHERE id = $1 FOR UPDATE`, o.ID).
		Scan(&prevPrice, &prevCurrency, &prevChanged)
	switch {
	case err == nil:
		prior = offer.PriorPrice{Exists: true, PriceCents: prevPrice, Currency: prevCurrency, ChangedAt: timeOrZero(prevChanged)}
		if offer.HistoryNeedsProbe(prior, o.PriceCents) {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM offer_price_history WHERE offer_id = $1)`, o.ID).
				Scan(&prior.HasHistory); err != nil {
				return false, fmt.Errorf("pgoffer: probe history: %w", err)
			}
		}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return false, fmt.Errorf("pgoffer: read previous: %w", err)
	}

	var changedNS int64
	var inserted bool
	err = tx.QueryRow(ctx, `
INSERT INTO offers (
  id, source_id, source_key, source_url, title, body,
  price_cents, currency, condition, seller, aspects_json,
  hint_brand, hint_mpn, hint_gtin, hint_model, hint_text,
  first_seen_ns, last_seen_ns, min_price_cents, status, outcome, expired_at_ns,
  price_changed_at_ns, ends_at_ns
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
ON CONFLICT (id) DO UPDATE SET
  -- Price change (offer.Store.Put): a different non-zero price stamps the
  -- observation time (never earlier than the stored stamp) and records the
  -- stored price as the previous one. Read before price_cents is overwritten
  -- below, like the hint CASEs. A stored 0 stamp predates the column: it
  -- means first_seen.
  price_changed_at_ns = CASE WHEN `+priceChanged+`
    THEN GREATEST(`+storedPriceChangedAt+`, EXCLUDED.last_seen_ns)
    ELSE `+storedPriceChangedAt+` END,
  previous_price_cents = CASE WHEN `+priceChanged+` THEN offers.price_cents ELSE offers.previous_price_cents END,
  -- what the source states now; 0 when it states no end
  ends_at_ns = EXCLUDED.ends_at_ns,
  -- Resolution survives a re-ingest of the SAME hint and resets on a changed
  -- one. SET expressions read the row as it was before this update, so these
  -- compare the stored hint with the incoming one even though the hint columns
  -- are overwritten below. Put never writes a caller-supplied resolution.
  resolution_state = CASE WHEN `+sameHint+` THEN offers.resolution_state ELSE 'unattempted' END,
  product_id = CASE WHEN `+sameHint+` THEN offers.product_id ELSE '' END,
  resolution_generation = CASE WHEN `+sameHint+` THEN offers.resolution_generation ELSE 0 END,
  resolved_at_ns = CASE WHEN `+sameHint+` THEN offers.resolved_at_ns ELSE 0 END,
  vintage_mode = CASE WHEN `+sameHint+` THEN offers.vintage_mode ELSE '' END,
  source_url = EXCLUDED.source_url,
  title = EXCLUDED.title,
  body = EXCLUDED.body,
  -- an unknown-price (0) poll keeps the last known price, so a later poll
  -- still compares against it (offer.Store.Put)
  price_cents = CASE WHEN EXCLUDED.price_cents = 0 THEN offers.price_cents ELSE EXCLUDED.price_cents END,
  currency = EXCLUDED.currency,
  condition = EXCLUDED.condition,
  seller = EXCLUDED.seller,
  aspects_json = EXCLUDED.aspects_json,
  hint_brand = EXCLUDED.hint_brand,
  hint_mpn = EXCLUDED.hint_mpn,
  hint_gtin = EXCLUDED.hint_gtin,
  hint_model = EXCLUDED.hint_model,
  hint_text = EXCLUDED.hint_text,
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
  expired_at_ns = CASE WHEN EXCLUDED.status = 'active' THEN 0 ELSE EXCLUDED.expired_at_ns END
RETURNING price_changed_at_ns, (xmax = 0)`,
		o.ID, o.SourceID, o.SourceKey, o.SourceURL, o.Title, o.Body,
		o.PriceCents, o.Currency, o.Condition, o.Seller, aspects,
		o.ProductHint.Brand, o.ProductHint.MPN, o.ProductHint.GTIN, o.ProductHint.Model, o.ProductHint.Text,
		o.FirstSeen.UnixNano(), o.LastSeen.UnixNano(), o.MinPriceSeen,
		string(o.Status), string(o.Outcome), nsOrZero(o.ExpiredAt),
		o.FirstSeen.UnixNano(), nsOrZero(o.EndsAt),
	).Scan(&changedNS, &inserted)
	if err != nil {
		return false, fmt.Errorf("pgoffer: upsert: %w", err)
	}
	// xmax = 0 is how an upsert tells an insert from an update. No row was
	// read, yet this updated one: a concurrent first insert won the race.
	if !prior.Exists && !inserted {
		return true, nil
	}
	anchor, row := offer.HistoryRows(prior, o.PriceCents, o.Currency, o.LastSeen, timeOrZero(changedNS))
	if anchor != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO offer_price_history (offer_id, observed_at_ns, price_cents, currency)
VALUES ($1,$2,$3,$4) ON CONFLICT (offer_id, observed_at_ns) DO NOTHING`,
			o.ID, anchor.ObservedAt.UnixNano(), anchor.PriceCents, anchor.Currency); err != nil {
			return false, fmt.Errorf("pgoffer: history anchor: %w", err)
		}
	}
	if row != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO offer_price_history (offer_id, observed_at_ns, price_cents, currency)
VALUES ($1,$2,$3,$4) ON CONFLICT (offer_id, observed_at_ns) DO UPDATE SET price_cents = EXCLUDED.price_cents, currency = EXCLUDED.currency`,
			o.ID, row.ObservedAt.UnixNano(), row.PriceCents, row.Currency); err != nil {
			return false, fmt.Errorf("pgoffer: history: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("pgoffer: commit: %w", err)
	}
	return false, nil
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
	if q.ProductID != "" {
		add(`product_id = $%d`, q.ProductID)
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
	hcut, purgeHistory := r.HistoryCutoff(now)
	if r.Policy == offer.RetainFull && !purgeHistory {
		return 0, nil
	}
	for attempt := 1; ; attempt++ {
		n, err := s.applyRetention(ctx, sourceID, r, now, hcut, purgeHistory)
		if err == nil || !retryableTxError(err) || attempt >= putAttempts {
			return n, err
		}
	}
}

// applyRetention is one attempt of ApplyRetention, in one transaction.
//
// LOCK ORDER: offers before offer_price_history, the order Put takes them
// (SELECT ... FOR UPDATE on the offer, then the history insert). Deleting the
// history first and the offers second was the opposite order, and a Put of a
// purged offer running alongside deadlocked (40P01). So the purged offers are
// deleted FIRST, then -- in a SEPARATE statement -- every history row of
// those offers, whatever its observed_at; the window purge of the surviving
// offers' history comes after and takes no offer lock.
//
// Why two statements and not one data-modifying CTE: a statement runs on ONE
// snapshot. When the offers DELETE waits on a Put's row lock and that Put
// commits a history row (with a last_seen still older than the cutoff, so the
// offer is deleted anyway), a history DELETE in the same statement cannot see
// the new row and leaves it orphaned. The next statement takes a new snapshot
// (READ COMMITTED) and sees it; the offer rows stay locked by this
// transaction, so no new history for them can commit in between.
func (s *Store) applyRetention(ctx context.Context, sourceID string, r offer.Retention, now, hcut time.Time, purgeHistory bool) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("pgoffer: retention begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	n := 0
	if r.Policy != offer.RetainFull {
		cutoff := now.Add(-r.Window).UnixNano()
		rows, err := tx.Query(ctx, `DELETE FROM offers WHERE last_seen_ns < $1 AND ($2 = '' OR source_id = $2) RETURNING id`, cutoff, sourceID)
		if err != nil {
			return 0, fmt.Errorf("pgoffer: apply retention: %w", err)
		}
		gone, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, fmt.Errorf("pgoffer: apply retention: %w", err)
		}
		n = len(gone)
		if n > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM offer_price_history WHERE offer_id = ANY($1)`, gone); err != nil {
				return 0, fmt.Errorf("pgoffer: purge offer history: %w", err)
			}
		}
	}
	if purgeHistory {
		// RetainFull keeps each offer's carry-in point (its newest row at or
		// before the cutoff): a row goes only when a NEWER row at or before
		// the cutoff supersedes it. Purge (eBay) keeps nothing older.
		if _, err := tx.Exec(ctx, `DELETE FROM offer_price_history h WHERE h.observed_at_ns < $1
AND h.offer_id IN (SELECT id FROM offers WHERE $2 = '' OR source_id = $2)
AND (NOT $3 OR EXISTS (SELECT 1 FROM offer_price_history h2
  WHERE h2.offer_id = h.offer_id AND h2.observed_at_ns > h.observed_at_ns AND h2.observed_at_ns <= $1))`,
			hcut.UnixNano(), sourceID, r.KeepsCarryIn()); err != nil {
			return 0, fmt.Errorf("pgoffer: purge price history: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pgoffer: retention commit: %w", err)
	}
	return n, nil
}

// PriceHistory returns the retained history of each offer among ids, oldest
// first, at most perOffer of the newest rows observed at or after since, in
// ONE query (WHERE offer_id = ANY).
func (s *Store) PriceHistory(ctx context.Context, ids []string, since time.Time, perOffer int) (map[string][]offer.PricePoint, error) {
	out := make(map[string][]offer.PricePoint, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT offer_id, observed_at_ns, price_cents, currency FROM (
  SELECT offer_id, observed_at_ns, price_cents, currency,
         ROW_NUMBER() OVER (PARTITION BY offer_id ORDER BY observed_at_ns DESC) AS rn
  FROM offer_price_history
  WHERE offer_id = ANY($1) AND observed_at_ns >= $2
) h WHERE $3 <= 0 OR rn <= $3
ORDER BY offer_id, observed_at_ns`, ids, nsOrZero(since), perOffer)
	if err != nil {
		return nil, fmt.Errorf("pgoffer: price history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p offer.PricePoint
		var at int64
		if err := rows.Scan(&id, &at, &p.PriceCents, &p.Currency); err != nil {
			return nil, fmt.Errorf("pgoffer: price history scan: %w", err)
		}
		p.ObservedAt = timeOrZero(at)
		out[id] = append(out[id], p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgoffer: price history rows: %w", err)
	}
	return out, nil
}

// --- scanning -----------------------------------------------------------------

const selectCols = `SELECT
  id, source_id, source_key, source_url, title, body,
  price_cents, currency, condition, seller, aspects_json,
  hint_brand, hint_mpn, hint_gtin, hint_model, hint_text,
  first_seen_ns, last_seen_ns, min_price_cents, status, outcome, expired_at_ns,
  resolution_state, product_id, resolution_generation, resolved_at_ns, vintage_mode,
  price_changed_at_ns, previous_price_cents, ends_at_ns`

// priceChanged is true when the incoming price and the stored (last known)
// price are both known and differ (offer.Store.Put's price-change rule).
const priceChanged = `(EXCLUDED.price_cents > 0 AND offers.price_cents > 0 AND EXCLUDED.price_cents <> offers.price_cents)`

// storedPriceChangedAt is the stored stamp, a pre-column 0 read as first_seen.
const storedPriceChangedAt = `(CASE WHEN offers.price_changed_at_ns = 0 THEN offers.first_seen_ns ELSE offers.price_changed_at_ns END)`

// storedPriceChangedAtBare is storedPriceChangedAt for a plain SELECT.
const storedPriceChangedAtBare = `(CASE WHEN price_changed_at_ns = 0 THEN first_seen_ns ELSE price_changed_at_ns END)`

// sameHint is true when the stored hint equals the incoming one, column by
// column. Used inside the upsert's CASE expressions.
const sameHint = `(offers.hint_brand = EXCLUDED.hint_brand AND offers.hint_mpn = EXCLUDED.hint_mpn AND offers.hint_gtin = EXCLUDED.hint_gtin AND offers.hint_model = EXCLUDED.hint_model AND offers.hint_text = EXCLUDED.hint_text)`

// storedFingerprint computes offer.ProductHint.Fingerprint in SQL.
const storedFingerprint = `(hint_brand || '|' || hint_mpn || '|' || hint_gtin || '|' || hint_model || CASE WHEN hint_text <> '' THEN '|' || hint_text ELSE '' END)`

// PendingResolution returns never-asked offers first, then refusals recorded
// under a generation below retryBelowGeneration, each in id order.
func (s *Store) PendingResolution(ctx context.Context, limit int, retryBelowGeneration int64) ([]offer.Offer, error) {
	sqlStr := selectCols + ` FROM offers
WHERE resolution_state IN ('unattempted', '')
   OR (resolution_state = 'refused' AND $1 > 0 AND resolution_generation < $1)
ORDER BY CASE WHEN resolution_state = 'refused' THEN 1 ELSE 0 END, id`
	if limit > 0 {
		sqlStr += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := s.pool.Query(ctx, sqlStr, retryBelowGeneration)
	if err != nil {
		return nil, fmt.Errorf("pgoffer: pending resolution: %w", err)
	}
	defer rows.Close()
	return scanOffers(rows)
}

// RecordResolution stamps r only while the offer still carries the hint with
// this fingerprint; the WHERE clause is the race guard.
func (s *Store) RecordResolution(ctx context.Context, offerID, hintFingerprint string, r offer.Resolution) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE offers SET resolution_state = $1, product_id = $2, resolution_generation = $3, resolved_at_ns = $4, vintage_mode = $7
WHERE id = $5 AND `+storedFingerprint+` = $6`,
		string(r.State), r.ProductID, r.Generation, nsOrZero(r.At), offerID, hintFingerprint, r.VintageMode)
	if err != nil {
		return false, fmt.Errorf("pgoffer: record resolution: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func scanOffers(rows pgx.Rows) ([]offer.Offer, error) {
	var out []offer.Offer
	for rows.Next() {
		var o offer.Offer
		var aspects, status, outcome, resState string
		var firstNS, lastNS, expiredNS, resolvedNS, changedNS, endsNS int64
		if err := rows.Scan(
			&o.ID, &o.SourceID, &o.SourceKey, &o.SourceURL, &o.Title, &o.Body,
			&o.PriceCents, &o.Currency, &o.Condition, &o.Seller, &aspects,
			&o.ProductHint.Brand, &o.ProductHint.MPN, &o.ProductHint.GTIN, &o.ProductHint.Model, &o.ProductHint.Text,
			&firstNS, &lastNS, &o.MinPriceSeen, &status, &outcome, &expiredNS,
			&resState, &o.Resolution.ProductID, &o.Resolution.Generation, &resolvedNS, &o.Resolution.VintageMode,
			&changedNS, &o.PreviousPriceCents, &endsNS,
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
		o.PriceChangedAt = changedAtOrFirstSeen(changedNS, firstNS)
		o.EndsAt = timeOrZero(endsNS)
		o.Status = offer.Status(status)
		o.Resolution.State = offer.ResolutionState(resState)
		if o.Resolution.State == "" {
			o.Resolution.State = offer.ResolutionUnattempted
		}
		o.Resolution.At = timeOrZero(resolvedNS)
		o.Outcome = offer.Outcome(outcome)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("pgoffer: rows: %w", err)
	}
	return out, nil
}

// Summaries returns the summaries of the stored offers among ids, expired ones
// included, in ONE query (WHERE id = ANY): the surface checks a whole
// candidate set (up to pipeline.DefaultMaxCandidates) at once.
func (s *Store) Summaries(ctx context.Context, ids []string) (map[string]offer.Summary, error) {
	out := make(map[string]offer.Summary, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT
  id, source_id, status, price_cents, min_price_cents, previous_price_cents,
  first_seen_ns, last_seen_ns, price_changed_at_ns, expired_at_ns, ends_at_ns,
  resolution_state, product_id, resolution_generation, resolved_at_ns, vintage_mode
FROM offers WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("pgoffer: summaries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sm offer.Summary
		var status, resState string
		var firstNS, lastNS, changedNS, expiredNS, endsNS, resolvedNS int64
		if err := rows.Scan(&sm.ID, &sm.SourceID, &status, &sm.PriceCents, &sm.MinPriceSeen, &sm.PreviousPriceCents,
			&firstNS, &lastNS, &changedNS, &expiredNS, &endsNS,
			&resState, &sm.Resolution.ProductID, &sm.Resolution.Generation, &resolvedNS, &sm.Resolution.VintageMode,
		); err != nil {
			return nil, fmt.Errorf("pgoffer: summaries scan: %w", err)
		}
		sm.Status = offer.Status(status)
		sm.FirstSeen, sm.LastSeen = timeOrZero(firstNS), timeOrZero(lastNS)
		sm.PriceChangedAt = changedAtOrFirstSeen(changedNS, firstNS)
		sm.ExpiredAt, sm.EndsAt = timeOrZero(expiredNS), timeOrZero(endsNS)
		sm.Resolution.State = offer.ResolutionState(resState)
		if sm.Resolution.State == "" {
			sm.Resolution.State = offer.ResolutionUnattempted
		}
		sm.Resolution.At = timeOrZero(resolvedNS)
		out[sm.ID] = sm
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgoffer: summaries rows: %w", err)
	}
	return out, nil
}

// BackfillPriceChangedAt gives every offer stored before price_changed_at_ns
// existed the value a first insert gets, its first_seen, and returns how many
// rows it changed. See offer.Backfiller for why it is batched and run in the
// background rather than in New.
//
// Batches walk the primary key (id > last ORDER BY id LIMIT batch), so each is
// an index range, not a table scan, and holds its row locks only for its own
// short transaction: an ingest upsert of a row in a running batch waits for
// that batch, never for the whole backfill. A database with nothing to fill
// (every later startup) answers from one EXISTS probe.
func (s *Store) BackfillPriceChangedAt(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = offer.DefaultBackfillBatch
	}
	var pending bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM offers WHERE price_changed_at_ns = 0 AND first_seen_ns <> 0)`).Scan(&pending); err != nil {
		return 0, fmt.Errorf("pgoffer: backfill probe: %w", err)
	}
	if !pending {
		return 0, nil
	}
	total, last := 0, ""
	for {
		var upper string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM offers WHERE id > $1 ORDER BY id OFFSET $2 LIMIT 1`, last, batch-1).Scan(&upper)
		done := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !done {
			return total, fmt.Errorf("pgoffer: backfill bound: %w", err)
		}
		q := `UPDATE offers SET price_changed_at_ns = first_seen_ns
 WHERE id > $1 AND price_changed_at_ns = 0 AND first_seen_ns <> 0`
		args := []any{last}
		if !done {
			q += ` AND id <= $2`
			args = append(args, upper)
		}
		tag, err := s.pool.Exec(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("pgoffer: backfill: %w", err)
		}
		total += int(tag.RowsAffected())
		if done {
			return total, nil
		}
		last = upper
	}
}

// changedAtOrFirstSeen reads a stored price_changed_at, where 0 (a row older
// than the column, not yet backfilled) means first_seen.
func changedAtOrFirstSeen(changedNS, firstNS int64) time.Time {
	if changedNS == 0 {
		return timeOrZero(firstNS)
	}
	return timeOrZero(changedNS)
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
