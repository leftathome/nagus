package category

import (
	"context"
	"strings"
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
		PriceCents: cents, Currency: "USD", SourceID: "landsource", SourceKey: id,
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
	sf := NewLandSurface(deps)

	res, err := sf.Surface(context.Background(), store.Query{Category: "land"})
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

// --- positioning: coordinates vs city centroid (nagus-hla follow-up) ----------

// countingGeo records how it was used so a test can prove a code path was NOT
// taken -- specifically that an exact-coordinate source never geocodes.
type countingGeo struct {
	geocodeCalls int
	geocodeArgs  []string
	enrichLat    float64
	enrichLon    float64
	enrichCalls  int
	geocodeLat   float64
	geocodeLon   float64
	geocodeErr   error
	result       geo.Result
}

func (c *countingGeo) Geocode(_ context.Context, addr string) (float64, float64, error) {
	c.geocodeCalls++
	c.geocodeArgs = append(c.geocodeArgs, addr)
	if c.geocodeErr != nil {
		return 0, 0, c.geocodeErr
	}
	return c.geocodeLat, c.geocodeLon, nil
}

func (c *countingGeo) Enrich(_ context.Context, lat, lon float64) (geo.Result, error) {
	c.enrichCalls++
	c.enrichLat, c.enrichLon = lat, lon
	return c.result, nil
}

type recordingParcel struct {
	args []string
	data parcel.ParcelData
	err  error
}

func (r *recordingParcel) Lookup(_ context.Context, addr string) (parcel.ParcelData, error) {
	r.args = append(r.args, addr)
	return r.data, r.err
}

func landItemWith(attrs map[string]string) item.Item {
	it := item.Item{
		ID: "t1", Category: "land", Class: item.ClassDurable,
		PriceCents: 15_000_000, Currency: "USD",
		SourceID: "zillapi:north-sound", SourceKey: "z1",
		Attributes: map[string]string{},
	}
	for k, v := range attrs {
		it.Attributes[k] = v
	}
	return it
}

// THE BUG THIS FIXES: "location" is a CITY label ("Sedro Woolley, WA"), so
// geocoding it returns the city centroid and the flood zone would describe
// downtown rather than the parcel. When the source supplies exact coordinates
// they must be used verbatim and Geocode must never be called.
func TestLandSignalsUseExactCoordinatesAndNeverGeocode(t *testing.T) {
	g := &countingGeo{geocodeLat: 48.4, geocodeLon: -122.2} // the WRONG (centroid) answer
	it := landItemWith(map[string]string{
		"acreage":  "5.19",
		"location": "Sedro Woolley, WA",
		"lat":      "48.5041",
		"lon":      "-122.2359",
	})
	_, enriched := buildLandSignals(context.Background(), it, g, nil, LandScoreConfig{MinAcreageAcres: 1})
	if !enriched {
		t.Fatal("expected enrichment to run")
	}
	if g.geocodeCalls != 0 {
		t.Errorf("Geocode called %d times with %v; exact coordinates must bypass geocoding entirely", g.geocodeCalls, g.geocodeArgs)
	}
	if g.enrichLat != 48.5041 || g.enrichLon != -122.2359 {
		t.Errorf("enriched at (%v,%v), want the listing's own (48.5041,-122.2359)", g.enrichLat, g.enrichLon)
	}
}

// Sources without coordinates keep the old behavior.
func TestLandSignalsFallBackToGeocodingWithoutCoordinates(t *testing.T) {
	g := &countingGeo{geocodeLat: 47.6, geocodeLon: -122.3}
	it := landItemWith(map[string]string{"acreage": "3", "location": "Seattle, WA"})
	_, enriched := buildLandSignals(context.Background(), it, g, nil, LandScoreConfig{MinAcreageAcres: 1})
	if !enriched {
		t.Fatal("expected enrichment to run via the geocode fallback")
	}
	if g.geocodeCalls != 1 {
		t.Fatalf("Geocode called %d times, want 1", g.geocodeCalls)
	}
	if g.enrichLat != 47.6 || g.enrichLon != -122.3 {
		t.Errorf("enriched at (%v,%v), want the geocoded point", g.enrichLat, g.enrichLon)
	}
}

// Garbage or null-island coordinates must not be trusted -- fall back rather than
// confidently enriching the Gulf of Guinea.
func TestLandSignalsRejectBadCoordinates(t *testing.T) {
	for _, bad := range []map[string]string{
		{"lat": "0", "lon": "0"},
		{"lat": "abc", "lon": "-122.2"},
		{"lat": "48.5"},
		{"lat": "91.2", "lon": "-122.2"},
		{"lat": "48.5", "lon": "-999"},
	} {
		g := &countingGeo{geocodeLat: 47.6, geocodeLon: -122.3}
		attrs := map[string]string{"acreage": "3", "location": "Seattle, WA"}
		for k, v := range bad {
			attrs[k] = v
		}
		_, _ = buildLandSignals(context.Background(), landItemWith(attrs), g, nil, LandScoreConfig{MinAcreageAcres: 1})
		if g.geocodeCalls != 1 {
			t.Errorf("coords %v: Geocode called %d times, want 1 (bad coords must fall back)", bad, g.geocodeCalls)
		}
		if g.enrichLat == 0 && g.enrichLon == 0 && g.enrichCalls > 0 {
			t.Errorf("coords %v: enriched at null island", bad)
		}
	}
}

// A parcel provider needs a STREET ADDRESS. Passing it a city label is a lookup
// that cannot succeed, so the street address must be preferred when present.
func TestParcelLookupPrefersStreetAddress(t *testing.T) {
	p := &recordingParcel{}
	it := landItemWith(map[string]string{
		"acreage":        "5",
		"location":       "Sedro Woolley, WA",
		"street_address": "0 262XX Helmick Road, Sedro Woolley, WA 98284",
	})
	_, _ = buildLandSignals(context.Background(), it, nil, p, LandScoreConfig{MinAcreageAcres: 1})
	if len(p.args) != 1 {
		t.Fatalf("parcel Lookup called %d times, want 1", len(p.args))
	}
	if !strings.Contains(p.args[0], "Helmick") {
		t.Errorf("parcel looked up %q, want the street address", p.args[0])
	}
}

func TestParcelLookupFallsBackToLocality(t *testing.T) {
	p := &recordingParcel{}
	it := landItemWith(map[string]string{"acreage": "5", "location": "Sedro Woolley, WA"})
	_, _ = buildLandSignals(context.Background(), it, nil, p, LandScoreConfig{MinAcreageAcres: 1})
	if len(p.args) != 1 || p.args[0] != "Sedro Woolley, WA" {
		t.Fatalf("parcel args = %v, want the locality fallback", p.args)
	}
}

// Coordinates alone are enough to enrich, even with no place label at all.
func TestCoordinatesAloneEnableEnrichment(t *testing.T) {
	g := &countingGeo{}
	it := landItemWith(map[string]string{"acreage": "5", "lat": "48.5", "lon": "-122.2"})
	_, enriched := buildLandSignals(context.Background(), it, g, nil, LandScoreConfig{MinAcreageAcres: 1})
	if !enriched {
		t.Fatal("coordinates alone must enable enrichment")
	}
	if g.geocodeCalls != 0 {
		t.Errorf("Geocode called %d times, want 0", g.geocodeCalls)
	}
}
