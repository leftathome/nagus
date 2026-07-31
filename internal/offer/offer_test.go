package offer

import (
	"context"
	"errors"
	"testing"
	"time"
)

var (
	t0 = time.Unix(1_750_000_000, 0).UTC()
	t1 = t0.Add(24 * time.Hour)
	t2 = t0.Add(48 * time.Hour)
)

func mkOffer(source, key string, price int64, seen time.Time) Offer {
	return Offer{
		SourceID: source, SourceKey: key,
		SourceURL:  "https://example.test/" + key,
		Title:      "A drive",
		PriceCents: price, Currency: "USD", Condition: "refurb",
		Seller:   "vendor-x",
		LastSeen: seen,
	}
}

func mustPut(t *testing.T, s Store, o Offer) {
	t.Helper()
	if err := s.Put(context.Background(), o); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// --- the purchasability rule ---------------------------------------------------

// THE SAFETY RULE: an expired offer is retained as signal but must never be
// recommended for purchase. Query's DEFAULT must therefore exclude it -- the
// dangerous mistake is the one a default must not make.
func TestQueryExcludesExpiredByDefault(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "live", 10000, t1))
	mustPut(t, s, mkOffer("shopify:x", "dead", 8000, t0))

	if _, err := s.MarkExpired(context.Background(), "shopify:x", t1, t2); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}

	got, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].SourceKey != "live" {
		t.Fatalf("default Query returned %d offers %v, want only the purchasable one", len(got), keysOf(got))
	}
	for _, o := range got {
		if !o.Purchasable() {
			t.Errorf("default Query returned a non-purchasable offer %q", o.SourceKey)
		}
	}
}

// The expired offer is RETAINED and reachable when asked for explicitly --
// that is the signal the operator wants ("vendor X ran a deal last week").
func TestExpiredOffersAreRetainedAndReachable(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "dead", 8000, t0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", t1, t2); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}

	if n := s.Len(); n != 1 {
		t.Fatalf("store holds %d offers after expiry, want 1 -- expiry must RETAIN, not delete", n)
	}
	got, err := s.Query(context.Background(), Query{IncludeExpired: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("IncludeExpired returned %d, want 1", len(got))
	}
	o := got[0]
	if o.Purchasable() {
		t.Error("an expired offer must not report as purchasable")
	}
	if o.Status != StatusExpired || o.ExpiredAt.IsZero() {
		t.Errorf("status=%q expiredAt=%v, want expired with a timestamp", o.Status, o.ExpiredAt)
	}
	// The vendor + price evidence must survive: that is what makes it useful.
	if o.Seller != "vendor-x" || o.PriceCents != 8000 {
		t.Errorf("expired offer lost its evidence: seller=%q price=%d", o.Seller, o.PriceCents)
	}
}

func TestMarkExpiredOnlyTouchesTheNamedSource(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:a", "old-a", 100, t0))
	mustPut(t, s, mkOffer("shopify:b", "old-b", 100, t0))
	n, err := s.MarkExpired(context.Background(), "shopify:a", t1, t2)
	if err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d, want 1", n)
	}
	all, _ := s.Query(context.Background(), Query{IncludeExpired: true})
	for _, o := range all {
		want := StatusActive
		if o.SourceID == "shopify:a" {
			want = StatusExpired
		}
		if o.Status != want {
			t.Errorf("%s/%s status=%q want %q", o.SourceID, o.SourceKey, o.Status, want)
		}
	}
}

// A listing that comes back is purchasable again.
func TestReappearanceRevivesAnExpiredOffer(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "k", 9000, t0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", t1, t1); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	mustPut(t, s, mkOffer("shopify:x", "k", 9500, t2)) // seen again

	o, ok, err := s.Get(context.Background(), DeterministicID("shopify:x", "k"))
	if err != nil || !ok {
		t.Fatalf("Get: %v ok=%v", err, ok)
	}
	if !o.Purchasable() {
		t.Fatal("a re-observed offer must become purchasable again")
	}
	if !o.ExpiredAt.IsZero() {
		t.Errorf("ExpiredAt = %v, want cleared on revival", o.ExpiredAt)
	}
}

