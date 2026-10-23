package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/store"
)

// Expired hiding (nagus-417). Items and offers share an id; the fixtures write
// both stores directly so the seen_at order and each offer's status are exact.

var expT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type expRow struct {
	id      string
	seen    time.Time
	status  offer.Status // "" = no offer row
	priceCt int64
}

func expFixture(t *testing.T, rows []expRow) (*store.MemoryStore, *offer.MemoryStore) {
	t.Helper()
	ctx := context.Background()
	st := store.NewMemoryStore()
	offers := offer.NewMemoryStore()
	for _, r := range rows {
		price := r.priceCt
		if price == 0 {
			price = 10000
		}
		it := item.Item{
			ID: r.id, Category: "hdd", Class: item.ClassDurable, Title: "drive " + r.id,
			PriceCents: price, SourceID: "shopify:s", SourceKey: r.id, SeenAt: r.seen,
			Attributes: map[string]string{"capacity_tb": "16"},
		}
		if err := st.Put(ctx, it); err != nil {
			t.Fatal(err)
		}
		if r.status == "" {
			continue
		}
		o := offer.Offer{
			ID: r.id, SourceID: "shopify:s", SourceKey: r.id, PriceCents: price,
			FirstSeen: expT0.Add(-24 * time.Hour), LastSeen: r.seen, Status: r.status,
		}
		if r.status == offer.StatusExpired {
			o.ExpiredAt = r.seen.Add(time.Hour)
		}
		if err := offers.Put(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	return st, offers
}

func surfIDs(res SurfaceResult) map[string]*offer.Summary {
	out := map[string]*offer.Summary{}
	for _, sc := range res.Items {
		out[sc.Item.ID] = sc.Offer
	}
	return out
}

func TestSurfaceHidesExpiredKeepsNoOfferRows(t *testing.T) {
	st, offers := expFixture(t, []expRow{
		{"live-a", expT0, offer.StatusActive, 0},
		{"live-b", expT0.Add(-time.Minute), offer.StatusActive, 0},
		{"gone-c", expT0.Add(-5 * time.Hour), offer.StatusExpired, 0},
		{"gone-d", expT0.Add(-6 * time.Hour), offer.StatusExpired, 0},
		{"orphan", expT0.Add(-time.Hour), "", 0},
	})
	s := &Surface{Store: st, Offers: offers}
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	got := surfIDs(res)
	if len(got) != 3 || res.HiddenExpired != 2 || res.Matched != 3 {
		t.Fatalf("items=%v hidden=%d matched=%d, want live-a, live-b, orphan; 2 hidden", got, res.HiddenExpired, res.Matched)
	}
	for _, id := range []string{"gone-c", "gone-d"} {
		if _, ok := got[id]; ok {
			t.Fatalf("expired listing %s surfaced", id)
		}
	}
	if got["orphan"] != nil {
		t.Fatalf("an item with no offer row carries an offer summary: %+v", got["orphan"])
	}
	if sm := got["live-a"]; sm == nil || sm.Status != offer.StatusActive || !sm.LastSeen.Equal(expT0) {
		t.Fatalf("live-a summary = %+v", sm)
	}
}

// Without an offer layer nothing is hidden and no row carries a summary: the
// pre-offer behaviour, byte for byte.
func TestSurfaceWithoutOffersHidesNothing(t *testing.T) {
	st, _ := expFixture(t, []expRow{
		{"live-a", expT0, offer.StatusActive, 0},
		{"gone-c", expT0.Add(-5 * time.Hour), offer.StatusExpired, 0},
	})
	res, err := (&Surface{Store: st}).Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.HiddenExpired != 0 {
		t.Fatalf("items=%d hidden=%d, want 2 and 0", len(res.Items), res.HiddenExpired)
	}
	for _, sc := range res.Items {
		if sc.Offer != nil {
			t.Fatalf("%s carries a summary with the offer layer off", sc.Item.ID)
		}
	}
}

// THE CAP: expired rows that sort FIRST (newest seen_at) must not push live
// listings out of a capped read. With a cap of 3 and the 3 newest rows all
// expired, a filter-after-cap would return nothing.
func TestSurfaceExpiredDoNotConsumeTheCandidateCap(t *testing.T) {
	rows := []expRow{
		{"gone-1", expT0, offer.StatusExpired, 0},
		{"gone-2", expT0.Add(-time.Minute), offer.StatusExpired, 0},
		{"gone-3", expT0.Add(-2 * time.Minute), offer.StatusExpired, 0},
	}
	for i := 0; i < 3; i++ {
		rows = append(rows, expRow{fmt.Sprintf("live-%d", i), expT0.Add(-time.Duration(10+i) * time.Hour), offer.StatusActive, 0})
	}
	st, offers := expFixture(t, rows)
	var logs []string
	s := &Surface{Store: st, Offers: offers, MaxCandidates: 3, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	got := surfIDs(res)
	for i := 0; i < 3; i++ {
		if _, ok := got[fmt.Sprintf("live-%d", i)]; !ok {
			t.Fatalf("live-%d missing: the expired rows used up the cap (got %v, logs %v)", i, got, logs)
		}
	}
	if len(got) != 3 || res.Matched != 3 || res.HiddenExpired != 3 {
		t.Fatalf("items=%v matched=%d hidden=%d", got, res.Matched, res.HiddenExpired)
	}
}

// The same at production scale with the real cap: 1,000 expired listings
// newer than 5,200 live ones. A filter after the 5,000-row read would keep
// only 4,000 live candidates.
func TestSurfaceDefaultCapWithExpiredFirst(t *testing.T) {
	var rows []expRow
	for i := 0; i < 1000; i++ {
		rows = append(rows, expRow{fmt.Sprintf("gone-%04d", i), expT0.Add(-time.Duration(i) * time.Second), offer.StatusExpired, 0})
	}
	for i := 0; i < 5200; i++ {
		rows = append(rows, expRow{fmt.Sprintf("live-%04d", i), expT0.Add(-time.Hour - time.Duration(i)*time.Second), offer.StatusActive, 0})
	}
	st, offers := expFixture(t, rows)
	res, err := (&Surface{Store: st, Offers: offers}).Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != DefaultMaxCandidates || len(res.Items) != DefaultMaxCandidates || !res.CapReached {
		t.Fatalf("matched=%d items=%d capReached=%v, want %d live candidates", res.Matched, len(res.Items), res.CapReached, DefaultMaxCandidates)
	}
	got := surfIDs(res)
	for i := 0; i < DefaultMaxCandidates; i++ {
		if _, ok := got[fmt.Sprintf("live-%04d", i)]; !ok {
			t.Fatalf("live-%04d (among the %d most recently polled live rows) missing", i, DefaultMaxCandidates)
		}
	}
	if res.HiddenExpired != 1000 {
		t.Fatalf("hidden = %d, want 1000", res.HiddenExpired)
	}
}

// The top-up is bounded: a pathological corpus logs and returns what it has.
func TestSurfaceTopUpIsBounded(t *testing.T) {
	var rows []expRow
	// 40 expired rows newer than the one live row; cap 2. The reads ask for
	// 2, then 4, then 6 rows -- all expired -- and stop at maxTopUps.
	for i := 0; i < 40; i++ {
		rows = append(rows, expRow{fmt.Sprintf("gone-%02d", i), expT0.Add(-time.Duration(i) * time.Minute), offer.StatusExpired, 0})
	}
	rows = append(rows, expRow{"live-x", expT0.Add(-48 * time.Hour), offer.StatusActive, 0})
	st, offers := expFixture(t, rows)
	var searches int
	var logs []string
	s := &Surface{
		Store: countingSearch{st, &searches}, Offers: offers, MaxCandidates: 2,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	}
	res, err := s.Surface(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	if searches != maxTopUps {
		t.Fatalf("store searched %d times, want %d", searches, maxTopUps)
	}
	if len(res.Items) != 0 || !res.CapReached {
		t.Fatalf("items=%d capReached=%v", len(res.Items), res.CapReached)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "expired rows still fill part of the candidate cap") {
			found = true
		}
	}
	if !found {
		t.Fatalf("shortfall not logged: %v", logs)
	}
}

func TestSurfaceLimitAppliesAfterHiding(t *testing.T) {
	st, offers := expFixture(t, []expRow{
		{"gone-1", expT0, offer.StatusExpired, 0},
		{"live-a", expT0.Add(-time.Hour), offer.StatusActive, 0},
		{"live-b", expT0.Add(-2 * time.Hour), offer.StatusActive, 0},
	})
	res, err := (&Surface{Store: st, Offers: offers}).Surface(context.Background(), store.Query{Category: "hdd", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Item.ID == "gone-1" {
		t.Fatalf("limit 1 returned %v", surfIDs(res))
	}
}

// An auction whose stated end has passed is gone even before a poll expires
// its offer (~3 intervals later); one that has not ended stays.
func TestSurfaceHidesListingsPastTheirStatedEnd(t *testing.T) {
	st, offers := expFixture(t, []expRow{
		{"ended", expT0, offer.StatusActive, 0},
		{"running", expT0, offer.StatusActive, 0},
	})
	ctx := context.Background()
	for id, end := range map[string]time.Time{"ended": expT0.Add(-time.Minute), "running": expT0.Add(time.Hour)} {
		if err := offers.Put(ctx, offer.Offer{
			ID: id, SourceID: "shopify:s", SourceKey: id, PriceCents: 10000,
			LastSeen: expT0, Status: offer.StatusActive, EndsAt: end,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Surface{Store: st, Offers: offers, Now: func() time.Time { return expT0 }}
	res, err := s.Surface(ctx, store.Query{Category: "hdd"})
	if err != nil {
		t.Fatal(err)
	}
	got := surfIDs(res)
	if _, ok := got["ended"]; ok || len(got) != 1 || res.HiddenExpired != 1 {
		t.Fatalf("items=%v hidden=%d, want only the running auction", got, res.HiddenExpired)
	}
}

type failingSummaries struct{}

func (failingSummaries) Summaries(context.Context, []string) (map[string]offer.Summary, error) {
	return nil, errors.New("offer store down")
}

// Fail closed: without the offer status nagus cannot tell live from gone.
func TestSurfaceOfferReadFailureFailsTheRead(t *testing.T) {
	st, _ := expFixture(t, []expRow{{"live-a", expT0, offer.StatusActive, 0}})
	if _, err := (&Surface{Store: st, Offers: failingSummaries{}}).Surface(context.Background(), store.Query{Category: "hdd"}); err == nil {
		t.Fatal("an offer-store failure surfaced unchecked listings")
	}
}

// countingSearch counts store reads.
type countingSearch struct {
	store.Store
	n *int
}

func (c countingSearch) Search(ctx context.Context, q store.Query) ([]item.Item, error) {
	*c.n++
	return c.Store.Search(ctx, q)
}
