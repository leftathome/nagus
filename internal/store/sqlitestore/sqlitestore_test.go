package sqlitestore

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/store"
)

// newTestStore returns a Store backed by a fresh on-disk temp DB, closed
// automatically when the test ends. A real file (rather than ":memory:") is
// used in most tests to exercise the actual schema/migration path the way it
// will run in production; a couple of tests exercise ":memory:" directly.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "nagus.db")
	s, err := New(dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkItem(id, cat string, price int64, seen time.Time, title string) item.Item {
	return item.Item{
		ID: id, Category: cat, Class: item.ClassDurable, Title: title,
		PriceCents: price, Currency: "USD",
		SourceID: "test", SourceKey: id, SeenAt: seen,
	}
}

func ids(items []item.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestNewCreatesSchemaOnMemoryDSN(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatalf("New(:memory:): %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	it := mkItem("m1", "hdd", 100, time.Unix(1, 0), "in memory")
	if err := s.Put(ctx, it); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := s.Get(ctx, "m1")
	if err != nil || !ok {
		t.Fatalf("get: %+v ok=%v err=%v", got, ok, err)
	}
}

func TestVarStoreSatisfiesInterface(t *testing.T) {
	var _ store.Store = (*Store)(nil)
}

func TestPutRejectsInvalidItem(t *testing.T) {
	s := newTestStore(t)
	err := s.Put(context.Background(), item.Item{ID: ""}) // missing everything
	if err == nil {
		t.Fatal("expected Put to reject an invalid item")
	}
}

func TestPutGetRoundTripAndReplace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	it := mkItem("a", "hdd", 10000, time.Unix(100, 0), "16TB Exos")
	if err := s.Put(ctx, it); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := s.Get(ctx, "a")
	if err != nil || !ok || got.Title != "16TB Exos" {
		t.Fatalf("get round-trip failed: %+v ok=%v err=%v", got, ok, err)
	}
	// Replace-by-ID.
	it.PriceCents = 9000
	if err := s.Put(ctx, it); err != nil {
		t.Fatalf("put replace: %v", err)
	}
	got, _, err = s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("get after replace: %v", err)
	}
	if got.PriceCents != 9000 {
		t.Fatalf("expected replace to 9000, got %d", got.PriceCents)
	}
}

func TestDeleteStaleRemovesOldItemsOfOneSource(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ebayOld := mkItem("eo", "hdd", 10000, time.Unix(100, 0), "old ebay drive")
	ebayOld.SourceID = "ebay"
	ebayFresh := mkItem("ef", "hdd", 10000, time.Unix(1000, 0), "fresh ebay drive")
	ebayFresh.SourceID = "ebay"
	clOld := mkItem("co", "land", 50000, time.Unix(100, 0), "old craigslist parcel")
	clOld.SourceID = "craigslist"
	for _, it := range []item.Item{ebayOld, ebayFresh, clOld} {
		if err := s.Put(ctx, it); err != nil {
			t.Fatalf("put %s: %v", it.ID, err)
		}
	}

	n, err := s.DeleteStale(ctx, "ebay", time.Unix(500, 0))
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteStale deleted %d, want 1", n)
	}
	if _, ok, _ := s.Get(ctx, "eo"); ok {
		t.Fatalf("stale ebay item eo should be deleted")
	}
	if _, ok, _ := s.Get(ctx, "ef"); !ok {
		t.Fatalf("fresh ebay item ef must survive")
	}
	if _, ok, _ := s.Get(ctx, "co"); !ok {
		t.Fatalf("craigslist item co must be untouched by an ebay purge")
	}
	// The FTS mirror must not resurrect the deleted row via text search.
	hits, _ := s.Search(ctx, store.Query{Text: "old ebay drive"})
	for _, h := range hits {
		if h.ID == "eo" {
			t.Fatalf("deleted item eo still reachable via FTS search")
		}
	}
}

func TestGetMissingReturnsFalse(t *testing.T) {
	s := newTestStore(t)
	got, ok, err := s.Get(context.Background(), "nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for missing id, got item %+v", got)
	}
}

func TestSearchFiltersAndOrdersNewestFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustPut(t, s, mkItem("old", "land", 100000, time.Unix(100, 0), "back forty"))
	mustPut(t, s, mkItem("new", "land", 200000, time.Unix(300, 0), "creek parcel"))
	mustPut(t, s, mkItem("hdd", "hdd", 5000, time.Unix(200, 0), "drive"))

	got, err := s.Search(ctx, store.Query{Category: "land"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 land items, got %d (%v)", len(got), ids(got))
	}
	if got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("expected newest-first [new, old], got %v", ids(got))
	}
}

func TestSearchOrderingStableTiebreakOnEqualSeenAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	same := time.Unix(500, 0)
	mustPut(t, s, mkItem("b", "land", 1, same, "one"))
	mustPut(t, s, mkItem("a", "land", 1, same, "two"))
	mustPut(t, s, mkItem("c", "land", 1, same, "three"))

	got, err := s.Search(ctx, store.Query{Category: "land"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 3 || got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("expected ID-ascending tiebreak [a, b, c], got %v", ids(got))
	}
}

func TestSearchEmptyQueryReturnsEverything(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustPut(t, s, mkItem("a", "land", 1, time.Unix(100, 0), "one"))
	mustPut(t, s, mkItem("b", "hdd", 2, time.Unix(200, 0), "two"))

	got, err := s.Search(ctx, store.Query{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected empty query to return everything, got %v", ids(got))
	}
}

func TestSearchPriceBoundExcludesUnknownPrice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustPut(t, s, mkItem("cheap", "hdd", 5000, time.Unix(100, 0), "a"))
	mustPut(t, s, mkItem("dear", "hdd", 50000, time.Unix(100, 0), "b"))
	mustPut(t, s, mkItem("unknown", "hdd", 0, time.Unix(100, 0), "c")) // price unknown

	got, err := s.Search(ctx, store.Query{Category: "hdd", MaxPriceCents: 10000})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "cheap" {
		t.Fatalf("price bound should return only 'cheap', got %+v", ids(got))
	}
}

func TestSearchClassFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	durable := mkItem("d", "hdd", 1, time.Unix(100, 0), "durable one")
	durable.Class = item.ClassDurable
	consumable := mkItem("c", "hdd", 1, time.Unix(100, 0), "consumable one")
	consumable.Class = item.ClassConsumable
	mustPut(t, s, durable)
	mustPut(t, s, consumable)

	got, err := s.Search(ctx, store.Query{Class: item.ClassConsumable})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("class filter expected [c], got %v", ids(got))
	}
}

func TestSearchTextAndSinceAndLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustPut(t, s, mkItem("a", "land", 1, time.Unix(100, 0), "wooded ACRES near town"))
	mustPut(t, s, mkItem("b", "land", 1, time.Unix(400, 0), "downtown condo"))
	c := mkItem("c", "land", 1, time.Unix(500, 0), "open acres")
	c.Tokens = []string{"acres", "pasture"}
	mustPut(t, s, c)

	// case-insensitive text over title + tokens
	got, err := s.Search(ctx, store.Query{Text: "acres"})
	if err != nil {
		t.Fatalf("search text: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("text match expected 2, got %v", ids(got))
	}
	// Since bound
	got, err = s.Search(ctx, store.Query{Since: time.Unix(450, 0)})
	if err != nil {
		t.Fatalf("search since: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("since bound expected [c], got %v", ids(got))
	}
	// Limit
	got, err = s.Search(ctx, store.Query{Limit: 1})
	if err != nil {
		t.Fatalf("search limit: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c" { // newest first
		t.Fatalf("limit expected [c], got %v", ids(got))
	}
}

func TestSearchTextMatchesTokenOnly(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	it := mkItem("t", "hdd", 1, time.Unix(100, 0), "16TB Exos X18")
	it.Tokens = []string{"exos", "helium", "enterprise"}
	mustPut(t, s, it)

	got, err := s.Search(ctx, store.Query{Text: "HELIUM"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "t" {
		t.Fatalf("expected token-only match to find 't', got %v", ids(got))
	}
}

func TestSearchTextEscapesLikeMetacharacters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustPut(t, s, mkItem("pct", "hdd", 1, time.Unix(100, 0), "50% off drives"))
	mustPut(t, s, mkItem("plain", "hdd", 1, time.Unix(100, 0), "some drives"))

	got, err := s.Search(ctx, store.Query{Text: "50%"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "pct" {
		t.Fatalf("expected literal '50%%' match to find only 'pct', got %v", ids(got))
	}
}

// TestAttributesAndTokensRoundTrip proves the JSON-backed columns survive a
// Put/Get round trip intact, including multiple attributes and tokens.
func TestAttributesAndTokensRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	it := mkItem("rt", "land", 12345, time.Unix(600, 0), "40 acre parcel")
	it.Attributes = map[string]string{
		"acreage": "40",
		"zoning":  "agricultural",
		"county":  "El Paso",
	}
	it.Tokens = []string{"land", "40-acres", "el-paso", "agricultural"}

	if err := s.Put(ctx, it); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok, err := s.Get(ctx, "rt")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}

	if len(got.Attributes) != len(it.Attributes) {
		t.Fatalf("attributes length mismatch: want %v got %v", it.Attributes, got.Attributes)
	}
	for k, v := range it.Attributes {
		if got.Attributes[k] != v {
			t.Fatalf("attribute %q: want %q got %q", k, v, got.Attributes[k])
		}
	}

	if len(got.Tokens) != len(it.Tokens) {
		t.Fatalf("tokens length mismatch: want %v got %v", it.Tokens, got.Tokens)
	}
	for i, tok := range it.Tokens {
		if got.Tokens[i] != tok {
			t.Fatalf("token[%d]: want %q got %q", i, tok, got.Tokens[i])
		}
	}

	// The round-tripped item must also still be findable via text search
	// over its tokens, proving the FTS/LIKE path sees the same data.
	found, err := s.Search(ctx, store.Query{Text: "el-paso"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 || found[0].ID != "rt" {
		t.Fatalf("expected token search to find 'rt', got %v", ids(found))
	}
}

// TestAttributesAndTokensEmptyRoundTrip proves items with no Attributes or
// Tokens (the common case) round-trip to nil/empty without error.
func TestAttributesAndTokensEmptyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	it := mkItem("empty", "hdd", 1, time.Unix(1, 0), "bare item")

	if err := s.Put(ctx, it); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, err := s.Get(ctx, "empty")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if len(got.Attributes) != 0 {
		t.Fatalf("expected empty attributes, got %v", got.Attributes)
	}
	if len(got.Tokens) != 0 {
		t.Fatalf("expected empty tokens, got %v", got.Tokens)
	}
}

// TestConcurrentPutAndSearch drives concurrent writers and readers at the
// Store to prove the interface's "safe for concurrent use" requirement
// actually holds under -race, not just single-goroutine tests.
func TestConcurrentPutAndSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const writers = 8
	const perWriter = 20

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("w%d-i%d", w, i)
				it := mkItem(id, "concurrent", int64(i+1), time.Unix(int64(i), 0), "item "+id)
				if err := s.Put(ctx, it); err != nil {
					t.Errorf("put %s: %v", id, err)
					return
				}
				if _, err := s.Search(ctx, store.Query{Category: "concurrent", Limit: 5}); err != nil {
					t.Errorf("search: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	got, err := s.Search(ctx, store.Query{Category: "concurrent"})
	if err != nil {
		t.Fatalf("final search: %v", err)
	}
	if len(got) != writers*perWriter {
		t.Fatalf("expected %d items, got %d", writers*perWriter, len(got))
	}
}

func mustPut(t *testing.T, s *Store, it item.Item) {
	t.Helper()
	if err := s.Put(context.Background(), it); err != nil {
		t.Fatalf("put %s: %v", it.ID, err)
	}
}
