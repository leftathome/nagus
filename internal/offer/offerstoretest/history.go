package offerstoretest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// The price-history contract (nagus-0ta): offer_price_history holds one row per
// price CHANGE under the nagus-417 rule, plus the first known price, written by
// Put; ApplyRetention deletes it with the offer (Purge) or past the history
// window (RetainFull). See offer.HistoryRows and offer.Store.

// runHistory is called from Run.
func runHistory(t *testing.T, newStore NewStore) {
	t.Run("HistoryFirstSightingIsOneRow", func(t *testing.T) { historyFirstSighting(t, newStore(t)) })
	t.Run("HistoryNoRowPerPoll", func(t *testing.T) { historyNoRowPerPoll(t, newStore(t)) })
	t.Run("HistoryRowPerChange", func(t *testing.T) { historyRowPerChange(t, newStore(t)) })
	t.Run("HistoryUnknownPriceIsNoChange", func(t *testing.T) { historyUnknownPrice(t, newStore(t)) })
	t.Run("HistoryFirstKnownAfterUnknown", func(t *testing.T) { historyFirstKnownAfterUnknown(t, newStore(t)) })
	t.Run("HistoryAnchorsAnOfferWithoutHistory", func(t *testing.T) { historyAnchor(t, newStore(t)) })
	t.Run("HistoryOutOfOrderSameStampReplaces", func(t *testing.T) { historyOutOfOrder(t, newStore(t)) })
	t.Run("HistoryAgreesWithPriceChangedAt", func(t *testing.T) { historyAgreesWithOffer(t, newStore(t)) })
	t.Run("HistorySinceAndPerOfferCap", func(t *testing.T) { historySinceAndCap(t, newStore(t)) })
	t.Run("HistoryManyIDsAndEmpty", func(t *testing.T) { historyManyIDs(t, newStore(t)) })
	t.Run("HistoryPurgedWithOffer", func(t *testing.T) { historyPurgedWithOffer(t, newStore(t)) })
	t.Run("HistoryPurgeWindowBoundsLiveEbayOffer", func(t *testing.T) { historyPurgeBoundsLiveOffer(t, newStore(t)) })
	t.Run("HistoryWindowForRetainedSource", func(t *testing.T) { historyWindowRetainFull(t, newStore(t)) })
	t.Run("HistoryWindowNegativeRejected", func(t *testing.T) { historyWindowNegative(t, newStore(t)) })
	t.Run("HistoryConcurrentPutsOfOneOffer", func(t *testing.T) { historyConcurrent(t, newStore(t)) })
	t.Run("HistoryConcurrentPutAndRetention", func(t *testing.T) { historyConcurrentPutAndRetention(t, newStore(t)) })
}

// hist reads one offer's whole history.
func hist(t *testing.T, s offer.Store, id string) []offer.PricePoint {
	t.Helper()
	got, err := s.PriceHistory(context.Background(), []string{id}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	return got[id]
}

// pt is a compact expected point.
type pt struct {
	at    time.Time
	price int64
}

func wantHist(t *testing.T, got []offer.PricePoint, want ...pt) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("history = %s, want %d points %v", fmtHist(got), len(want), want)
	}
	for i := range want {
		if !got[i].ObservedAt.Equal(want[i].at) || got[i].PriceCents != want[i].price {
			t.Fatalf("history = %s, want %v", fmtHist(got), want)
		}
	}
}

func fmtHist(h []offer.PricePoint) string {
	s := "["
	for i, p := range h {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%s:%d%s", p.ObservedAt.Format(time.RFC3339), p.PriceCents, p.Currency)
	}
	return s + "]"
}

func historyFirstSighting(t *testing.T, s offer.Store) {
	o := Offer("shopify:a", "k", 1000, T0)
	put(t, s, o)
	got := hist(t, s, idOf(o))
	wantHist(t, got, pt{T0, 1000})
	if got[0].Currency != "USD" {
		t.Fatalf("currency = %q, want USD", got[0].Currency)
	}
}

func historyNoRowPerPoll(t *testing.T, s offer.Store) {
	for i := 0; i < 5; i++ {
		put(t, s, Offer("shopify:a", "k", 1000, T0.Add(time.Duration(i)*time.Hour)))
	}
	wantHist(t, hist(t, s, offer.DeterministicID("shopify:a", "k")), pt{T0, 1000})
}

func historyRowPerChange(t *testing.T, s offer.Store) {
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 800, T1))
	put(t, s, Offer("shopify:a", "k", 800, T1.Add(time.Hour)))
	put(t, s, Offer("shopify:a", "k", 900, T2))
	wantHist(t, hist(t, s, offer.DeterministicID("shopify:a", "k")), pt{T0, 1000}, pt{T1, 800}, pt{T2, 900})
}

