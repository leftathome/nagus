package hdd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/store"
)

type fakeSearcher struct {
	items   []item.Item
	queries []store.Query
	err     error
	calls   int
}

func (f *fakeSearcher) Search(_ context.Context, q store.Query) ([]item.Item, error) {
	f.calls++
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return f.items, nil
}

func hddItem(id string, capTB string, cond string, priceCents int64) item.Item {
	return item.Item{
		ID: id, Category: "hdd", Class: item.ClassDurable,
		PriceCents: priceCents, Currency: "USD", Condition: cond,
		SourceID: "shopify:test", SourceKey: id,
		Attributes: map[string]string{"capacity_tb": capTB},
		SeenAt:     time.Unix(1_750_000_000, 0).UTC(),
	}
}

func newStoreSource(f *fakeSearcher, min int) *StoreSource {
	return &StoreSource{
		Store: f, Category: "hdd", MinSamples: min,
		Now: func() time.Time { return time.Unix(1_750_000_000, 0).UTC() },
	}
}

func TestStoreSourceMedianOfComparables(t *testing.T) {
	f := &fakeSearcher{items: []item.Item{
		// 10TB refurb: $100, $150, $200 -> 10.00, 15.00, 20.00 $/TB -> median 15.00
		hddItem("a", "10", "refurb", 10000),
		hddItem("b", "10", "refurb", 15000),
		hddItem("c", "10", "refurb", 20000),
		// noise that must not be matched
		hddItem("d", "16", "refurb", 16000),
		hddItem("e", "10", "new", 30000),
	}}
	got, ok, err := newStoreSource(f, 3).PricePerTB(context.Background(), 10, "refurb")
	if err != nil || !ok {
		t.Fatalf("PricePerTB = (%d,%v,%v), want a reference", got, ok, err)
	}
	if want := int64(1500); got != want {
		t.Fatalf("reference = %d cents/TB, want %d (median of 1000/1500/2000)", got, want)
	}
}

// THE SELF-COMPARISON TRAP: a reference computed over the same corpus being
// scored would, for a listing with no comparables, return that listing's own
// price -- ratio exactly 1.0, "market" forever, and confident about it.
// Below MinSamples the honest answer is "no reference".
func TestStoreSourceRefusesBelowMinSamples(t *testing.T) {
	f := &fakeSearcher{items: []item.Item{
		hddItem("only", "14", "refurb", 28000),
		hddItem("other", "14", "refurb", 30000),
	}}
	_, ok, err := newStoreSource(f, 3).PricePerTB(context.Background(), 14, "refurb")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Fatal("2 comparables with MinSamples=3 must yield ok=false, not a self-referential median")
	}
	// One more comparable and it becomes usable.
	f.items = append(f.items, hddItem("third", "14", "refurb", 26000))
	s := newStoreSource(f, 3)
	if _, ok, _ := s.PricePerTB(context.Background(), 14, "refurb"); !ok {
		t.Fatal("3 comparables with MinSamples=3 must yield a reference")
	}
}

// The 6-14TB gap that motivated this change: the old live source had no coverage
// there. A store corpus that contains those capacities must produce a reference.
func TestStoreSourceCoversTheBandTheLiveFeedMissed(t *testing.T) {
	var items []item.Item
	for _, cap := range []string{"6", "10", "14"} {
		for i, price := range []int64{12000, 15000, 18000} {
			items = append(items, hddItem(cap+"-"+string(rune('a'+i)), cap, "refurb", price))
		}
	}
	s := newStoreSource(&fakeSearcher{items: items}, 3)
	for _, cap := range []float64{6, 10, 14} {
		got, ok, err := s.PricePerTB(context.Background(), cap, "refurb")
		if err != nil || !ok {
			t.Errorf("%.0fTB: got (%d,%v,%v), want a reference", cap, got, ok, err)
		}
	}
}