// --- lifecycle folding ---------------------------------------------------------

func TestPutFoldsLifecycle(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "k", 10000, t0))
	mustPut(t, s, mkOffer("shopify:x", "k", 7000, t1))  // a discount
	mustPut(t, s, mkOffer("shopify:x", "k", 12000, t2)) // then back up

	o, _, _ := s.Get(context.Background(), DeterministicID("shopify:x", "k"))
	if !o.FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen = %v, want %v (must not reset on re-observation)", o.FirstSeen, t0)
	}
	if !o.LastSeen.Equal(t2) {
		t.Errorf("LastSeen = %v, want %v", o.LastSeen, t2)
	}
	if o.PriceCents != 12000 {
		t.Errorf("PriceCents = %d, want the current 12000", o.PriceCents)
	}
	// The point of MinPriceSeen: the discount is still visible after it ended.
	if o.MinPriceSeen != 7000 {
		t.Errorf("MinPriceSeen = %d, want 7000 -- the ended discount must remain visible", o.MinPriceSeen)
	}
}

// An out-of-order write must not drag LastSeen backwards, or the next
// housekeeping pass would expire a live offer.
func TestPutDoesNotRewindLastSeen(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "k", 100, t2))
	mustPut(t, s, mkOffer("shopify:x", "k", 100, t0)) // stale write arrives late
	o, _, _ := s.Get(context.Background(), DeterministicID("shopify:x", "k"))
	if !o.LastSeen.Equal(t2) {
		t.Fatalf("LastSeen = %v, want %v (must only advance)", o.LastSeen, t2)
	}
}

// Price 0 means "unknown", not "free", so it must not become the minimum.
func TestUnknownPriceDoesNotPoisonMinPrice(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "k", 5000, t0))
	mustPut(t, s, mkOffer("shopify:x", "k", 0, t1))
	o, _, _ := s.Get(context.Background(), DeterministicID("shopify:x", "k"))
	if o.MinPriceSeen != 5000 {
		t.Fatalf("MinPriceSeen = %d, want 5000 -- price 0 is unknown, not free", o.MinPriceSeen)
	}
}

// --- retention -----------------------------------------------------------------

func TestRetentionPurgeDeletes(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("ebay:ebay", "old", 100, t0))
	mustPut(t, s, mkOffer("ebay:ebay", "new", 100, t2))
	n, err := s.ApplyRetention(context.Background(), "ebay:ebay", Retention{Policy: Purge, Window: 6 * time.Hour}, t2)
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if n != 1 || s.Len() != 1 {
		t.Fatalf("purged %d leaving %d, want 1 purged leaving 1", n, s.Len())
	}
}

func TestRetentionRetainFullDeletesNothing(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "ancient", 100, t0))
	n, err := s.ApplyRetention(context.Background(), "shopify:x", Retention{Policy: RetainFull}, t2.Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if n != 0 || s.Len() != 1 {
		t.Fatalf("retain-full deleted %d, want 0", n)
	}
}

// Purging is the ONLY thing that deletes -- expiring must not.
func TestExpiryAndRetentionAreIndependent(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:x", "k", 100, t0))
	if _, err := s.MarkExpired(context.Background(), "shopify:x", t2, t2); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	if s.Len() != 1 {
		t.Fatal("expiry deleted a row; expiry must retain")
	}
	if _, err := s.ApplyRetention(context.Background(), "shopify:x", Retention{Policy: RetainFull}, t2); err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if s.Len() != 1 {
		t.Fatal("retain-full deleted an expired row; policy governs deletion, not status")
	}
}

