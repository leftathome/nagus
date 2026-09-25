package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/shopify"
	"github.com/leftathome/nagus/internal/offer"
)

// nagus-bu2, end to end through the real Shopify connector: a product that sat
// past the page cap kept the seller-SKU hint it was stored with before the
// QUARK-02 re-key, and with it the quark product id resolved from that hint.
// Once the walk covers the store, the tail product is re-ingested with its
// manufacturer-part-number hint, which resets its resolution so quark
// re-resolves it; and because the walk completed, offer expiry runs and marks
// a product the store no longer lists as expired.
func TestTailOfferReResolvesAfterACompleteWalk(t *testing.T) {
	const total = 7 // limit 2 -> 4 pages; product 7 alone on the last
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit, page := 0, 0
		_, _ = fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		var parts []string
		for n := (page-1)*limit + 1; n <= page*limit && n <= total; n++ {
			sku := fmt.Sprintf("DRV%04dX", n)
			title := fmt.Sprintf("Drive %s 8TB", sku)
			if n == total {
				sku, title = "HUS726040AL4215-DELL_DELLG13_SR", "HGST Ultrastar 7K6000 HUS726040AL4215 4TB SAS"
			}
			parts = append(parts, fmt.Sprintf(`{"id":%d,"title":%q,"handle":"d%d","product_type":"Hard Drives > 8TB > 3.5","tags":["brand:HGST"],"variants":[{"id":%d,"price":"10.00","available":true,"sku":%q}]}`, n, title, n, 1000+n, sku))
		}
		_, _ = w.Write([]byte(`{"products":[` + strings.Join(parts, ",") + `]}`))
	}))
	defer srv.Close()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	src := "shopify:serverpartdeals"
	ctx := context.Background()
	offers := offer.NewMemoryStore()

	// The tail offer as the re-key left it: the whole seller SKU as its MPN,
	// resolved by quark to the old product.
	stale := offer.Offer{
		SourceID: src, SourceKey: fmt.Sprintf("%d:%d", total, 1000+total), Title: "HGST Ultrastar 7K6000 HUS726040AL4215 4TB SAS",
		PriceCents: 1000, Currency: "USD", LastSeen: now.Add(-48 * time.Hour),
		ProductHint: offer.ProductHint{Brand: "HGST", MPN: "HUS726040AL4215-DELL_DELLG13"},
	}
	stale.ID = offer.DeterministicID(stale.SourceID, stale.SourceKey)
	gone := offer.Offer{SourceID: src, SourceKey: "99:1099", Title: "Withdrawn drive", PriceCents: 500, Currency: "USD", LastSeen: now.Add(-48 * time.Hour)}
	gone.ID = offer.DeterministicID(gone.SourceID, gone.SourceKey)
	for _, o := range []offer.Offer{stale, gone} {
		if err := offers.Put(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := offers.RecordResolution(ctx, stale.ID, stale.ProductHint.Fingerprint(), offer.Resolution{State: offer.ResolutionResolved, ProductID: "q-old-sku"}); err != nil || !ok {
		t.Fatalf("seed resolution: ok=%v err=%v", ok, err)
	}

	ingest := func(maxPages int) {
		t.Helper()
		conn := shopify.NewConnector(shopify.Config{
			Name: "serverpartdeals", BaseURL: srv.URL, Limit: 2, MaxPages: maxPages, PageDelay: -1,
			BrandTag: "brand:", SKUIsMPN: true, SKUSuffixes: []string{"_SR", "_MR", "_NB"},
			Now: func() time.Time { return now },
		})
		ing := &Ingester{Connector: conn, Offers: offers, OfferExpireAfter: 24 * time.Hour, Now: func() time.Time { return now }}
		if _, err := ing.Ingest(ctx); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id string) offer.Offer {
		t.Helper()
		o, ok, err := offers.Get(ctx, id)
		if err != nil || !ok {
			t.Fatalf("offer %s: ok=%v err=%v", id, ok, err)
		}
		return o
	}

	// Capped short of the tail: the stale identity survives and nothing expires.
	ingest(3)
	if o := get(stale.ID); o.ProductHint.MPN != stale.ProductHint.MPN || o.Resolution.ProductID != "q-old-sku" {
		t.Fatalf("capped walk touched the tail: hint %+v resolution %+v", o.ProductHint, o.Resolution)
	}
	if o := get(gone.ID); o.Status != offer.StatusActive {
		t.Fatalf("an incomplete walk expired an offer: %+v", o.Status)
	}

	// Covering the store: the tail is refreshed with its MPN and re-resolves.
	ingest(5)
	o := get(stale.ID)
	if o.ProductHint.MPN != "HUS726040AL4215" || o.ProductHint.Brand != "HGST" {
		t.Fatalf("tail hint = %+v, want the manufacturer part number HUS726040AL4215", o.ProductHint)
	}
	if o.Resolution.State != offer.ResolutionUnattempted || o.Resolution.ProductID != "" {
		t.Fatalf("tail resolution = %+v, want it reset so quark re-resolves the new hint", o.Resolution)
	}
	if g := get(gone.ID); g.Status != offer.StatusExpired {
		t.Fatalf("withdrawn offer status = %s, want expired after a complete walk", g.Status)
	}
}
