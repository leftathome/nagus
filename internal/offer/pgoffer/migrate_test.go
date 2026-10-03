package pgoffer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pgtest"
)

// The prod offers table predates the resolution columns and holds live data, so
// the migration is tested against that exact shape with a row already in it.
const preResolutionSchema = `
DROP TABLE IF EXISTS offers;
CREATE TABLE offers (
	id TEXT PRIMARY KEY, source_id TEXT NOT NULL, source_key TEXT NOT NULL,
	source_url TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '',
	price_cents BIGINT NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
	condition TEXT NOT NULL DEFAULT '', seller TEXT NOT NULL DEFAULT '',
	aspects_json TEXT NOT NULL DEFAULT '{}', provisional_key TEXT NOT NULL DEFAULT '',
	hint_brand TEXT NOT NULL DEFAULT '', hint_mpn TEXT NOT NULL DEFAULT '',
	hint_gtin TEXT NOT NULL DEFAULT '', hint_model TEXT NOT NULL DEFAULT '',
	first_seen_ns BIGINT NOT NULL DEFAULT 0, last_seen_ns BIGINT NOT NULL DEFAULT 0,
	min_price_cents BIGINT NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'active',
	outcome TEXT NOT NULL DEFAULT '', expired_at_ns BIGINT NOT NULL DEFAULT 0
);
INSERT INTO offers (id, source_id, source_key, title, price_cents, hint_brand, hint_mpn, last_seen_ns)
VALUES ('legacy-1', 'shopify:old', 'k', 'An old drive', 5000, 'Seagate', 'ST1', 1750000000000000000);
`

func TestMigrationAddsResolutionToExistingTable(t *testing.T) {
	// A database private to this package (nagus-0wj): packages run in
	// parallel and must not truncate each other's tables.
	dsn := pgtest.DSN(t, "pgoffer_migrate")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(ctx, preResolutionSchema); err != nil {
		raw.Close()
		t.Fatalf("build legacy table: %v", err)
	}
	raw.Close()

	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New on a pre-resolution table: %v", err)
	}
	got, ok, err := s.Get(ctx, "legacy-1")
	if err != nil || !ok {
		t.Fatalf("legacy offer lost in migration: ok=%v err=%v", ok, err)
	}
	if got.Title != "An old drive" || got.PriceCents != 5000 || got.Resolution.State != offer.ResolutionUnattempted {
		t.Fatalf("legacy offer after migration: %+v", got)
	}
	if applied, err := s.RecordResolution(ctx, "legacy-1", got.ProductHint.Fingerprint(),
		offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", At: time.Unix(1_760_000_000, 0).UTC()}); err != nil || !applied {
		t.Fatalf("RecordResolution on migrated row: applied=%v err=%v", applied, err)
	}
	s.Close()

	// Idempotent: every startup re-runs the schema.
	s2, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("second New (migration re-run): %v", err)
	}
	defer s2.Close()
	again, _, _ := s2.Get(ctx, "legacy-1")
	if again.Resolution.ProductID != "p-1" {
		t.Fatalf("resolution lost across reopen: %+v", again.Resolution)
	}
}

// The prod table as it stands before the offer-timing columns (nagus-417):
// every column up to vintage_mode, with a live row that has a first_seen.
const preTimingRows = `
ALTER TABLE offers ADD COLUMN resolution_state TEXT NOT NULL DEFAULT 'unattempted';
ALTER TABLE offers ADD COLUMN product_id TEXT NOT NULL DEFAULT '';
ALTER TABLE offers ADD COLUMN resolution_generation BIGINT NOT NULL DEFAULT 0;
ALTER TABLE offers ADD COLUMN resolved_at_ns BIGINT NOT NULL DEFAULT 0;
ALTER TABLE offers ADD COLUMN hint_text TEXT NOT NULL DEFAULT '';
ALTER TABLE offers ADD COLUMN vintage_mode TEXT NOT NULL DEFAULT '';
INSERT INTO offers (id, source_id, source_key, title, price_cents, first_seen_ns, last_seen_ns, min_price_cents)
VALUES ('timed-1', 'shopify:old', 't1', 'A timed drive', 4000, 1740000000000000000, 1750000000000000000, 3500);
`

func TestMigrationBackfillsPriceChangedAt(t *testing.T) {
	dsn := pgtest.DSN(t, "pgoffer_migrate_timing")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(ctx, preResolutionSchema+preTimingRows); err != nil {
		t.Fatalf("build pre-timing table: %v", err)
	}

	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New on a pre-timing table: %v", err)
	}
	defer s.Close()
	first := time.Unix(0, 1740000000000000000).UTC()
	got, ok, err := s.Get(ctx, "timed-1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !got.PriceChangedAt.Equal(first) || got.PreviousPriceCents != 0 || !got.EndsAt.IsZero() {
		t.Fatalf("migrated offer timing: %+v", got)
	}
	// Opening the store does NOT rewrite the table: the read above came from
	// the fallback, and the column is still 0.
	var stored int64
	if err := raw.QueryRow(ctx, `SELECT price_changed_at_ns FROM offers WHERE id = 'timed-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("New rewrote price_changed_at_ns (%d); the backfill belongs in the background", stored)
	}
	// The background backfill fills it, once.
	if n, err := s.BackfillPriceChangedAt(ctx, 1); err != nil || n != 1 {
		t.Fatalf("backfill: n=%d err=%v, want 1 row (legacy-1 has no first_seen)", n, err)
	}
	if err := raw.QueryRow(ctx, `SELECT price_changed_at_ns FROM offers WHERE id = 'timed-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != first.UnixNano() {
		t.Fatalf("price_changed_at_ns = %d, want %d (backfilled)", stored, first.UnixNano())
	}
	if n, err := s.BackfillPriceChangedAt(ctx, 0); err != nil || n != 0 {
		t.Fatalf("second backfill: n=%d err=%v, want 0 (idempotent)", n, err)
	}
	// A row with no first_seen is left alone and reads as zero.
	legacy, _, _ := s.Get(ctx, "legacy-1")
	if !legacy.PriceChangedAt.IsZero() {
		t.Fatalf("legacy row without first_seen: price_changed_at = %v", legacy.PriceChangedAt)
	}
}

// The batches walk the whole key space: more rows than one batch, a batch
// size that does not divide the count, every row filled exactly once.
func TestBackfillCrossesBatches(t *testing.T) {
	dsn := pgtest.DSN(t, "pgoffer_backfill_batches")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(ctx, `DROP TABLE IF EXISTS offers`); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Unix(1_750_000_000, 0).UTC()
	for i := 0; i < 7; i++ {
		if err := s.Put(ctx, offer.Offer{SourceID: "shopify:b", SourceKey: string(rune('a' + i)), PriceCents: 100, LastSeen: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// Make every row look pre-column.
	if _, err := raw.Exec(ctx, `UPDATE offers SET price_changed_at_ns = 0`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.BackfillPriceChangedAt(ctx, 2); err != nil || n != 7 {
		t.Fatalf("backfill in batches of 2: n=%d err=%v, want 7", n, err)
	}
	var wrong int
	if err := raw.QueryRow(ctx, `SELECT count(*) FROM offers WHERE price_changed_at_ns <> first_seen_ns`).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("%d rows not backfilled to first_seen (err %v)", wrong, err)
	}
}