// summarize-decay must FAIL rather than silently behave as purge or retain:
// either silent choice is wrong in a way nobody would notice.
func TestSummarizeDecayIsRejectedNotDowngraded(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("ebay:ebay", "k", 100, t0))
	_, err := s.ApplyRetention(context.Background(), "ebay:ebay", Retention{Policy: SummarizeDecay, Window: time.Hour}, t2)
	if !errors.Is(err, ErrUnsupportedPolicy) {
		t.Fatalf("err = %v, want ErrUnsupportedPolicy", err)
	}
	if s.Len() != 1 {
		t.Fatal("a rejected policy must not have deleted anything")
	}
}

func TestRetentionValidate(t *testing.T) {
	cases := []struct {
		r  Retention
		ok bool
	}{
		{Retention{Policy: RetainFull}, true},
		{Retention{Policy: Purge, Window: 6 * time.Hour}, true},
		{Retention{Policy: Purge}, false}, // no window
		{Retention{Policy: SummarizeDecay, Window: time.Hour}, false},
		{Retention{Policy: "nonsense"}, false},
	}
	for _, c := range cases {
		err := c.r.Validate()
		if (err == nil) != c.ok {
			t.Errorf("Retention%+v Validate = %v, want ok=%v", c.r, err, c.ok)
		}
	}
}

// --- identity + grouping -------------------------------------------------------

func TestDeterministicIDIsStableAndDistinct(t *testing.T) {
	a := DeterministicID("shopify:x", "k")
	if a != DeterministicID("shopify:x", "k") {
		t.Fatal("id is not stable")
	}
	if a == DeterministicID("shopify:y", "k") {
		t.Fatal("different sources must not collide")
	}
	if a == DeterministicID("shopify:x", "k2") {
		t.Fatal("different keys must not collide")
	}
}

func TestComputeProvisionalKey(t *testing.T) {
	cases := []struct {
		name string
		h    ProductHint
		want string
	}{
		{"mpn wins", ProductHint{Brand: "WD", MPN: "WUH722424AL5201", Model: "HC580"}, "mpn:wuh722424al5201"},
		{"brand+model fallback", ProductHint{Brand: "Western Digital", Model: "HC580"}, "bm:westerndigital:hc580"},
		{"punctuation and case normalized", ProductHint{MPN: " wuh-722424 "}, "mpn:wuh722424"},
		{"nothing to key on", ProductHint{Brand: "WD"}, ""},
		{"empty", ProductHint{}, ""},
	}
	for _, c := range cases {
		if got := ComputeProvisionalKey(c.h); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// Grouping across sellers is the whole point of the provisional key.
func TestQueryByProvisionalKeyGroupsAcrossSellers(t *testing.T) {
	s := NewMemoryStore()
	for i, src := range []string{"shopify:a", "shopify:b", "ebay:ebay"} {
		o := mkOffer(src, "k", int64(10000+i*100), t1)
		o.ProvisionalKey = "mpn:abc123"
		o.Seller = src
		mustPut(t, s, o)
	}
	other := mkOffer("shopify:a", "other", 999, t1)
	other.ProvisionalKey = "mpn:zzz"
	mustPut(t, s, other)

	got, err := s.Query(context.Background(), Query{ProvisionalKey: "mpn:abc123"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d offers for the group, want 3 across sellers: %v", len(got), keysOf(got))
	}
}

// --- validation ----------------------------------------------------------------

func TestValidate(t *testing.T) {
	s := NewMemoryStore()
	bad := []struct {
		name string
		o    Offer
		want error
	}{
		{"no source", Offer{SourceKey: "k"}, ErrNoSourceID},
		{"no key", Offer{SourceID: "s"}, ErrNoSourceKey},
		{"negative price", Offer{SourceID: "s", SourceKey: "k", PriceCents: -1}, ErrNegPrice},
	}
	for _, c := range bad {
		if err := s.Put(context.Background(), c.o); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// Price 0 is legal: unpriced listings are common and must still be stored.
	if err := s.Put(context.Background(), mkOffer("s", "k", 0, t0)); err != nil {
		t.Errorf("price 0 must be accepted as unknown: %v", err)
	}
}

func TestQueryFiltersAndLimit(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("shopify:a", "k1", 100, t0))
	mustPut(t, s, mkOffer("shopify:a", "k2", 100, t1))
	mustPut(t, s, mkOffer("shopify:b", "k3", 100, t2))

	bySource, _ := s.Query(context.Background(), Query{SourceID: "shopify:a"})
	if len(bySource) != 2 {
		t.Errorf("SourceID filter returned %d, want 2", len(bySource))
	}
	since, _ := s.Query(context.Background(), Query{Since: t1})
	if len(since) != 2 {
		t.Errorf("Since filter returned %d, want 2", len(since))
	}
	lim, _ := s.Query(context.Background(), Query{Limit: 1})
	if len(lim) != 1 {
		t.Errorf("Limit returned %d, want 1", len(lim))
	}
	// Most-recently-seen first.
	all, _ := s.Query(context.Background(), Query{})
	if all[0].SourceKey != "k3" {
		t.Errorf("ordering: first = %q, want the most recently seen k3", all[0].SourceKey)
	}
}

func keysOf(os []Offer) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.SourceKey)
	}
	return out
}

