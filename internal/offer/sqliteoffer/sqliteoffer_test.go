package sqliteoffer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/offer/offerstoretest"
)

// The SQLite adapter is correct when it passes the SAME contract MemoryStore
// passes. The suite is the specification; this package is one implementation.
func TestSQLiteOfferStoreSatisfiesTheContract(t *testing.T) {
	offerstoretest.Run(t, func(t *testing.T) offer.Store {
		s, err := New(filepath.Join(t.TempDir(), "offers.db"))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

// Persistence is the whole reason this adapter exists: lifecycle folded across
// process restarts, not just within one.
func TestLifecycleSurvivesReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "offers.db")
	t0 := offerstoretest.T0
	t1 := offerstoretest.T1

	s1, err := New(dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s1.Put(context.Background(), offerstoretest.Offer("shopify:x", "k", 10000, t0)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s1.Put(context.Background(), offerstoretest.Offer("shopify:x", "k", 6000, t1)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := New(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	o, ok, err := s2.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if err != nil || !ok {
		t.Fatalf("Get after reopen: %v ok=%v", err, ok)
	}
	if !o.FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen = %v, want %v across restart", o.FirstSeen, t0)
	}
	if o.MinPriceSeen != 6000 {
		t.Errorf("MinPriceSeen = %d, want 6000 -- the discount must survive a restart", o.MinPriceSeen)
	}

	// And a THIRD observation at a higher price must not lose the old minimum.
	if err := s2.Put(context.Background(), offerstoretest.Offer("shopify:x", "k", 12000, t1.Add(time.Hour))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	o, _, _ = s2.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if o.MinPriceSeen != 6000 {
		t.Errorf("MinPriceSeen = %d after a price rise, want 6000", o.MinPriceSeen)
	}
	if o.PriceCents != 12000 {
		t.Errorf("PriceCents = %d, want the current 12000", o.PriceCents)
	}
}
