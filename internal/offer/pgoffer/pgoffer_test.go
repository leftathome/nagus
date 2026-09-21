package pgoffer

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/offer/offerstoretest"
	"github.com/leftathome/nagus/internal/pgtest"
)

// The Postgres adapter is correct when it passes the SAME contract MemoryStore
// and the SQLite adapter pass. Gated on NAGUS_TEST_POSTGRES_DSN so the suite
// stays green on machines with no Postgres, matching internal/store/postgresstore.
func TestPostgresOfferStoreSatisfiesTheContract(t *testing.T) {
	// A database private to this package (nagus-0wj): packages run in
	// parallel and must not truncate each other's tables.
	dsn := pgtest.DSN(t, "pgoffer")
	// ONE store for the whole contract run. New runs schema DDL, and ALTER TABLE
	// ... ADD COLUMN IF NOT EXISTS takes an ACCESS EXCLUSIVE lock even when it
	// adds nothing; running it (and TRUNCATE, the same lock) before every case
	// meant any lingering reader stalled setup to its deadline in CI (nagus-0wj,
	// pipeline #2672). Cases are isolated with DELETE, which readers never block.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	s, err := New(ctx, dsn)
	cancel()
	if err != nil {
		pgtest.LogActivity(t, dsn)
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	offerstoretest.Run(t, func(t *testing.T) offer.Store {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := s.pool.Exec(ctx, `DELETE FROM offers`); err != nil {
			pgtest.LogActivity(t, dsn)
			t.Fatalf("reset offers: %v", err)
		}
		return s
	})
}
