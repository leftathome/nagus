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
	offerstoretest.Run(t, func(t *testing.T) offer.Store {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s, err := New(ctx, dsn)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		// Truncate for isolation: each contract case expects an empty store.
		if _, err := s.pool.Exec(ctx, `TRUNCATE offers`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		t.Cleanup(s.Close)
		return s
	})
}
