package category

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/shipping"
	"github.com/leftathome/nagus/internal/store"
)

// partialConn is a wine connector whose fetch can be incomplete.
type partialConn struct {
	fakeWineConn
	complete bool
}

func (p *partialConn) FetchComplete() bool { return p.complete }

// The wine bundle wraps its connector; the wrapper must not hide an
// incomplete fetch, or offers nagus did not reach are expired (nagus-3ua).
func TestWineIngesterKeepsFetchCompleteness(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	raw := rawWine("a", "Example Cellars Syrah 2021", "", 2499, "USD")
	raw.SeenAt = now
	conn := &partialConn{fakeWineConn: fakeWineConn{id: "shopify:w", raws: []listing.Raw{raw}}, complete: true}
	src, _ := shipping.NewSource("retailer", "US-WA")
	offers := offer.NewMemoryStore()
	ing, err := NewWineIngester(conn, src, WineDeps{Store: store.NewMemoryStore(), Offers: offers, OfferExpireAfter: time.Hour, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	ing.Now = func() time.Time { return now }
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	rc, ok := ing.Connector.(interface{ FetchComplete() bool })
	if !ok || !rc.FetchComplete() {
		t.Fatal("the wrapped connector does not report completeness")
	}
	// Two hours later a PARTIAL fetch that did not reach the listing: it
	// must not be expired.
	conn.raws, conn.complete = nil, false
	now = now.Add(2 * time.Hour)
	if _, err := ing.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
	o, found, err := offers.Get(context.Background(), offer.DeterministicID("shopify:w", "a"))
	if err != nil || !found || o.Status != offer.StatusActive {
		t.Fatalf("offer after a partial fetch: found %v status %v err %v", found, o.Status, err)
	}
}
