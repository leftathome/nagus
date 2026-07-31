package pgoffer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/offer/offerstoretest"
)

// The Postgres adapter is correct when it passes the SAME contract MemoryStore
// and the SQLite adapter pass. Gated on NAGUS_TEST_POSTGRES_DSN so the suite
// stays green on machines with no Postgres, matching internal/store/postgresstore.
func TestPostgresOfferStoreSatisfiesTheContract(t *testing.T) {
	dsn := os.Getenv("NAGUS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set NAGUS_TEST_POSTGRES_DSN to run postgres offer contract tests")
	}
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
