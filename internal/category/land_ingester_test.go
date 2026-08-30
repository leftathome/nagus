package category

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/store"
)

// landingConnector is a fake listing.Connector for NewLandIngester tests: it
// emits a fixed batch of Raw listings, one that the land extractor accepts and
// one that it rejects (negative price fails item.Validate).
type landingConnector struct {
	id   string
	raws []listing.Raw
}

func (c landingConnector) SourceID() string { return c.id }
func (c landingConnector) Fetch(context.Context) ([]listing.Raw, error) {
	return c.raws, nil
}

// TestNewLandIngesterEndToEnd drives NewLandIngester's whole wire-up --
// Connector -> Passthrough sanitizer -> extland.Extractor -> Store -- and
// checks both the ingest outcome (one stored, one skipped at extract) and that
// LandDeps' retention/offer fields actually land on the returned Ingester.
func TestNewLandIngesterEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	offers := offer.NewMemoryStore()

	// SeenAt must be recent: deps.StaleAfter below drives a post-ingest purge
	// against real time, and a stale SeenAt would purge good1 right after it
	// is stored, defeating the point of this test.
	seenAt := time.Now()

	conn := landingConnector{
		id: "landsource",
		raws: []listing.Raw{
			{
				SourceID:   "landsource",
				SourceKey:  "good1",
				SourceURL:  "https://example.test/good1",
				Title:      "5 acres with well",
				Body:       "Buildable lot, septic needed.",
				PriceCents: 4_000_000,
				Currency:   "USD",
				SeenAt:     seenAt,
			},
			{
				// Negative price fails item.Validate inside extland.Extract, so
				// this must surface as a Skip at the "extract" stage, not get
				// stored.
				SourceID:   "landsource",
				SourceKey:  "bad1",
				Title:      "Nice flat lot",
				PriceCents: -100,
				Currency:   "USD",
				SeenAt:     seenAt,
			},
		},
	}

	deps := LandDeps{
		Store:            st,
		StaleAfter:       2 * time.Hour,
		Offers:           offers,
		OfferRetention:   offer.Retention{Policy: offer.RetainFull},
		OfferExpireAfter: 30 * time.Minute,
	}

	ing := NewLandIngester(conn, deps)

	// --- wiring: LandDeps' retention/offer fields must land on the Ingester ---
	if ing.StaleAfter != deps.StaleAfter {
		t.Errorf("StaleAfter = %v, want %v (deps.StaleAfter must pass through)", ing.StaleAfter, deps.StaleAfter)
	}
	if ing.Offers != deps.Offers {
		t.Errorf("Offers = %v, want the offer store from deps", ing.Offers)
	}
	if ing.OfferRetention != deps.OfferRetention {
		t.Errorf("OfferRetention = %+v, want %+v", ing.OfferRetention, deps.OfferRetention)
	}
	if ing.OfferExpireAfter != deps.OfferExpireAfter {
		t.Errorf("OfferExpireAfter = %v, want %v", ing.OfferExpireAfter, deps.OfferExpireAfter)
	}
	if ing.Store != st {
		t.Errorf("Store not wired through from deps")
	}
	if ing.Extractor == nil {
		t.Fatal("Extractor not wired (land ingest must use extland.New())")
	}
	if got := ing.Extractor.Category(); got != "land" {
		t.Errorf("Extractor.Category() = %q, want %q", got, "land")
	}

	// --- behavior: Ingest actually runs the wired pipeline ---
	res, err := ing.Ingest(ctx)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Fetched != 2 || res.Stored != 1 {
		t.Fatalf("Fetched=%d Stored=%d, want 2/1", res.Fetched, res.Stored)
	}
	// bad1's negative price is invalid for BOTH the offer layer (offer.Validate
	// rejects PriceCents < 0) and item extraction (item.Validate, same rule via
	// extland), so it produces two skips, one per stage -- neither for good1.
	if res.OffersRecorded != 1 {
		t.Errorf("OffersRecorded = %d, want 1 (only good1)", res.OffersRecorded)
	}
	if len(res.Skips) != 2 {
		t.Fatalf("Skips = %d, want 2: %+v", len(res.Skips), res.Skips)
	}
	stages := map[string]bool{}
	for _, sk := range res.Skips {
		if sk.SourceKey != "bad1" {
			t.Errorf("unexpected skip for %q, want only bad1: %+v", sk.SourceKey, sk)
		}
		stages[sk.Stage] = true
	}
	if !stages["offer"] || !stages["extract"] {
		t.Errorf("skip stages = %v, want both %q and %q", stages, "offer", "extract")
	}

	// The valid listing must be findable via Search ...
	found, err := st.Search(ctx, store.Query{Category: "land"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("Search returned %d items, want 1", len(found))
	}
	got := found[0]
	if got.Category != "land" {
		t.Errorf("Category = %q, want %q", got.Category, "land")
	}
	if got.SourceKey != "good1" {
		t.Errorf("SourceKey = %q, want %q", got.SourceKey, "good1")
	}
	if got.Attributes["acreage"] != "5" {
		t.Errorf("acreage = %q, want %q", got.Attributes["acreage"], "5")
	}

	// ... and via Get by the id Search resolved.
	byID, ok, err := st.Get(ctx, got.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get(%q): not found", got.ID)
	}
	if byID.SourceKey != "good1" || byID.Category != "land" {
		t.Errorf("Get(%q) = %+v, want SourceKey=good1 Category=land", got.ID, byID)
	}

	// The rejected listing must never have been stored.
	for _, it := range found {
		if it.SourceKey == "bad1" {
			t.Fatalf("bad1 was stored despite failing extraction: %+v", it)
		}
	}
}
