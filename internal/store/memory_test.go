package store

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
)

func mkItem(id, cat string, price int64, seen time.Time, title string) item.Item {
	return item.Item{
		ID: id, Category: cat, Class: item.ClassDurable, Title: title,
		PriceCents: price, Currency: "USD",
		SourceID: "test", SourceKey: id, SeenAt: seen,
	}
}

func TestPutRejectsInvalidItem(t *testing.T) {
	s := NewMemoryStore()
	err := s.Put(context.Background(), item.Item{ID: ""}) // missing everything
	if err == nil {
		t.Fatal("expected Put to reject an invalid item")
	}
}

func TestPutGetRoundTripAndReplace(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	it := mkItem("a", "hdd", 10000, time.Unix(100, 0), "16TB Exos")
	if err := s.Put(ctx, it); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok, _ := s.Get(ctx, "a")
	if !ok || got.Title != "16TB Exos" {
		t.Fatalf("get round-trip failed: %+v ok=%v", got, ok)
	}
	// Replace-by-ID.
	it.PriceCents = 9000
	_ = s.Put(ctx, it)
	got, _, _ = s.Get(ctx, "a")
	if got.PriceCents != 9000 {
		t.Fatalf("expected replace to 9000, got %d", got.PriceCents)
	}
}

func TestSearchFiltersAndOrdersNewestFirst(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.Put(ctx, mkItem("old", "land", 100000, time.Unix(100, 0), "back forty"))
	_ = s.Put(ctx, mkItem("new", "land", 200000, time.Unix(300, 0), "creek parcel"))
	_ = s.Put(ctx, mkItem("hdd", "hdd", 5000, time.Unix(200, 0), "drive"))

	got, _ := s.Search(ctx, Query{Category: "land"})
	if len(got) != 2 {
		t.Fatalf("expected 2 land items, got %d", len(got))
	}
	if got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("expected newest-first [new, old], got [%s, %s]", got[0].ID, got[1].ID)
	}
}

func TestSearchPriceBoundExcludesUnknownPrice(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.Put(ctx, mkItem("cheap", "hdd", 5000, time.Unix(100, 0), "a"))
	_ = s.Put(ctx, mkItem("dear", "hdd", 50000, time.Unix(100, 0), "b"))
	_ = s.Put(ctx, mkItem("unknown", "hdd", 0, time.Unix(100, 0), "c")) // price unknown

	got, _ := s.Search(ctx, Query{Category: "hdd", MaxPriceCents: 10000})
	if len(got) != 1 || got[0].ID != "cheap" {
		t.Fatalf("price bound should return only 'cheap', got %+v", ids(got))
	}
}

func TestSearchTextAndSinceAndLimit(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_ = s.Put(ctx, mkItem("a", "land", 1, time.Unix(100, 0), "wooded ACRES near town"))
	_ = s.Put(ctx, mkItem("b", "land", 1, time.Unix(400, 0), "downtown condo"))
	c := mkItem("c", "land", 1, time.Unix(500, 0), "open acres")
	c.Tokens = []string{"acres", "pasture"}
	_ = s.Put(ctx, c)

	// case-insensitive text over title + tokens
	got, _ := s.Search(ctx, Query{Text: "acres"})
	if len(got) != 2 {
		t.Fatalf("text match expected 2, got %v", ids(got))
	}
	// Since bound
	got, _ = s.Search(ctx, Query{Since: time.Unix(450, 0)})
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("since bound expected [c], got %v", ids(got))
	}
	// Limit
	got, _ = s.Search(ctx, Query{Limit: 1})
	if len(got) != 1 || got[0].ID != "c" { // newest first
		t.Fatalf("limit expected [c], got %v", ids(got))
	}
}

func ids(items []item.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}
