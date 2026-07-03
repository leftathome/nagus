package category

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/enrich/geo"
	"github.com/leftathome/nagus/internal/enrich/parcel"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/store"
)

// --- structure-first rubric (pure) ---

func TestScoreLandStructureFirst(t *testing.T) {
	cases := []struct {
		name       string
		sig        landSignals
		priceKnown bool
		enriched   bool
		want       string
	}{
		{"great: structure+dominant+lowflood+price", landSignals{StructurePresent: true, LandValueDominant: true, FloodLow: true, PriceOK: true, AcreageOK: true}, true, true, "great"},
		{"poor: high flood vetoes structure", landSignals{StructurePresent: true, LandValueDominant: true, FloodHigh: true, PriceOK: true}, true, true, "poor"},
		{"poor: wetland", landSignals{Wetland: true, AcreageOK: true, PriceOK: true}, true, true, "poor"},
		{"poor: unpriced and no structure", landSignals{AcreageOK: true, FloodLow: true}, false, true, "poor"},
		{"good: structure but not land-dominant", landSignals{StructurePresent: true, FloodLow: true, PriceOK: true}, true, true, "good"},
		{"good: buildable no structure", landSignals{FloodLow: true, AcreageOK: true, PriceOK: true}, true, true, "good"},
		{"market: enriched, unremarkable", landSignals{AcreageOK: true, PriceOK: true}, true, true, "market"},
		{"market: unassessed but typed fit", landSignals{AcreageOK: true, PriceOK: true}, true, false, "market"},
		{"unknown: unassessed and no fit", landSignals{}, false, false, "unknown-no-reference"},
	}
	for _, c := range cases {
		if got := scoreLand(c.sig, c.priceKnown, c.enriched); got != c.want {
			t.Errorf("%s: scoreLand = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFloodClassification(t *testing.T) {
	for _, z := range []string{"A", "AE", "VE", "AO"} {
		if !isFloodHigh(z) || isFloodLow(z) {
			t.Errorf("zone %s should be high, not low", z)
		}
	}
	for _, z := range []string{"X", "X500", "C"} {
		if !isFloodLow(z) || isFloodHigh(z) {
			t.Errorf("zone %s should be low, not high", z)
		}
	}
	// Unknown/empty zone is neither (cannot be "great", is not "poor").
	if isFloodHigh("") || isFloodLow("") {
		t.Error("empty zone must be neither high nor low")
	}
}

// --- end-to-end via the pipeline with fake enrichers ---

type fakeGeo struct{ zone string }

func (f fakeGeo) Geocode(_ context.Context, _ string) (float64, float64, error) {
	return 38.5, -122.5, nil // valid coords for any address
}
func (f fakeGeo) Enrich(_ context.Context, _, _ float64) (geo.Result, error) {
	return geo.Result{Flood: &geo.FloodInfo{Zone: f.zone}}, nil
}

type fakeParcel struct{ byAddr map[string]parcel.ParcelData }

func (f fakeParcel) Lookup(_ context.Context, address string) (parcel.ParcelData, error) {
	return f.byAddr[address], nil // zero value when absent (no structure)
}

func putLand(t *testing.T, st store.Store, id, acreage, location string, cents int64) {
	t.Helper()
	it := item.Item{
		ID: id, Category: "land", Class: item.ClassDurable, Title: id,
		PriceCents: cents, Currency: "USD", SourceID: "craigslist", SourceKey: id,
		SeenAt:     time.Unix(1000, 0),
		Attributes: map[string]string{"acreage": acreage, "location": location},
	}
	if err := st.Put(context.Background(), it); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
}

func TestLandPipelineStructureFirstEndToEnd(t *testing.T) {
	st := store.NewMemoryStore()
	putLand(t, st, "with-structure", "5", "A", 4000000) // $40k, 5ac
	putLand(t, st, "bare-lot", "10", "B", 3000000)      // $30k, 10ac, no structure
	putLand(t, st, "tiny", "0.5", "C", 1000000)         // filtered out (<1ac)

	deps := LandDeps{
		Store: st,
		Geo:   fakeGeo{zone: "X"}, // low flood everywhere
		Parcel: fakeParcel{byAddr: map[string]parcel.ParcelData{
			// land-value-dominant structure -> great
			"A": {AssessedImprovementValueCents: 5000000, AssessedLandValueCents: 8000000, YearBuilt: 1985},
			// "B" absent -> zero ParcelData -> no structure
		}},
		Score: LandScoreConfig{BudgetCents: 5000000, MinAcreageAcres: 1},
	}
	p := NewLandPipeline(nil, deps)

	res, err := p.Surface(context.Background(), store.Query{Category: "land"})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if res.Matched != 3 || res.Filtered != 2 {
		t.Fatalf("matched=%d filtered=%d, want 3/2 (tiny dropped)", res.Matched, res.Filtered)
	}
	// with-structure -> great (structure + land-dominant + low flood + in budget);
	// bare-lot -> good (low flood + acreage + price, no structure). Great ranks first.
	if res.Items[0].Item.ID != "with-structure" || res.Items[0].Signal.Verdict != "great" {
		t.Fatalf("top = %s/%s, want with-structure/great", res.Items[0].Item.ID, res.Items[0].Signal.Verdict)
	}
	if res.Items[1].Item.ID != "bare-lot" || res.Items[1].Signal.Verdict != "good" {
		t.Fatalf("second = %s/%s, want bare-lot/good", res.Items[1].Item.ID, res.Items[1].Signal.Verdict)
	}
}
