package offerstoretest

import (
	"testing"

	"github.com/leftathome/nagus/internal/offer"
)

// vintageModeRoundTripsAndResets: quark's wine vintage mode (QUARK-04) is
// stored with the resolution, survives a re-ingest of the same hint, and
// resets with the rest of the resolution when the hint changes -- a mode kept
// for a hint the source no longer states would key comparisons on the wrong
// product.
func vintageModeRoundTripsAndResets(t *testing.T, s offer.Store) {
	o := Offer("shop:w", "bsc", 100, T0)
	o.ProductHint = offer.ProductHint{Brand: "Bollinger", Text: "Special Cuvee Brut NV"}
	put(t, s, o)
	if !record(t, s, o, offer.Resolution{State: offer.ResolutionResolved, ProductID: "p-bsc", VintageMode: "non_vintage", Generation: 1, At: T1}) {
		t.Fatal("RecordResolution not applied")
	}
	if got := get(t, s, o).Resolution; got.VintageMode != "non_vintage" || got.ProductID != "p-bsc" {
		t.Fatalf("round trip = %+v", got)
	}
	put(t, s, o)
	if got := get(t, s, o).Resolution; got.VintageMode != "non_vintage" {
		t.Fatalf("re-ingest of the same hint dropped the vintage mode: %+v", got)
	}
	o.ProductHint.Text = "Special Cuvee Rose Brut NV"
	put(t, s, o)
	if got := get(t, s, o).Resolution; got.VintageMode != "" || got.ProductID != "" {
		t.Fatalf("a changed hint must reset the vintage mode with the resolution: %+v", got)
	}
}
