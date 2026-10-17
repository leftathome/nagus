package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
)

// The retention pass purges a source's mail-reader state older than its
// MailStateWindow (nagus-p7x), and nothing without a window.
func TestRetentionPassPurgesMailState(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	offers := offer.NewMemoryStore()
	ctx := context.Background()
	_ = offers.PutPromoMessage(ctx, offer.PromoMessage{SourceID: "fake", MessageKey: "old", ProcessedAt: now.AddDate(0, 0, -30)})
	_ = offers.PutPromoMessage(ctx, offer.PromoMessage{SourceID: "fake", MessageKey: "new", ProcessedAt: now.AddDate(0, 0, -1)})
	ing := &Ingester{Connector: fakeConnector{raws: []listing.Raw{}}, Offers: offers, Now: func() time.Time { return now },
		OfferRetention: offer.Retention{Policy: offer.RetainFull}}
	if _, err := ing.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := offers.GetPromoMessage(ctx, "fake", "old"); !ok {
		t.Fatal("purged without a window")
	}
	ing.MailStateWindow = 16 * 24 * time.Hour
	if _, err := ing.Ingest(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := offers.GetPromoMessage(ctx, "fake", "old"); ok {
		t.Fatal("old state survived the retention pass")
	}
	if _, ok, _ := offers.GetPromoMessage(ctx, "fake", "new"); !ok {
		t.Fatal("recent state was purged")
	}
}