// An unknown-price poll is not a change, and 1000 -> 0 -> 1000 adds nothing.
func historyUnknownPrice(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "k")
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 0, T1))
	put(t, s, Offer("shopify:a", "k", 1000, T2))
	wantHist(t, hist(t, s, id), pt{T0, 1000})
	// 1000 -> 0 -> 900 IS a change, from the last known price.
	put(t, s, Offer("shopify:a", "k", 0, T2.Add(time.Hour)))
	put(t, s, Offer("shopify:a", "k", 900, T2.Add(2*time.Hour)))
	wantHist(t, hist(t, s, id), pt{T0, 1000}, pt{T2.Add(2 * time.Hour), 900})
}

// A listing first seen without a price gets no row until a price is known;
// the first known price starts its series (it is not a change).
func historyFirstKnownAfterUnknown(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "u")
	put(t, s, Offer("shopify:a", "u", 0, T0))
	if got := hist(t, s, id); len(got) != 0 {
		t.Fatalf("an unpriced first sighting wrote history: %s", fmtHist(got))
	}
	put(t, s, Offer("shopify:a", "u", 900, T1))
	put(t, s, Offer("shopify:a", "u", 800, T2))
	wantHist(t, hist(t, s, id), pt{T1, 900}, pt{T2, 800})
}

// An offer with no history (older than the table, or its rows purged by the
// window) gets an ANCHOR for the price a change leaves, at that price's stored
// price_changed_at, so the price before the change is not lost.
func historyAnchor(t *testing.T, s offer.Store) {
	ctx := context.Background()
	id := offer.DeterministicID("ebay:ebay", "k")
	put(t, s, Offer("ebay:ebay", "k", 1000, T0))
	// Still listed 6.5h later at the same price (no row), then the 6h purge
	// drops the T0 row of the LIVE offer: it now has no history.
	put(t, s, Offer("ebay:ebay", "k", 1000, T0.Add(390*time.Minute)))
	if _, err := s.ApplyRetention(ctx, "ebay:ebay", offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}, T0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := hist(t, s, id); len(got) != 0 {
		t.Fatalf("the purge window kept %s", fmtHist(got))
	}
	at := T0.Add(8 * time.Hour)
	put(t, s, Offer("ebay:ebay", "k", 800, at))
	wantHist(t, hist(t, s, id), pt{T0, 1000}, pt{at, 800})
	// Only once: the next change adds just its own row.
	put(t, s, Offer("ebay:ebay", "k", 700, at.Add(time.Hour)))
	wantHist(t, hist(t, s, id), pt{T0, 1000}, pt{at, 800}, pt{at.Add(time.Hour), 700})
	// The anchor is older than the content window, so the next pass drops it
	// (and the read path never serves it meanwhile: cmd/nagus filters eBay
	// points by the same window).
	if _, err := s.ApplyRetention(ctx, "ebay:ebay", offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantHist(t, hist(t, s, id), pt{at, 800}, pt{at.Add(time.Hour), 700})
}

// A late write with another price lands on the stamp price_changed_at keeps
// (it never rewinds), and the point there takes the price the offer now holds.
func historyOutOfOrder(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "k")
	put(t, s, Offer("shopify:a", "k", 1000, T0))
	put(t, s, Offer("shopify:a", "k", 800, T2))
	put(t, s, Offer("shopify:a", "k", 900, T1))
	got := hist(t, s, id)
	o := getByID(t, s, id)
	if last := got[len(got)-1]; last.PriceCents != o.PriceCents || !last.ObservedAt.Equal(o.PriceChangedAt) {
		t.Fatalf("newest point %s disagrees with the offer (price %d changed at %v)", fmtHist(got), o.PriceCents, o.PriceChangedAt)
	}
	wantHist(t, got, pt{T0, 1000}, pt{T2, 900})
}

// The newest point is always the offer's current price at its
// price_changed_at: the two are written by one rule in one transaction.
func historyAgreesWithOffer(t *testing.T, s offer.Store) {
	id := offer.DeterministicID("shopify:a", "k")
	prices := []int64{1000, 1000, 0, 950, 950, 1100, 0, 0, 1100, 990}
	for i, p := range prices {
		put(t, s, Offer("shopify:a", "k", p, T0.Add(time.Duration(i)*time.Hour)))
		o := getByID(t, s, id)
		h := hist(t, s, id)
		last := h[len(h)-1]
		if last.PriceCents != o.PriceCents || !last.ObservedAt.Equal(o.PriceChangedAt) {
			t.Fatalf("after poll %d: newest point %s, offer price %d changed at %v", i, fmtHist(h), o.PriceCents, o.PriceChangedAt)
		}
	}
	if h := hist(t, s, id); len(h) != 4 { // 1000, 950, 1100, 990
		t.Fatalf("history = %s, want 4 points", fmtHist(h))
	}
}

