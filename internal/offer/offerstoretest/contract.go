// Package offerstoretest holds the reference contract every offer.Store adapter
// must satisfy.
//
// MemoryStore is the reference implementation; a persistent adapter is correct
// when it passes these same tests, exactly as the item store's adapters do. The
// suite lives in its own package so both the in-tree MemoryStore tests and an
// out-of-package adapter (sqlitestore) can run it without an import cycle.
package offerstoretest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// Times used across the contract.
var (
	T0 = time.Unix(1_750_000_000, 0).UTC()
	T1 = T0.Add(24 * time.Hour)
	T2 = T0.Add(48 * time.Hour)
)

// NewStore builds a fresh, empty store for one subtest.
type NewStore func(t *testing.T) offer.Store

// Offer builds a plain offer for the contract's fixtures.
func Offer(source, key string, price int64, seen time.Time) offer.Offer {
	return offer.Offer{
		SourceID: source, SourceKey: key,
		SourceURL:  "https://example.test/" + key,
		Title:      "A drive",
		PriceCents: price, Currency: "USD", Condition: "refurb",
		Seller:   "vendor-x",
		LastSeen: seen,
	}
}

func put(t *testing.T, s offer.Store, o offer.Offer) {
	t.Helper()
	if err := s.Put(context.Background(), o); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func count(t *testing.T, s offer.Store) int {
	t.Helper()
	all, err := s.Query(context.Background(), offer.Query{IncludeExpired: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return len(all)
}

// Run executes the whole contract against the store built by newStore.
func Run(t *testing.T, newStore NewStore) {
	t.Helper()
	t.Run("QueryExcludesExpiredByDefault", func(t *testing.T) { queryExcludesExpiredByDefault(t, newStore(t)) })
	t.Run("ExpiredRetainedAndReachable", func(t *testing.T) { expiredRetainedAndReachable(t, newStore(t)) })
	t.Run("MarkExpiredScopedToSource", func(t *testing.T) { markExpiredScopedToSource(t, newStore(t)) })
	t.Run("ReappearanceRevives", func(t *testing.T) { reappearanceRevives(t, newStore(t)) })
	t.Run("PutFoldsLifecycle", func(t *testing.T) { putFoldsLifecycle(t, newStore(t)) })
	t.Run("PutDoesNotRewindLastSeen", func(t *testing.T) { putDoesNotRewindLastSeen(t, newStore(t)) })
	t.Run("UnknownPriceDoesNotPoisonMin", func(t *testing.T) { unknownPriceDoesNotPoisonMin(t, newStore(t)) })
	t.Run("RetentionPurgeDeletes", func(t *testing.T) { retentionPurgeDeletes(t, newStore(t)) })
	t.Run("RetentionRetainFullKeeps", func(t *testing.T) { retentionRetainFullKeeps(t, newStore(t)) })
	t.Run("ExpiryAndRetentionIndependent", func(t *testing.T) { expiryAndRetentionIndependent(t, newStore(t)) })
	t.Run("SummarizeDecayRejected", func(t *testing.T) { summarizeDecayRejected(t, newStore(t)) })
	t.Run("OutcomeRoundTrips", func(t *testing.T) { outcomeRoundTrips(t, newStore(t)) })
	t.Run("GroupsByProvisionalKey", func(t *testing.T) { groupsByProvisionalKey(t, newStore(t)) })
	t.Run("Validation", func(t *testing.T) { validation(t, newStore(t)) })
	t.Run("FiltersAndOrdering", func(t *testing.T) { filtersAndOrdering(t, newStore(t)) })
	t.Run("AspectsRoundTrip", func(t *testing.T) { aspectsRoundTrip(t, newStore(t)) })
	t.Run("ConcurrentWritesFromManySources", func(t *testing.T) { concurrentWrites(t, newStore(t)) })
}

// offer.Store documents that implementations MUST be safe for concurrent use,
// and nagus runs ONE INGEST GOROUTINE PER SOURCE all writing the same store, so
// the contract has to actually exercise that. A single-threaded suite let a real
// SQLITE_BUSY bug through: offers were silently dropped whenever two sources
// ingested at once.
func concurrentWrites(t *testing.T, s offer.Store) {
	const sources, perSource = 4, 25
	var wg sync.WaitGroup
	errs := make(chan error, sources*perSource)
	for src := 0; src < sources; src++ {
		wg.Add(1)
		go func(src int) {
			defer wg.Done()
			id := fmt.Sprintf("shopify:s%d", src)
			for i := 0; i < perSource; i++ {
				o := Offer(id, fmt.Sprintf("k%d", i), int64(1000+i), T1)
				if err := s.Put(context.Background(), o); err != nil {
					errs <- err
					return
				}
			}
		}(src)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Put failed: %v", err)
	}
	if got := count(t, s); got != sources*perSource {
		t.Fatalf("stored %d offers, want %d -- concurrent writes were dropped", got, sources*perSource)
	}
}

// THE SAFETY RULE: an expired offer must never reach a purchase recommendation,
// so the DEFAULT query excludes it.
func queryExcludesExpiredByDefault(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "live", 10000, T1))
	put(t, s, Offer("shopify:x", "dead", 8000, T0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", T1, T2); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	got, err := s.Query(context.Background(), offer.Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].SourceKey != "live" {
		t.Fatalf("default Query returned %d, want only the purchasable one", len(got))
	}
	for _, o := range got {
		if !o.Purchasable() {
			t.Errorf("default Query returned non-purchasable %q", o.SourceKey)
		}
	}
}

// Expiry RETAINS: the evidence survives and is reachable on request.
func expiredRetainedAndReachable(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "dead", 8000, T0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", T1, T2); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if n := count(t, s); n != 1 {
		t.Fatalf("holds %d after expiry, want 1 -- expiry must RETAIN", n)
	}
	got, _ := s.Query(context.Background(), offer.Query{IncludeExpired: true})
	o := got[0]
	if o.Purchasable() || o.Status != offer.StatusExpired || o.ExpiredAt.IsZero() {
		t.Errorf("status=%q expiredAt=%v purchasable=%v", o.Status, o.ExpiredAt, o.Purchasable())
	}
	if o.Seller != "vendor-x" || o.PriceCents != 8000 {
		t.Errorf("expired offer lost evidence: seller=%q price=%d", o.Seller, o.PriceCents)
	}
}

func markExpiredScopedToSource(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:a", "old-a", 100, T0))
	put(t, s, Offer("shopify:b", "old-b", 100, T0))
	n, err := s.MarkExpired(context.Background(), "shopify:a", T1, T2)
	if err != nil || n != 1 {
		t.Fatalf("MarkExpired = %d, %v; want 1, nil", n, err)
	}
	all, _ := s.Query(context.Background(), offer.Query{IncludeExpired: true})
	for _, o := range all {
		want := offer.StatusActive
		if o.SourceID == "shopify:a" {
			want = offer.StatusExpired
		}
		if o.Status != want {
			t.Errorf("%s/%s status=%q want %q", o.SourceID, o.SourceKey, o.Status, want)
		}
	}
}

func reappearanceRevives(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "k", 9000, T0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", T1, T1); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	put(t, s, Offer("shopify:x", "k", 9500, T2))
	o, ok, err := s.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	if !o.Purchasable() || !o.ExpiredAt.IsZero() {
		t.Fatalf("revival failed: status=%q expiredAt=%v", o.Status, o.ExpiredAt)
	}
}

func putFoldsLifecycle(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "k", 10000, T0))
	put(t, s, Offer("shopify:x", "k", 7000, T1))
	put(t, s, Offer("shopify:x", "k", 12000, T2))
	o, _, _ := s.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if !o.FirstSeen.Equal(T0) {
		t.Errorf("FirstSeen = %v, want %v", o.FirstSeen, T0)
	}
	if !o.LastSeen.Equal(T2) {
		t.Errorf("LastSeen = %v, want %v", o.LastSeen, T2)
	}
	if o.PriceCents != 12000 {
		t.Errorf("PriceCents = %d, want 12000", o.PriceCents)
	}
	if o.MinPriceSeen != 7000 {
		t.Errorf("MinPriceSeen = %d, want 7000 -- an ended discount must stay visible", o.MinPriceSeen)
	}
}

