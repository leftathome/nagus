package pgoffer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pgtest"
)

// Price history (nagus-0ta) on a database that predates it: New adds the
// table and seeds NOTHING (no row per existing offer); the first change of an
// existing offer anchors the price it leaves at its stored price_changed_at (a
// pre-column 0 read as first_seen) before the change itself.
func TestMigrationAddsPriceHistoryWithoutSeeding(t *testing.T) {
	dsn := pgtest.DSN(t, "pgoffer_migrate_history")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(ctx, `DROP TABLE IF EXISTS offer_price_history;`+preResolutionSchema+preTimingRows); err != nil {
		t.Fatalf("build pre-history table: %v", err)
	}
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New on a pre-history table: %v", err)
	}
	defer s.Close()
	var rows int
	if err := raw.QueryRow(ctx, `SELECT count(*) FROM offer_price_history`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("history rows after migration = %d (err %v), want 0: nothing is seeded", rows, err)
	}
	first := time.Unix(0, 1740000000000000000).UTC()
	now := time.Unix(0, 1760000000000000000).UTC()
	o := offer.Offer{ID: "timed-1", SourceID: "shopify:old", SourceKey: "t1", PriceCents: 4000, LastSeen: now}
	if err := s.Put(ctx, o); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.PriceHistory(ctx, []string{"timed-1"}, time.Time{}, 0); len(h) != 0 {
		t.Fatalf("a same-price poll wrote history: %+v", h)
	}
	o.PriceCents, o.LastSeen = 3000, now.Add(time.Hour)
	if err := s.Put(ctx, o); err != nil {
		t.Fatal(err)
	}
	h, err := s.PriceHistory(ctx, []string{"timed-1"}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := h["timed-1"]
	if len(got) != 2 || !got[0].ObservedAt.Equal(first) || got[0].PriceCents != 4000 ||
		!got[1].ObservedAt.Equal(now.Add(time.Hour)) || got[1].PriceCents != 3000 {
		t.Fatalf("history after the first change = %+v, want anchor 4000 at first_seen then 3000", got)
	}
	// Idempotent: every startup re-runs the schema.
	s2, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	s2.Close()
}
