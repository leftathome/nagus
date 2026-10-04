package offerstoretest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// The offer-timing contract (nagus-417): price_changed_at, previous price,
// a source-stated end time, and the batched Summaries read the surface uses
// to hide expired listings. See offer.Store.Put for the rule.

// runTiming is called from Run.
func runTiming(t *testing.T, newStore NewStore) {
	t.Run("PriceChangeFirstInsertIsFirstSeen", func(t *testing.T) { priceChangeFirstInsert(t, newStore(t)) })
	t.Run("PriceChangeSamePriceKeepsStamp", func(t *testing.T) { priceChangeSamePrice(t, newStore(t)) })
	t.Run("PriceChangeLowerStampsAndRecordsPrevious", func(t *testing.T) { priceChangeLower(t, newStore(t)) })
	t.Run("PriceChangeHigherStampsAndIsNotADrop", func(t *testing.T) { priceChangeHigher(t, newStore(t)) })
	t.Run("PriceChangeZeroKeepsLastKnownPrice", func(t *testing.T) { priceChangeZero(t, newStore(t)) })
	t.Run("PriceChangeZeroRoundTripIsNoChange", func(t *testing.T) { priceChangeZeroRoundTrip(t, newStore(t)) })
	t.Run("PriceChangeCannotBeSetByCaller", func(t *testing.T) { priceChangeCallerIgnored(t, newStore(t)) })
	t.Run("PriceChangeNeverRewinds", func(t *testing.T) { priceChangeNeverRewinds(t, newStore(t)) })
	t.Run("EndsAtRoundTripsAndClears", func(t *testing.T) { endsAtRoundTrips(t, newStore(t)) })
	t.Run("SummariesBatchIncludesExpired", func(t *testing.T) { summariesBatch(t, newStore(t)) })
	t.Run("SummariesManyIDs", func(t *testing.T) { summariesMany(t, newStore(t)) })
	t.Run("SummariesEmpty", func(t *testing.T) { summariesEmpty(t, newStore(t)) })
}

func getByID(t *testing.T, s offer.Store, id string) offer.Offer {
	t.Helper()
	o, ok, err := s.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("Get(%s): ok=%v err=%v", id, ok, err)
	}
	return o
}

func idOf(o offer.Offer) string { return offer.DeterministicID(o.SourceID, o.SourceKey) }

func priceChangeFirstInsert(t *testing.T, s offer.Store) {
	o := Offer("shopify:a", "k", 1000, T0)
	put(t, s, o)
	got := getByID(t, s, idOf(o))
	if !got.PriceChangedAt.Equal(T0) || got.PreviousPriceCents != 0 {
		t.Fatalf("first insert: price_changed_at=%v previous=%d, want %v and 0", got.PriceChangedAt, got.PreviousPriceCents, T0)
	}
	if !got.PriceChangedAt.Equal(got.FirstSeen) {
		t.Fatalf("first insert: price_changed_at %v != first_seen %v", got.PriceChangedAt, got.FirstSeen)
	}
}

func priceChangeSamePrice(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 1000, T1))
	got := getByID(t, s, offer.DeterministicID("shopify:a", "k"))
	if !got.PriceChangedAt.Equal(T0) || got.PreviousPriceCents != 0 {
		t.Fatalf("same price re-seen: price_changed_at=%v previous=%d, want %v and 0", got.PriceChangedAt, got.PreviousPriceCents, T0)
	}
	if !got.LastSeen.Equal(T1) {
		t.Fatalf("last_seen = %v, want %v", got.LastSeen, T1)
	}
}

func priceChangeLower(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 800, T1))
	got := getByID(t, s, offer.DeterministicID("shopify:a", "k"))
	if !got.PriceChangedAt.Equal(T1) || got.PreviousPriceCents != 1000 || got.PriceCents != 800 {
		t.Fatalf("drop: %+v", got)
	}
	if !got.Summarize().PriceDropped() {
		t.Fatal("a 1000 -> 800 change must read as a drop")
	}
	// A later poll at the same price keeps the stamp and the previous price.
	put(t, s, Offer("shopify:a", "k", 800, T2))
	again := getByID(t, s, offer.DeterministicID("shopify:a", "k"))
	if !again.PriceChangedAt.Equal(T1) || again.PreviousPriceCents != 1000 {
		t.Fatalf("re-seen after drop: price_changed_at=%v previous=%d", again.PriceChangedAt, again.PreviousPriceCents)
	}
}

func priceChangeHigher(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 1200, T1))
	got := getByID(t, s, offer.DeterministicID("shopify:a", "k"))
	if !got.PriceChangedAt.Equal(T1) || got.PreviousPriceCents != 1000 {
		t.Fatalf("rise: %+v", got)
	}
	if got.Summarize().PriceDropped() {
		t.Fatal("a 1000 -> 1200 change must not read as a drop")
	}
	if got.MinPriceSeen != 1000 {
		t.Fatalf("min price = %d, want 1000", got.MinPriceSeen)
	}
}

func priceChangeZero(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "k")
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 0, T1))
	got := getByID(t, s, id)
	if !got.PriceChangedAt.Equal(T0) || got.PreviousPriceCents != 0 || got.PriceCents != 1000 {
		t.Fatalf("unknown price must not be a change nor erase the last known price: %+v", got)
	}
	// Known again after unknown, lower: a drop FROM THE LAST KNOWN price.
	put(t, s, Offer("shopify:a", "k", 900, T2))
	got = getByID(t, s, id)
	if !got.PriceChangedAt.Equal(T2) || got.PreviousPriceCents != 1000 || got.PriceCents != 900 || !got.Summarize().PriceDropped() {
		t.Fatalf("1000 -> 0 -> 900 must be a drop from 1000: %+v", got)
	}
}