func historySinceAndCap(t *testing.T, s offer.Store) {
	ctx := context.Background()
	id := offer.DeterministicID("shopify:a", "k")
	for i := 0; i < 6; i++ {
		put(t, s, Offer("shopify:a", "k", int64(1000+i), T0.Add(time.Duration(i)*time.Hour)))
	}
	since := T0.Add(2 * time.Hour)
	got, err := s.PriceHistory(ctx, []string{id}, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantHist(t, got[id], pt{since, 1002}, pt{T0.Add(3 * time.Hour), 1003}, pt{T0.Add(4 * time.Hour), 1004}, pt{T0.Add(5 * time.Hour), 1005})
	// The cap keeps the NEWEST rows, still oldest first.
	got, err = s.PriceHistory(ctx, []string{id}, since, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantHist(t, got[id], pt{T0.Add(4 * time.Hour), 1004}, pt{T0.Add(5 * time.Hour), 1005})
}

func historyManyIDs(t *testing.T, s offer.Store) {
	const n = 1100
	ids := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		o := Offer("shopify:many", fmt.Sprintf("k%d", i), int64(1000+i), T1)
		put(t, s, o)
		ids = append(ids, idOf(o))
	}
	ids = append(ids, "no-such-offer")
	got, err := s.PriceHistory(context.Background(), ids, time.Time{}, 1)
	if err != nil {
		t.Fatalf("PriceHistory(%d ids): %v", len(ids), err)
	}
	if len(got) != n {
		t.Fatalf("PriceHistory returned %d offers, want %d (unknown id absent)", len(got), n)
	}
	if empty, err := s.PriceHistory(context.Background(), nil, time.Time{}, 0); err != nil || len(empty) != 0 {
		t.Fatalf("PriceHistory(nil) = %v, %v", empty, err)
	}
}

// eBay (License 8.1(b)): a purged offer takes its history with it.
func historyPurgedWithOffer(t *testing.T, s offer.Store) {
	ctx := context.Background()
	old := Offer("ebay:ebay", "old", 1000, T0)
	put(t, s, old)
	put(t, s, Offer("ebay:ebay", "old", 900, T0.Add(time.Hour)))
	keep := Offer("shopify:x", "keep", 1000, T0)
	put(t, s, keep)
	n, err := s.ApplyRetention(ctx, "ebay:ebay", offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}, T2)
	if err != nil || n != 1 {
		t.Fatalf("ApplyRetention = %d, %v; want 1", n, err)
	}
	if got := hist(t, s, idOf(old)); len(got) != 0 {
		t.Fatalf("purged eBay offer kept history %s", fmtHist(got))
	}
	wantHist(t, hist(t, s, idOf(keep)), pt{T0, 1000})
}

// Under Purge no history row outlives the content window, even for an offer
// that is still live: the history is eBay Content too.
func historyPurgeBoundsLiveOffer(t *testing.T, s offer.Store) {
	ctx := context.Background()
	id := offer.DeterministicID("ebay:ebay", "live")
	put(t, s, Offer("ebay:ebay", "live", 1000, T0))
	put(t, s, Offer("ebay:ebay", "live", 900, T2))
	n, err := s.ApplyRetention(ctx, "ebay:ebay", offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}, T2.Add(time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("ApplyRetention = %d, %v; want 0 (the offer is live)", n, err)
	}
	wantHist(t, hist(t, s, id), pt{T2, 900})
}