func putDoesNotRewindLastSeen(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "k", 100, T2))
	put(t, s, Offer("shopify:x", "k", 100, T0))
	o, _, _ := s.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if !o.LastSeen.Equal(T2) {
		t.Fatalf("LastSeen = %v, want %v (must only advance)", o.LastSeen, T2)
	}
}

func unknownPriceDoesNotPoisonMin(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "k", 5000, T0))
	put(t, s, Offer("shopify:x", "k", 0, T1))
	o, _, _ := s.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if o.MinPriceSeen != 5000 {
		t.Fatalf("MinPriceSeen = %d, want 5000 -- 0 is unknown, not free", o.MinPriceSeen)
	}
}

func retentionPurgeDeletes(t *testing.T, s offer.Store) {
	put(t, s, Offer("ebay:ebay", "old", 100, T0))
	put(t, s, Offer("ebay:ebay", "new", 100, T2))
	n, err := s.ApplyRetention(context.Background(), "ebay:ebay", offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}, T2)
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if n != 1 || count(t, s) != 1 {
		t.Fatalf("purged %d leaving %d, want 1 and 1", n, count(t, s))
	}
}

func retentionRetainFullKeeps(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "ancient", 100, T0))
	n, err := s.ApplyRetention(context.Background(), "shopify:x", offer.Retention{Policy: offer.RetainFull}, T2.Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if n != 0 || count(t, s) != 1 {
		t.Fatalf("retain-full deleted %d", n)
	}
}

// Retention is the ONLY thing that deletes; expiry never is.
func expiryAndRetentionIndependent(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:x", "k", 100, T0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", T2, T2); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if count(t, s) != 1 {
		t.Fatal("expiry deleted a row")
	}
	if _, err := s.ApplyRetention(context.Background(), "shopify:x", offer.Retention{Policy: offer.RetainFull}, T2); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if count(t, s) != 1 {
		t.Fatal("retain-full deleted an expired row; policy governs deletion, not status")
	}
}