// 1000 -> 0 -> 1000 is no change at all, and a price that first becomes
// known after unknown polls is not a change either.
func priceChangeZeroRoundTrip(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "k")
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 0, T1))
	put(t, s, Offer("shopify:a", "k", 1000, T2))
	if got := getByID(t, s, id); !got.PriceChangedAt.Equal(T0) || got.PreviousPriceCents != 0 {
		t.Fatalf("1000 -> 0 -> 1000 stamped a change: %+v", got)
	}
	unk := offer.DeterministicID("shopify:a", "u")
	put(t, s, Offer("shopify:a", "u", 0, T0))
	put(t, s, Offer("shopify:a", "u", 900, T1))
	if got := getByID(t, s, unk); !got.PriceChangedAt.Equal(T0) || got.PreviousPriceCents != 0 || got.PriceCents != 900 {
		t.Fatalf("first known price after unknown: %+v", got)
	}
}

func priceChangeCallerIgnored(t *testing.T, s offer.Store) {
	o := Offer("shopify:a", "k", 1000, T1)
	o.PriceChangedAt = T2
	o.PreviousPriceCents = 5000
	put(t, s, o)
	got := getByID(t, s, idOf(o))
	if !got.PriceChangedAt.Equal(T1) || got.PreviousPriceCents != 0 {
		t.Fatalf("caller-supplied price-change fields were stored: %+v", got)
	}
}

func priceChangeNeverRewinds(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "k")
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 800, T2))
	// A late, out-of-order write with another price must not move the stamp
	// back to T1.
	put(t, s, Offer("shopify:a", "k", 900, T1))
	got := getByID(t, s, id)
	if got.PriceChangedAt.Before(T2) {
		t.Fatalf("price_changed_at rewound to %v", got.PriceChangedAt)
	}
}

func endsAtRoundTrips(t *testing.T, s offer.Store) {
	end := T2.Add(3 * time.Hour)
	o := Offer("ebay:x", "v1|1|0", 1000, T0)
	o.EndsAt = end
	put(t, s, o)
	if got := getByID(t, s, idOf(o)); !got.EndsAt.Equal(end) {
		t.Fatalf("ends_at = %v, want %v", got.EndsAt, end)
	}
	// The source stops stating an end: the offer stops carrying one.
	o2 := Offer("ebay:x", "v1|1|0", 1000, T1)
	put(t, s, o2)
	if got := getByID(t, s, idOf(o2)); !got.EndsAt.IsZero() {
		t.Fatalf("ends_at kept after the source stopped stating it: %v", got.EndsAt)
	}
}

func summariesBatch(t *testing.T, s offer.Store) {
	ctx := context.Background()
	a := Offer("shopify:a", "a", 1000, T0)
	b := Offer("shopify:a", "b", 2000, T2)
	b.EndsAt = T2.Add(time.Hour)
	c := Offer("shopify:c", "c", 3000, T0)
	for _, o := range []offer.Offer{a, b, c} {
		put(t, s, o)
	}
	put(t, s, Offer("shopify:a", "a", 700, T1)) // a drops
	if _, err := s.MarkExpired(ctx, "shopify:c", T1, T2); err != nil {
		t.Fatal(err)
	}
	got, err := s.Summaries(ctx, []string{idOf(a), idOf(b), idOf(c), "no-such-offer", idOf(a)})
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d summaries, want 3 (unknown id absent, duplicate collapsed): %+v", len(got), got)
	}
	sa := got[idOf(a)]
	if sa.Status != offer.StatusActive || sa.PriceCents != 700 || sa.PreviousPriceCents != 1000 ||
		!sa.PriceChangedAt.Equal(T1) || !sa.FirstSeen.Equal(T0) || !sa.LastSeen.Equal(T1) ||
		sa.MinPriceSeen != 700 || sa.SourceID != "shopify:a" || !sa.PriceDropped() {
		t.Fatalf("summary a = %+v", sa)
	}
	if sb := got[idOf(b)]; !sb.EndsAt.Equal(T2.Add(time.Hour)) || !sb.PriceChangedAt.Equal(T2) {
		t.Fatalf("summary b = %+v", sb)
	}
	sc := got[idOf(c)]
	if sc.Status != offer.StatusExpired || !sc.ExpiredAt.Equal(T2) {
		t.Fatalf("summary c (expired) = %+v", sc)
	}
	if sc.Resolution.State != offer.ResolutionUnattempted {
		t.Fatalf("summary c resolution = %+v", sc.Resolution)
	}
}

// More ids than one SQL statement may bind on SQLite (999 by default in older
// builds): the adapter must chunk, not fail or drop.
func summariesMany(t *testing.T, s offer.Store) {
	const n = 1100
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		o := Offer("shopify:many", fmt.Sprintf("k%d", i), int64(1000+i), T1)
		put(t, s, o)
		ids = append(ids, idOf(o))
	}
	got, err := s.Summaries(context.Background(), ids)
	if err != nil {
		t.Fatalf("Summaries(%d ids): %v", n, err)
	}
	if len(got) != n {
		t.Fatalf("Summaries returned %d of %d", len(got), n)
	}
}

func summariesEmpty(t *testing.T, s offer.Store) {
	got, err := s.Summaries(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Summaries(nil) = %v, %v; want empty, nil", got, err)
	}
}
