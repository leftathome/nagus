package sqliteoffer

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// preResolutionSchema is the offers table EXACTLY as it shipped before the
// quark resolution columns existed. Every deployment that ran an older nagus
// has this shape on disk, so the migration is tested against it, not against a
// fresh database that already has the new columns.
const preResolutionSchema = `
CREATE TABLE offers (
	id TEXT PRIMARY KEY, source_id TEXT NOT NULL, source_key TEXT NOT NULL,
	source_url TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '',
	price_cents INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
	condition TEXT NOT NULL DEFAULT '', seller TEXT NOT NULL DEFAULT '',
	aspects_json TEXT NOT NULL DEFAULT '{}', provisional_key TEXT NOT NULL DEFAULT '',
	hint_brand TEXT NOT NULL DEFAULT '', hint_mpn TEXT NOT NULL DEFAULT '',
	hint_gtin TEXT NOT NULL DEFAULT '', hint_model TEXT NOT NULL DEFAULT '',
	first_seen_ns INTEGER NOT NULL DEFAULT 0, last_seen_ns INTEGER NOT NULL DEFAULT 0,
	min_price_cents INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'active',
	outcome TEXT NOT NULL DEFAULT '', expired_at_ns INTEGER NOT NULL DEFAULT 0
);
INSERT INTO offers (id, source_id, source_key, title, price_cents, hint_brand, hint_mpn, last_seen_ns)
VALUES ('legacy-1', 'shopify:old', 'k', 'An old drive', 5000, 'Seagate', 'ST1', 1750000000000000000);
`

func TestMigrationAddsResolutionToExistingDatabase(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(preResolutionSchema); err != nil {
		t.Fatalf("build legacy schema: %v", err)
	}
	_ = raw.Close()

	ctx := context.Background()
	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New on a pre-resolution database: %v", err)
	}
	got, ok, err := s.Get(ctx, "legacy-1")
	if err != nil || !ok {
		t.Fatalf("legacy offer lost in migration: ok=%v err=%v", ok, err)
	}
	if got.Title != "An old drive" || got.PriceCents != 5000 {
		t.Fatalf("legacy offer data changed: %+v", got)
	}
	if got.Resolution.State != offer.ResolutionUnattempted {
		t.Fatalf("legacy offer state = %q, want unattempted", got.Resolution.State)
	}
	pending, err := s.PendingResolution(ctx, 0, 0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("legacy offer not pending resolution: n=%d err=%v", len(pending), err)
	}
	applied, err := s.RecordResolution(ctx, "legacy-1", got.ProductHint.Fingerprint(),
		offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1", At: time.Unix(1_760_000_000, 0).UTC()})
	if err != nil || !applied {
		t.Fatalf("RecordResolution on migrated row: applied=%v err=%v", applied, err)
	}
	_ = s.Close()

	// The migration must be idempotent: every startup runs it.
	s2, err := New(dsn)
	if err != nil {
		t.Fatalf("second New (migration re-run): %v", err)
	}
	defer s2.Close()
	again, _, _ := s2.Get(ctx, "legacy-1")
	if again.Resolution.ProductID != "p-1" {
		t.Fatalf("resolution lost across reopen: %+v", again.Resolution)
	}
}