func summarizeDecayRejected(t *testing.T, s offer.Store) {
	put(t, s, Offer("ebay:ebay", "k", 100, T0))
	_, err := s.ApplyRetention(context.Background(), "ebay:ebay", offer.Retention{Policy: offer.SummarizeDecay, Window: time.Hour}, T2)
	if !errors.Is(err, offer.ErrUnsupportedPolicy) {
		t.Fatalf("err = %v, want ErrUnsupportedPolicy", err)
	}
	if count(t, s) != 1 {
		t.Fatal("a rejected policy must not delete")
	}
}

// A listing is not a transaction: the model must keep the distinction.
func outcomeRoundTrips(t *testing.T, s offer.Store) {
	listed := Offer("ebay:ebay", "listed", 5000, T0)
	sold := Offer("ebay:ebay", "sold", 5000, T0)
	sold.Outcome = offer.OutcomeSold
	unsold := Offer("ebay:ebay", "unsold", 5000, T0)
	unsold.Outcome = offer.OutcomeUnsold
	put(t, s, listed)
	put(t, s, sold)
	put(t, s, unsold)
	got := map[string]offer.Outcome{}
	all, _ := s.Query(context.Background(), offer.Query{IncludeExpired: true})
	for _, o := range all {
		got[o.SourceKey] = o.Outcome
	}
	if got["listed"] != offer.OutcomeUnknown || got["sold"] != offer.OutcomeSold || got["unsold"] != offer.OutcomeUnsold {
		t.Fatalf("outcomes did not round-trip: %v", got)
	}
}

func groupsByProvisionalKey(t *testing.T, s offer.Store) {
	for i, src := range []string{"shopify:a", "shopify:b", "ebay:ebay"} {
		o := Offer(src, "k", int64(10000+i*100), T1)
		o.ProvisionalKey = "mpn:abc123"
		o.Seller = src
		put(t, s, o)
	}
	other := Offer("shopify:a", "other", 999, T1)
	other.ProvisionalKey = "mpn:zzz"
	put(t, s, other)
	got, err := s.Query(context.Background(), offer.Query{ProvisionalKey: "mpn:abc123"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("group returned %d, want 3 across sellers", len(got))
	}
}

func validation(t *testing.T, s offer.Store) {
	bad := []struct {
		name string
		o    offer.Offer
		want error
	}{
		{"no source", offer.Offer{SourceKey: "k"}, offer.ErrNoSourceID},
		{"no key", offer.Offer{SourceID: "s"}, offer.ErrNoSourceKey},
		{"negative price", offer.Offer{SourceID: "s", SourceKey: "k", PriceCents: -1}, offer.ErrNegPrice},
	}
	for _, c := range bad {
		if err := s.Put(context.Background(), c.o); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if err := s.Put(context.Background(), Offer("s", "k", 0, T0)); err != nil {
		t.Errorf("price 0 must be accepted as unknown: %v", err)
	}
}

func filtersAndOrdering(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:a", "k1", 100, T0))
	put(t, s, Offer("shopify:a", "k2", 100, T1))
	put(t, s, Offer("shopify:b", "k3", 100, T2))
	if got, _ := s.Query(context.Background(), offer.Query{SourceID: "shopify:a"}); len(got) != 2 {
		t.Errorf("SourceID filter returned %d, want 2", len(got))
	}
	if got, _ := s.Query(context.Background(), offer.Query{Since: T1}); len(got) != 2 {
		t.Errorf("Since filter returned %d, want 2", len(got))
	}
	if got, _ := s.Query(context.Background(), offer.Query{Limit: 1}); len(got) != 1 {
		t.Errorf("Limit returned %d, want 1", len(got))
	}
	all, _ := s.Query(context.Background(), offer.Query{})
	if len(all) != 3 || all[0].SourceKey != "k3" {
		t.Errorf("ordering: want most-recently-seen k3 first, got %d rows", len(all))
	}
}

// Aspects are UNTRUSTED values but must survive persistence intact.
func aspectsRoundTrip(t *testing.T, s offer.Store) {
	o := Offer("shopify:x", "k", 100, T0)
	o.Aspects = map[string]string{"vendor": "Western Digital", "sku": "WUH-1", "weird": `quote" and \ backslash`}
	o.ProductHint = offer.ProductHint{Brand: "Western Digital", MPN: "WUH722424AL5201", Model: "HC580"}
	put(t, s, o)
	got, ok, err := s.Get(context.Background(), offer.DeterministicID("shopify:x", "k"))
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	for k, want := range o.Aspects {
		if got.Aspects[k] != want {
			t.Errorf("Aspects[%q] = %q, want %q", k, got.Aspects[k], want)
		}
	}
	if got.ProductHint != o.ProductHint {
		t.Errorf("ProductHint = %+v, want %+v", got.ProductHint, o.ProductHint)
	}
}
