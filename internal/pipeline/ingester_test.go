package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/store"
)

func TestIngesterStoresAndSkips(t *testing.T) {
	raws := []listing.Raw{
		raw("a", "Seagate 16TB", 12000, "16"),
		raw("bad", "broken listing", 9999, "8"),
		raw("b", "WD 8TB", 8000, "8"),
	}
	st := store.NewMemoryStore()
	ing := &Ingester{Connector: fakeConnector{raws: raws}, Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest error: %v", err)
	}
	if res.Fetched != 3 || res.Stored != 2 {
		t.Fatalf("Fetched=%d Stored=%d, want 3/2", res.Fetched, res.Stored)
	}
	if len(res.Skips) != 1 || res.Skips[0].Stage != "extract" || res.Skips[0].SourceKey != "bad" {
		t.Fatalf("expected one extract skip for 'bad', got %+v", res.Skips)
	}
	if _, ok, _ := st.Get(context.Background(), "a"); !ok {
		t.Fatal("item 'a' not stored")
	}
	if _, ok, _ := st.Get(context.Background(), "bad"); ok {
		t.Fatal("item 'bad' should not be stored")
	}
}

func TestIngesterConnectorErrorAborts(t *testing.T) {
	st := store.NewMemoryStore()
	ing := &Ingester{
		Connector: fakeConnector{err: errors.New("network down")},
		Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st,
	}
	if _, err := ing.Ingest(context.Background()); err == nil {
		t.Fatal("expected Ingest to propagate the connector Fetch error")
	}
}

func TestIngesterPurgesStaleSourceItems(t *testing.T) {
	freshRaw := listing.Raw{
		SourceID: "fake", SourceKey: "fresh", Title: "Seagate 16TB",
		PriceCents: 12000, Currency: "USD", ConditionRaw: "refurb",
		Aspects: map[string]string{"capacity_tb": "16"}, SeenAt: time.Unix(100000, 0),
	}
	st := store.NewMemoryStore()
	ing := &Ingester{
		Connector: fakeConnector{raws: []listing.Raw{freshRaw}},
		Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: st,
	}
	ing.StaleAfter = 1000 * time.Second
	ing.Now = func() time.Time { return time.Unix(100000, 0) }

	ctx := context.Background()
	// Pre-seed a stale item from the SAME source and a stale item from ANOTHER
	// source. Only the same-source stale one is eBay-style content past its window.
	staleSame := item.Item{
		ID: "stale", Category: "hdd", Class: item.ClassDurable, Title: "old drive",
		PriceCents: 5000, Currency: "USD", SourceID: "fake", SourceKey: "stale",
		SeenAt: time.Unix(1000, 0),
	}
	staleOther := item.Item{
		ID: "other", Category: "hdd", Class: item.ClassDurable, Title: "old thing",
		PriceCents: 5000, Currency: "USD", SourceID: "landsource", SourceKey: "other",
		SeenAt: time.Unix(1000, 0),
	}
	if err := st.Put(ctx, staleSame); err != nil {
		t.Fatalf("seed staleSame: %v", err)
	}
	if err := st.Put(ctx, staleOther); err != nil {
		t.Fatalf("seed staleOther: %v", err)
	}

	res, err := ing.Ingest(ctx)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Purged != 1 {
		t.Fatalf("Purged=%d, want 1 (the same-source stale item)", res.Purged)
	}
	if _, ok, _ := st.Get(ctx, "stale"); ok {
		t.Fatal("stale same-source item should be purged")
	}
	if _, ok, _ := st.Get(ctx, "fresh"); !ok {
		t.Fatal("freshly-ingested item must survive")
	}
	if _, ok, _ := st.Get(ctx, "other"); !ok {
		t.Fatal("other-source stale item must be untouched by a scoped purge")
	}
}