func TestStoreSourceConditionIsExact(t *testing.T) {
	f := &fakeSearcher{items: []item.Item{
		hddItem("a", "8", "refurb", 8000),
		hddItem("b", "8", "refurb", 9000),
		hddItem("c", "8", "refurb", 10000),
	}}
	s := newStoreSource(f, 3)
	if _, ok, _ := s.PricePerTB(context.Background(), 8, "new"); ok {
		t.Error("a refurb-only corpus must not answer a 'new' lookup directly (the Valuer applies its own condition fallback)")
	}
	if _, ok, _ := s.PricePerTB(context.Background(), 8, "REFURB"); !ok {
		t.Error("condition match must be case-insensitive")
	}
}

// Items that cannot anchor a $/TB figure are skipped rather than poisoning it.
func TestStoreSourceSkipsUnusableItems(t *testing.T) {
	f := &fakeSearcher{items: []item.Item{
		hddItem("ok1", "10", "refurb", 10000),
		hddItem("ok2", "10", "refurb", 12000),
		hddItem("ok3", "10", "refurb", 14000),
		hddItem("noprice", "10", "refurb", 0), // unknown price
		hddItem("badcap", "not-a-number", "refurb", 9000),
		func() item.Item {
			i := hddItem("nocap", "10", "refurb", 9000)
			delete(i.Attributes, "capacity_tb")
			return i
		}(),
		func() item.Item { i := hddItem("nocond", "10", "", 9000); return i }(),
	}}
	got, ok, err := newStoreSource(f, 3).PricePerTB(context.Background(), 10, "refurb")
	if err != nil || !ok {
		t.Fatalf("got (%d,%v,%v), want a reference from the 3 usable items", got, ok, err)
	}
	if want := int64(1200); got != want {
		t.Fatalf("reference = %d, want %d -- unusable rows must be skipped, not counted", got, want)
	}
}

// The read path is hot: the corpus must be cached, not re-queried per lookup.
func TestStoreSourceCachesCorpus(t *testing.T) {
	f := &fakeSearcher{items: []item.Item{
		hddItem("a", "10", "refurb", 10000),
		hddItem("b", "10", "refurb", 12000),
		hddItem("c", "10", "refurb", 14000),
	}}
	s := newStoreSource(f, 3)
	s.CacheTTL = time.Hour
	for i := 0; i < 5; i++ {
		if _, ok, _ := s.PricePerTB(context.Background(), 10, "refurb"); !ok {
			t.Fatalf("lookup %d failed", i)
		}
	}
	if f.calls != 1 {
		t.Fatalf("store searched %d times for 5 lookups, want 1 (cached)", f.calls)
	}
}

// Staleness is bounded: the corpus query must carry a Since floor so a
// long-dead price cannot anchor today's verdict.
func TestStoreSourceBoundsStaleness(t *testing.T) {
	f := &fakeSearcher{}
	s := newStoreSource(f, 3)
	s.MaxAge = 48 * time.Hour
	_, _, _ = s.PricePerTB(context.Background(), 10, "refurb")
	if len(f.queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(f.queries))
	}
	q := f.queries[0]
	if q.Category != "hdd" {
		t.Errorf("query category = %q, want hdd", q.Category)
	}
	want := time.Unix(1_750_000_000, 0).UTC().Add(-48 * time.Hour)
	if !q.Since.Equal(want) {
		t.Errorf("query Since = %v, want %v", q.Since, want)
	}
}

func TestStoreSourceInvalidCapacity(t *testing.T) {
	s := newStoreSource(&fakeSearcher{}, 3)
	if _, _, err := s.PricePerTB(context.Background(), 0, "refurb"); !errors.Is(err, ErrInvalidCapacity) {
		t.Fatalf("err = %v, want ErrInvalidCapacity", err)
	}
}

func TestStoreSourcePropagatesStoreError(t *testing.T) {
	boom := errors.New("store exploded")
	s := newStoreSource(&fakeSearcher{err: boom}, 3)
	if _, _, err := s.PricePerTB(context.Background(), 10, "refurb"); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store error propagated", err)
	}
}
