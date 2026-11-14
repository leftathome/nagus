package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/shopify"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/sanitize"
	"github.com/leftathome/nagus/internal/store"
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
		// Evaluating, with a passing gate: hints are recorded only for a
		// listing glovebox passed (nagus-voe).
		ing := &Ingester{
			Connector: conn, Offers: offers, OfferExpireAfter: 24 * time.Hour, Now: func() time.Time { return now },
			Sanitizer: sanitize.Passthrough{}, Extractor: fakeExtractor{}, Store: store.NewMemoryStore(),
		}
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

// nagus-voe: no structured hint reaches quark unless glovebox passed the
// listing. A refused listing's hint is withheld (its stored one, if any, is
// kept); an offer-only source -- which never crosses glovebox -- records no
// hint at all.
func TestStructuredHintsNeedTheGate(t *testing.T) {
	ctx := context.Background()
	mk := func(key string) listing.Raw {
		return listing.Raw{
			SourceID: "shopify:spd", SourceKey: key, Title: "Seagate 8TB", PriceCents: 100, Currency: "USD",
			Aspects: map[string]string{"brand": "Seagate", "mpn": "ST8000NM000A"},
		}
	}
	for _, tc := range []struct {
		name string
		ing  func(offers offer.Store, raws []listing.Raw) *Ingester
		want string
	}{
		{"passed", func(o offer.Store, r []listing.Raw) *Ingester {
			return &Ingester{Connector: fakeConnector{raws: r}, Offers: o, Sanitizer: vettingGate{}, Extractor: fakeExtractor{}, Store: store.NewMemoryStore()}
		}, "ST8000NM000A"},
		{"refused", func(o offer.Store, r []listing.Raw) *Ingester {
			return &Ingester{Connector: fakeConnector{raws: r}, Offers: o, Sanitizer: refusingSanitizer{err: errors.New("quarantined")}, Extractor: fakeExtractor{}, Store: store.NewMemoryStore()}
		}, ""},
		{"offer-only", func(o offer.Store, r []listing.Raw) *Ingester {
			return &Ingester{Connector: fakeConnector{raws: r}, Offers: o}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offers := offer.NewMemoryStore()
			raw := mk("k")
			if _, err := tc.ing(offers, []listing.Raw{raw}).Ingest(ctx); err != nil {
				t.Fatal(err)
			}
			o, ok, err := offers.Get(ctx, offer.DeterministicID(raw.SourceID, raw.SourceKey))
			if err != nil || !ok {
				t.Fatalf("offer: ok=%v err=%v (the offer itself is always recorded)", ok, err)
			}
			if o.ProductHint.MPN != tc.want || (tc.want == "" && !o.ProductHint.Empty()) || o.ProductHint.Vetted != (tc.want != "") {
				t.Fatalf("hint %+v, want MPN %q (vetted only when passed)", o.ProductHint, tc.want)
			}
		})
	}
}

// nagus-voe: hints stored before the rule were never vetted. A listing the
// gate refuses keeps only a VETTED previous hint; a passing listing vets the
// stored strings in place, keeping their resolution.
func TestStoredHintsAreKeptOnlyWhenVetted(t *testing.T) {
	ctx := context.Background()
	raw := listing.Raw{
		SourceID: "shopify:spd", SourceKey: "k", Title: "Seagate 8TB", PriceCents: 100, Currency: "USD",
		Aspects: map[string]string{"brand": "Seagate", "mpn": "ST8000NM000A"},
	}
	id := offer.DeterministicID(raw.SourceID, raw.SourceKey)
	seed := func(t *testing.T, vetted bool) offer.Store {
		offers := offer.NewMemoryStore()
		hint := offer.ProductHint{Brand: "Seagate", MPN: "ST8000NM000A", Vetted: vetted}
		o := offer.Offer{SourceID: raw.SourceID, SourceKey: raw.SourceKey, PriceCents: 100, Currency: "USD", LastSeen: time.Unix(1, 0), ProductHint: hint}
		if err := offers.Put(ctx, o); err != nil {
			t.Fatal(err)
		}
		if ok, err := offers.RecordResolution(ctx, id, hint.Fingerprint(), offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-1"}); err != nil || !ok {
			t.Fatalf("seed resolution: %v %v", ok, err)
		}
		return offers
	}
	ingest := func(t *testing.T, offers offer.Store, san listing.Sanitizer) offer.Offer {
		ing := &Ingester{Connector: fakeConnector{raws: []listing.Raw{raw}}, Offers: offers, Sanitizer: san, Extractor: fakeExtractor{}, Store: store.NewMemoryStore()}
		if _, err := ing.Ingest(ctx); err != nil {
			t.Fatal(err)
		}
		o, _, err := offers.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	refuse := refusingSanitizer{err: fmt.Errorf("%w: test", sanitize.ErrQuarantined)}
	outage := refusingSanitizer{err: errors.New("sanitize: glovebox unreachable")}
	pass := vettingGate{}

	if o := ingest(t, seed(t, false), refuse); !o.ProductHint.Empty() || o.Resolution.ProductID != "" {
		t.Fatalf("legacy unvetted hint survived a refusal: %+v %+v", o.ProductHint, o.Resolution)
	}
	if o := ingest(t, seed(t, true), refuse); o.ProductHint.MPN != "ST8000NM000A" || o.Resolution.ProductID != "p-1" {
		t.Fatalf("a vetted hint was not kept through a refusal: %+v %+v", o.ProductHint, o.Resolution)
	}
	if o := ingest(t, seed(t, false), pass); !o.ProductHint.Vetted || o.Resolution.ProductID != "p-1" {
		t.Fatalf("a passing listing did not vet the stored hint in place: %+v %+v", o.ProductHint, o.Resolution)
	}
	// An outage is not a verdict: a legacy hint is kept, still unvetted (so
	// still never sent to quark), and the product id survives.
	if o := ingest(t, seed(t, false), outage); o.ProductHint.MPN != "ST8000NM000A" || o.ProductHint.Vetted || o.Resolution.ProductID != "p-1" {
		t.Fatalf("an outage cleared or vetted a legacy hint: %+v %+v", o.ProductHint, o.Resolution)
	}
	// Passthrough (no glovebox configured) scans nothing and vets nothing.
	if o := ingest(t, seed(t, false), sanitize.Passthrough{}); o.ProductHint.Vetted {
		t.Fatalf("Passthrough vetted a hint: %+v", o.ProductHint)
	}
}

// vettingGate passes every listing like glovebox would, without being the
// Passthrough.
type vettingGate struct{}

func (vettingGate) Sanitize(ctx context.Context, r listing.Raw) (listing.Sanitized, error) {
	return sanitize.Passthrough{Name: "test.gate"}.Sanitize(ctx, r)
}
