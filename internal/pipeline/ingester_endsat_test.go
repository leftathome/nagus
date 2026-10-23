package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/store"
)

// A source-stated end (listing.Raw.EndsAt) reaches the offer (nagus-417).
func TestIngestCarriesEndsAtToTheOffer(t *testing.T) {
	end := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	withEnd := raw("auction", "Seagate 16TB", 12000, "16")
	withEnd.EndsAt = end
	plain := raw("fixed", "HGST 10TB", 9000, "10")
	offers := offer.NewMemoryStore()
	ing := &Ingester{
		Connector: fakeConnector{raws: []listing.Raw{withEnd, plain}}, Sanitizer: sanitize.Passthrough{},
		Extractor: fakeExtractor{}, Store: store.NewMemoryStore(), Offers: offers,
	}
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := offers.Summaries(context.Background(), []string{
		offer.DeterministicID(withEnd.SourceID, withEnd.SourceKey),
		offer.DeterministicID(plain.SourceID, plain.SourceKey),
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("summaries: %v %v", got, err)
	}
	if a := got[offer.DeterministicID(withEnd.SourceID, withEnd.SourceKey)]; !a.EndsAt.Equal(end) {
		t.Fatalf("auction offer EndsAt = %v, want %v", a.EndsAt, end)
	}
	if f := got[offer.DeterministicID(plain.SourceID, plain.SourceKey)]; !f.EndsAt.IsZero() {
		t.Fatalf("fixed offer EndsAt = %v, want zero", f.EndsAt)
	}
}