// --- outcome: a listing is not a transaction ----------------------------------

// The mere existence of an offer is weak price evidence -- anyone can list
// anything at any price. The model must distinguish "was offered" from "was
// offered AND fulfilled", or downstream cannot weigh them differently.
func TestOutcomeDistinguishesListedFromSold(t *testing.T) {
	s := NewMemoryStore()
	listed := mkOffer("ebay:ebay", "listed", 5000, t0) // never observed ending
	sold := mkOffer("ebay:ebay", "sold", 5000, t0)
	sold.Outcome = OutcomeSold
	unsold := mkOffer("ebay:ebay", "unsold", 5000, t0)
	unsold.Outcome = OutcomeUnsold
	mustPut(t, s, listed)
	mustPut(t, s, sold)
	mustPut(t, s, unsold)

	got := map[string]Outcome{}
	all, _ := s.Query(context.Background(), Query{IncludeExpired: true})
	for _, o := range all {
		got[o.SourceKey] = o.Outcome
	}
	if got["listed"] != OutcomeUnknown {
		t.Errorf("an offer never observed ending must default to OutcomeUnknown, got %q", got["listed"])
	}
	if got["sold"] != OutcomeSold || got["unsold"] != OutcomeUnsold {
		t.Errorf("outcomes did not round-trip: %v", got)
	}
}

// Outcome and Status are independent axes: an offer can be expired (not
// purchasable) with an unknown outcome, which is the common case.
func TestOutcomeIsIndependentOfPurchasability(t *testing.T) {
	s := NewMemoryStore()
	mustPut(t, s, mkOffer("ebay:ebay", "k", 5000, t0))
	if _, err := s.MarkExpired(context.Background(), "ebay:ebay", t1, t1); err != nil {
		t.Fatalf("MarkExpired: %v", err)
	}
	o, _, _ := s.Get(context.Background(), DeterministicID("ebay:ebay", "k"))
	if o.Purchasable() {
		t.Fatal("expired offer should not be purchasable")
	}
	if o.Outcome != OutcomeUnknown {
		t.Errorf("expiry must not imply an outcome; got %q -- we did not observe a sale", o.Outcome)
	}
}

// Seller retention is a PER-SOURCE terms question, defaulting to the safe answer.
func TestRetainSellerDefaultsOff(t *testing.T) {
	if (Retention{Policy: Purge, Window: time.Hour}).RetainSeller {
		t.Fatal("RetainSeller must default to false: some sources permit recording that an offer was made but not tying a seller to it")
	}
}
