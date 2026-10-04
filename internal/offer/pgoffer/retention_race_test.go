package pgoffer

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pgtest"
)

// A Put that commits a history row while ApplyRetention's offers DELETE waits
// on its row lock -- with a last_seen still older than the cutoff, so the
// offer is deleted anyway -- must not leave that row orphaned. A single
// statement (one snapshot) deleting offers and their history did (review of
// !38). The interleaving is forced, not timed: an open transaction plays the
// Put and holds the offer's lock; the test waits until pg_stat_activity shows
// the retention DELETE blocked on a lock, then commits.
func TestRetentionDeletesHistoryCommittedWhileItWaited(t *testing.T) {
	dsn := pgtest.DSN(t, "pgoffer_retention_race")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.pool.Exec(ctx, `DELETE FROM offers; DELETE FROM offer_price_history`); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_750_000_000, 0).UTC()
	id := offer.DeterministicID("ebay:race", "stale")
	if err := s.Put(ctx, offer.Offer{SourceID: "ebay:race", SourceKey: "stale", PriceCents: 1000, Currency: "USD", LastSeen: t0}); err != nil {
		t.Fatal(err)
	}

	// The "Put": lock the offer, write a history row, do not commit yet.
	put, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = put.Rollback(context.Background()) }()
	if _, err := put.Exec(ctx, `SELECT 1 FROM offers WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := put.Exec(ctx, `INSERT INTO offer_price_history (offer_id, observed_at_ns, price_cents, currency) VALUES ($1, $2, 900, 'USD')`,
		id, t0.Add(time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := s.ApplyRetention(ctx, "ebay:race", offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}, t0.Add(24*time.Hour))
		done <- result{n, err}
	}()

	// Wait until the retention DELETE is blocked on the offer's lock.
	for {
		var waiting bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%DELETE FROM offers WHERE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("ApplyRetention finished before it could block (n=%d err=%v)", r.n, r.err)
		case <-ctx.Done():
			t.Fatal("ApplyRetention never blocked on the offer lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := put.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.n != 1 {
		t.Fatalf("ApplyRetention = %d, %v; want the stale offer purged", r.n, r.err)
	}
	var orphans int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM offer_price_history WHERE offer_id = $1`, id).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d history rows outlived their purged offer", orphans)
	}
}
