package enrich

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/quark"
)

// wineQuark answers like quark's LWIN name tier (QUARK-04): fuzzy with an id,
// adjudicate and unmatched without one.
type wineQuark struct{ calls [][]quark.Hint }

func (w *wineQuark) Resolve(_ context.Context, hints []quark.Hint, _ bool) (quark.Response, error) {
	w.calls = append(w.calls, hints)
	out := quark.Response{CatalogGeneration: 1, Results: make([]quark.Result, len(hints))}
	for i, h := range hints {
		switch h.Text {
		case "2019 Cabernet Sauvignon, Walla Walla Valley":
			out.Results[i] = quark.Result{Route: quark.RouteFuzzy, ProductID: "p-lwin-1101245", Confidence: 100, Standing: "authoritative",
				Specs: []quark.Spec{{Key: quark.Untrusted{Value: "vintage_mode"}, Value: quark.Untrusted{Value: "vintage"}, Tier: "catalog"}}}
		case "2019 Reserve Cabernet, Seven Hills Vineyard":
			out.Results[i] = quark.Result{Route: quark.RouteAdjudicate, Confidence: 100, Reason: "coverage"}
		default:
			out.Results[i] = quark.Result{Route: quark.RouteUnmatched, Reason: "low_score"}
		}
	}
	return out, nil
}

// A wine name hint (producer + title) goes to quark as brand + text in
// category wine. Only the fuzzy route -- quark's auto band -- records a
// product id: the rule "only auto-route matches ever stamp an identity"
// survives the move of the resolver. Adjudicate and unmatched record refused,
// retried when quark's catalog generation advances.
func TestWineNameHintsStampOnlyTheAutoBand(t *testing.T) {
	s := offer.NewMemoryStore()
	mk := func(key, title string) offer.Offer {
		o := offer.Offer{SourceID: "shopify:leonetti", SourceKey: key, PriceCents: 100, LastSeen: t0,
			ProductHint: offer.ProductHint{Brand: "Leonetti Cellar", Text: title}}
		if err := s.Put(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		o.ID = offer.DeterministicID(o.SourceID, key)
		return o
	}
	auto := mk("a", "2019 Cabernet Sauvignon, Walla Walla Valley")
	adj := mk("b", "2019 Reserve Cabernet, Seven Hills Vineyard")
	none := mk("c", "Gift Card")
	q := &wineQuark{}
	e := &Enricher{Offers: s, Quark: q, Now: func() time.Time { return t0 },
		CategoryFor: func(string) string { return "wine" }}
	if _, err := e.RunPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(q.calls) != 1 || len(q.calls[0]) != 3 {
		t.Fatalf("calls = %+v", q.calls)
	}
	for _, h := range q.calls[0] {
		if h.Category != "wine" || h.Brand != "Leonetti Cellar" || h.Text == "" || h.MPN != "" || h.GTIN != "" || h.Model != "" {
			t.Fatalf("wine hint on the wire = %+v", h)
		}
	}
	if r := state(t, s, auto); r.State != offer.ResolutionResolved || r.ProductID != "p-lwin-1101245" || r.VintageMode != "vintage" {
		t.Errorf("fuzzy route: %+v, want resolved with the product id and quark's vintage mode", r)
	}
	for name, o := range map[string]offer.Offer{"adjudicate": adj, "unmatched": none} {
		if r := state(t, s, o); r.State != offer.ResolutionRefused || r.ProductID != "" {
			t.Errorf("%s route: %+v, want refused with no id", name, r)
		}
	}
	if st := e.Snapshot(); st.Resolved != 1 || st.Refused != 2 {
		t.Errorf("stats = %+v", st)
	}
}