func historyWindowRetainFull(t *testing.T, s offer.Store) {
	ctx := context.Background()
	a := offer.DeterministicID("shopify:x", "a")
	b := offer.DeterministicID("shopify:y", "b")
	put(t, s, Offer("shopify:x", "a", 1000, T0))
	put(t, s, Offer("shopify:x", "a", 900, T2))
	put(t, s, Offer("shopify:y", "b", 1000, T0))
	// No window: everything stays.
	if _, err := s.ApplyRetention(ctx, "shopify:x", offer.Retention{Policy: offer.RetainFull}, T2.Add(1000*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantHist(t, hist(t, s, a), pt{T0, 1000}, pt{T2, 900})
	// A window drops only that source's rows older than it, and no offer --
	// but keeps each offer's CARRY-IN point (the newest row at or before the
	// cutoff: the price in effect when the window opens). a: 1000 (T0), 900
	// (T2), 800 (T2+1h); cutoff T2+30m -> T0 goes, T2 is the carry-in.
	put(t, s, Offer("shopify:x", "a", 800, T2.Add(time.Hour)))
	wantHist(t, hist(t, s, a), pt{T0, 1000}, pt{T2, 900}, pt{T2.Add(time.Hour), 800})
	n, err := s.ApplyRetention(ctx, "shopify:x", offer.Retention{Policy: offer.RetainFull, HistoryWindow: 30 * time.Minute}, T2.Add(time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("ApplyRetention = %d, %v; want 0 offers removed", n, err)
	}
	wantHist(t, hist(t, s, a), pt{T2, 900}, pt{T2.Add(time.Hour), 800})
	wantHist(t, hist(t, s, b), pt{T0, 1000})
	// A price that stood still longer than the window keeps its start.
	if _, err := s.ApplyRetention(ctx, "shopify:y", offer.Retention{Policy: offer.RetainFull, HistoryWindow: time.Hour}, T2.Add(1000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantHist(t, hist(t, s, b), pt{T0, 1000})
	// A row exactly at the cutoff is the carry-in; everything older goes.
	cut := T2.Add(time.Hour)
	if _, err := s.ApplyRetention(ctx, "shopify:x", offer.Retention{Policy: offer.RetainFull, HistoryWindow: time.Hour}, cut.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantHist(t, hist(t, s, a), pt{cut, 800})
	if got := count(t, s); got != 2 {
		t.Fatalf("offers = %d, want 2 (the history window never deletes offers)", got)
	}
}

// The store contract says concurrent use is safe; ingest runs one goroutine
// per source, but a retention pass and a Put of the same source's offers must
// still never fail or orphan history (Postgres deadlocked here: history-then-
// offers in ApplyRetention against offers-then-history in Put). Invariants
// only, no timing.
func historyConcurrentPutAndRetention(t *testing.T, s offer.Store) {
	ctx := context.Background()
	const offers = 30
	old, now := T0, T2
	ret := offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour}
	ids := make([]string, 0, offers)
	for i := 0; i < offers; i++ {
		key := fmt.Sprintf("k%d", i)
		put(t, s, Offer("ebay:race", key, 1000, old))
		put(t, s, Offer("ebay:race", key, 900, old.Add(time.Minute)))
		ids = append(ids, offer.DeterministicID("ebay:race", key))
	}
	var wg sync.WaitGroup
	errs := make(chan error, offers+20)
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < offers; i += 3 {
				// Re-seen now at a new price: the offer either survives the
				// purge (and loses its old rows) or is purged first and comes
				// back as a first sighting. Never an error, never an orphan.
				if err := s.Put(ctx, Offer("ebay:race", fmt.Sprintf("k%d", i), int64(800+i), now)); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := s.ApplyRetention(ctx, "ebay:race", ret, now.Add(time.Minute)); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Put + ApplyRetention: %v", err)
	}
	if _, err := s.ApplyRetention(ctx, "ebay:race", ret, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.PriceHistory(ctx, ids, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		o, ok, err := s.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		h := got[id]
		if !ok {
			t.Fatalf("offer %d is gone, but it was re-seen at %v", i, now)
		}
		if len(h) != 1 || h[0].PriceCents != o.PriceCents || !h[0].ObservedAt.Equal(now) || o.PriceCents != int64(800+i) {
			t.Fatalf("offer %d: price %d, history %s; want one point at the new price", i, o.PriceCents, fmtHist(h))
		}
	}
}

func historyWindowNegative(t *testing.T, s offer.Store) {
	_, err := s.ApplyRetention(context.Background(), "shopify:x", offer.Retention{Policy: offer.RetainFull, HistoryWindow: -time.Hour}, T2)
	if err == nil {
		t.Fatal("a negative history window must be rejected")
	}
}

// Ingest of one source is one goroutine, but nothing stops two passes (or a
// deal and a store source) writing one offer at once. Every Put must succeed
// and history must stay consistent with the offer, including the race of two
// FIRST inserts of one offer.
func historyConcurrent(t *testing.T, s offer.Store) {
	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := Offer("shopify:race", "k", int64(1000+10*i), T0.Add(time.Duration(i)*time.Minute))
			if err := s.Put(context.Background(), o); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Put: %v", err)
	}
	id := offer.DeterministicID("shopify:race", "k")
	o := getByID(t, s, id)
	h := hist(t, s, id)
	if len(h) == 0 || len(h) > writers {
		t.Fatalf("history = %s, want 1..%d points", fmtHist(h), writers)
	}
	if last := h[len(h)-1]; last.PriceCents != o.PriceCents || !last.ObservedAt.Equal(o.PriceChangedAt) {
		t.Fatalf("newest point %s disagrees with the offer (price %d changed at %v)", fmtHist(h), o.PriceCents, o.PriceChangedAt)
	}
}
