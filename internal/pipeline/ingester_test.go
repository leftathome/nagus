package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
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

// --- offer layer (nagus-q6u increment A) --------------------------------------

// The offer layer is ADDITIVE: enabling it must not change what reaches the item
// store, because the item store is what feeds the live surface.
func TestIngestWritesOffersWithoutDisturbingItems(t *testing.T) {
	itemStore := store.NewMemoryStore()
	offers := offer.NewMemoryStore()
	now := time.Unix(1_750_000_000, 0).UTC()

	ing := &Ingester{
		Connector: idConnector{id: "shopify:x", raws: []listing.Raw{
			{SourceID: "shopify:x", SourceKey: "a", Title: "Drive A", PriceCents: 1000, Currency: "USD", SeenAt: now},
			{SourceID: "shopify:x", SourceKey: "b", Title: "Drive B", PriceCents: 2000, Currency: "USD", SeenAt: now},
		}},
		Sanitizer: sanitize.Passthrough{Name: "test"},
		Extractor: fakeExtractor{},
		Store:     itemStore,
		Offers:    offers,
		Now:       func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Stored != 2 {
		t.Errorf("Stored = %d, want 2 (item path must be unaffected)", res.Stored)
	}
	if res.OffersRecorded != 2 {
		t.Errorf("OffersRecorded = %d, want 2", res.OffersRecorded)
	}
	got, err := offers.Query(context.Background(), offer.Query{})
	if err != nil {
		t.Fatalf("offer Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("offer store holds %d, want 2", len(got))
	}
	for _, o := range got {
		if !o.Purchasable() {
			t.Errorf("freshly ingested offer %q should be purchasable", o.SourceKey)
		}
	}
}

// Housekeeping runs on ingest regardless of evaluation: offers the source
// stopped showing become expired but are RETAINED.
func TestIngestExpiresOffersButRetainsThem(t *testing.T) {
	offers := offer.NewMemoryStore()
	now := time.Unix(1_750_000_000, 0).UTC()
	// An old offer from a previous run.
	if err := offers.Put(context.Background(), offer.Offer{
		SourceID: "shopify:x", SourceKey: "gone", PriceCents: 500, Currency: "USD",
		LastSeen: now.Add(-72 * time.Hour), Status: offer.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	ing := &Ingester{
		Connector: idConnector{id: "shopify:x", raws: []listing.Raw{
			{SourceID: "shopify:x", SourceKey: "still-here", Title: "T", PriceCents: 100, Currency: "USD", SeenAt: now},
		}},
		Sanitizer:        sanitize.Passthrough{Name: "test"},
		Extractor:        fakeExtractor{},
		Store:            store.NewMemoryStore(),
		Offers:           offers,
		OfferExpireAfter: 24 * time.Hour,
		Now:              func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersExpired != 1 {
		t.Fatalf("OffersExpired = %d, want 1", res.OffersExpired)
	}
	if offers.Len() != 2 {
		t.Fatalf("offer store holds %d, want 2 -- expiry must RETAIN", offers.Len())
	}
	live, _ := offers.Query(context.Background(), offer.Query{})
	if len(live) != 1 || live[0].SourceKey != "still-here" {
		t.Errorf("purchasable offers = %v, want only still-here", live)
	}
}

// Retention is the only thing that deletes, and it is per-source.
func TestIngestAppliesPerSourceRetention(t *testing.T) {
	offers := offer.NewMemoryStore()
	now := time.Unix(1_750_000_000, 0).UTC()
	if err := offers.Put(context.Background(), offer.Offer{
		SourceID: "ebay:ebay", SourceKey: "ancient", PriceCents: 500, Currency: "USD",
		LastSeen: now.Add(-48 * time.Hour), Status: offer.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	ing := &Ingester{
		Connector:      idConnector{id: "ebay:ebay"},
		Sanitizer:      sanitize.Passthrough{Name: "test"},
		Extractor:      fakeExtractor{},
		Store:          store.NewMemoryStore(),
		Offers:         offers,
		OfferRetention: offer.Retention{Policy: offer.Purge, Window: 6 * time.Hour},
		Now:            func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersPurged != 1 || offers.Len() != 0 {
		t.Fatalf("OffersPurged=%d len=%d, want 1 purged leaving 0", res.OffersPurged, offers.Len())
	}
}

// A nil offer store leaves behaviour exactly as before the layer existed.
func TestIngestWithoutOfferStoreIsUnchanged(t *testing.T) {
	itemStore := store.NewMemoryStore()
	ing := &Ingester{
		Connector: idConnector{id: "shopify:x", raws: []listing.Raw{
			{SourceID: "shopify:x", SourceKey: "a", Title: "T", PriceCents: 1000, Currency: "USD"},
		}},
		Sanitizer: sanitize.Passthrough{Name: "test"},
		Extractor: fakeExtractor{},
		Store:     itemStore,
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Stored != 1 || res.OffersRecorded != 0 {
		t.Fatalf("Stored=%d OffersRecorded=%d, want 1 and 0", res.Stored, res.OffersRecorded)
	}
}

// idConnector is fakeConnector with a configurable SourceID, which the offer
// layer needs because offers and their retention are scoped per source.
type idConnector struct {
	id   string
	raws []listing.Raw
}

func (c idConnector) SourceID() string { return c.id }
func (c idConnector) Fetch(context.Context) ([]listing.Raw, error) {
	return c.raws, nil
}

// --- offer-only sources (gate-at-eval, nagus-7yq) ------------------------------

// A source with no evaluation machinery records offers and stops. This is what
// lets a source be collected speculatively -- accumulating history for goods no
// category evaluates yet -- at ZERO extraction cost.
func TestOfferOnlySourceRecordsOffersAndDoesNotEvaluate(t *testing.T) {
	offers := offer.NewMemoryStore()
	itemStore := store.NewMemoryStore()
	now := time.Unix(1_750_000_000, 0).UTC()

	ing := &Ingester{
		Connector: idConnector{id: "shopify:speculative", raws: []listing.Raw{
			{SourceID: "shopify:speculative", SourceKey: "a", Title: "A whole server", PriceCents: 381880, Currency: "USD", SeenAt: now},
			{SourceID: "shopify:speculative", SourceKey: "b", Title: "Another server", PriceCents: 250000, Currency: "USD", SeenAt: now},
		}},
		Offers: offers,
		Now:    func() time.Time { return now },
		// No Sanitizer, Extractor or Store: nothing evaluates this source.
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersRecorded != 2 {
		t.Errorf("OffersRecorded = %d, want 2", res.OffersRecorded)
	}
	if res.Stored != 0 {
		t.Errorf("Stored = %d, want 0 -- an offer-only source must not produce items", res.Stored)
	}
	if len(res.Skips) != 0 {
		t.Errorf("skips = %v, want none: not evaluating is the intended behaviour, not a failure", res.Skips)
	}
	if n, _ := itemStore.Search(context.Background(), store.Query{}); len(n) != 0 {
		t.Errorf("item store got %d rows from an offer-only source", len(n))
	}
	got, _ := offers.Query(context.Background(), offer.Query{})
	if len(got) != 2 {
		t.Fatalf("offer store holds %d, want 2", len(got))
	}
}

// A nil Sanitizer must not panic -- the glovebox crossing is deliberately not
// performed for goods nothing evaluates (that is the cost saving).
func TestOfferOnlySourceSkipsTheGloveboxCrossing(t *testing.T) {
	ing := &Ingester{
		Connector: idConnector{id: "s", raws: []listing.Raw{{SourceID: "s", SourceKey: "k", Title: "T", PriceCents: 1, Currency: "USD"}}},
		Offers:    offer.NewMemoryStore(),
		Now:       func() time.Time { return time.Unix(1_750_000_000, 0).UTC() },
	}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatalf("Ingest with no Sanitizer must not error: %v", err)
	}
}

// Offer housekeeping still runs for an offer-only source -- expiry and retention
// are properties of the SOURCE, not of whether anything evaluates it.
func TestOfferOnlySourceStillDoesHousekeeping(t *testing.T) {
	offers := offer.NewMemoryStore()
	now := time.Unix(1_750_000_000, 0).UTC()
	if err := offers.Put(context.Background(), offer.Offer{
		SourceID: "s", SourceKey: "old", PriceCents: 100, Currency: "USD",
		LastSeen: now.Add(-72 * time.Hour), Status: offer.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	ing := &Ingester{
		Connector:        idConnector{id: "s"},
		Offers:           offers,
		OfferExpireAfter: 24 * time.Hour,
		Now:              func() time.Time { return now },
	}
	res, err := ing.Ingest(context.Background())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.OffersExpired != 1 {
		t.Fatalf("OffersExpired = %d, want 1 -- housekeeping is independent of evaluation", res.OffersExpired)
	}
}
