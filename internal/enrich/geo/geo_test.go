package geo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
)

// newTestEnricher wires an Enricher whose four point-lookup endpoints are
// all served by mux, so each test controls every source independently
// without ever touching the real network.
func newTestEnricher(t *testing.T, mux *http.ServeMux) (*Enricher, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &Enricher{
		Client:            srv.Client(),
		CensusGeocoderURL: srv.URL + "/census",
		FEMANFHLURL:       srv.URL + "/fema",
		USGSElevationURL:  srv.URL + "/usgs",
		SSURGOURL:         srv.URL + "/ssurgo",
		NWIURL:            srv.URL + "/nwi",
	}
	return e, srv
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
}

// allSourcesOKMux returns a mux with all four point-lookup endpoints wired
// to canned successful responses, except any name listed in skip -- callers
// that want to override a source's behavior must skip it here first, since
// http.ServeMux panics on a duplicate pattern registration.
func allSourcesOKMux(t *testing.T, skip ...string) *http.ServeMux {
	mux := http.NewServeMux()
	skipped := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipped[s] = true
	}
	if !skipped["fema"] {
		mux.HandleFunc("/fema", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, arcgisQueryResponse{
				Features: []struct {
					Attributes map[string]any `json:"attributes"`
				}{
					{Attributes: map[string]any{"FLD_ZONE": "AE", "ZONE_SUBTY": "FLOODWAY"}},
				},
			})
		})
	}
	if !skipped["usgs"] {
		mux.HandleFunc("/usgs", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, map[string]any{"value": 123.45})
		})
	}
	if !skipped["ssurgo"] {
		mux.HandleFunc("/ssurgo", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, sdaResponse{
				Table: [][]string{
					{"muname", "compname", "comppct_r"},
					{"Chester silt loam", "Chester", "60"},
				},
			})
		})
	}
	if !skipped["nwi"] {
		mux.HandleFunc("/nwi", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(t, w, arcgisQueryResponse{
				Features: []struct {
					Attributes map[string]any `json:"attributes"`
				}{
					// Live NWI (2026-07) returns table-qualified attribute keys.
					{Attributes: map[string]any{"Wetlands.WETLAND_TYPE": "Freshwater Forested/Shrub Wetland", "Wetlands.ATTRIBUTE": "PFO1A"}},
				},
			})
		})
	}
	return mux
}

func TestEnrich_AllSourcesSucceed(t *testing.T) {
	mux := allSourcesOKMux(t)
	e, _ := newTestEnricher(t, mux)

	res, err := e.Enrich(context.Background(), 39.9, -77.9)
	if err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", res.Errors)
	}

	if res.Flood == nil {
		t.Fatal("expected Flood to be populated")
	} else if res.Flood.Zone != "AE" || res.Flood.ZoneSubtype != "FLOODWAY" {
		t.Errorf("Flood = %+v, want Zone=AE ZoneSubtype=FLOODWAY", res.Flood)
	}

	if res.Elevation == nil {
		t.Fatal("expected Elevation to be populated")
	} else if res.Elevation.Meters != 123.45 {
		t.Errorf("Elevation.Meters = %v, want 123.45", res.Elevation.Meters)
	}

	if res.Soil == nil {
		t.Fatal("expected Soil to be populated")
	} else if res.Soil.Series != "Chester" || res.Soil.ComponentPct != 60 {
		t.Errorf("Soil = %+v, want Series=Chester ComponentPct=60", res.Soil)
	}

	if res.Wetlands == nil {
		t.Fatal("expected Wetlands to be populated")
	} else if res.Wetlands.Type != "Freshwater Forested/Shrub Wetland" || res.Wetlands.Code != "PFO1A" {
		t.Errorf("Wetlands = %+v, want the NWI forested/shrub wetland", res.Wetlands)
	}
}

func TestEnrich_OneSourceDown_OthersStillPopulate(t *testing.T) {
	mux := allSourcesOKMux(t, "fema")
	// FEMA returns 500; everything else still succeeds.
	mux.HandleFunc("/fema", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	})
	e, _ := newTestEnricher(t, mux)

	res, err := e.Enrich(context.Background(), 39.9, -77.9)
	if err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}

	if res.Flood != nil {
		t.Errorf("expected Flood nil after upstream 500, got %+v", res.Flood)
	}
	if res.Errors["flood"] == "" {
		t.Errorf("expected Errors[flood] to be set, got %v", res.Errors)
	}

	if res.Elevation == nil {
		t.Error("expected Elevation still populated despite flood failure")
	}
	if res.Soil == nil {
		t.Error("expected Soil still populated despite flood failure")
	}
	if res.Wetlands == nil {
		t.Error("expected Wetlands still populated despite flood failure")
	}
	if _, bad := res.Errors["elevation"]; bad {
		t.Errorf("did not expect elevation error, got %v", res.Errors)
	}
}

func TestEnrich_SourceReturnsEmpty_NotAnError(t *testing.T) {
	mux := allSourcesOKMux(t, "fema")
	// FEMA returns a well-formed response with zero features -- "no
	// mapped flood zone at this point", not a failure.
	mux.HandleFunc("/fema", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, arcgisQueryResponse{})
	})
	e, _ := newTestEnricher(t, mux)

	res, err := e.Enrich(context.Background(), 39.9, -77.9)
	if err != nil {
		t.Fatalf("Enrich returned error: %v", err)
	}
	if res.Flood != nil {
		t.Errorf("expected Flood nil for empty feature set, got %+v", res.Flood)
	}
	if msg, ok := res.Errors["flood"]; ok {
		t.Errorf("empty-but-valid response must not be recorded as an error, got %q", msg)
	}
	if res.Elevation == nil || res.Soil == nil || res.Wetlands == nil {
		t.Error("expected other sources still populated")
	}
}

func TestEnrich_AllSourcesFail(t *testing.T) {
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}
	mux.HandleFunc("/fema", fail)
	mux.HandleFunc("/usgs", fail)
	mux.HandleFunc("/ssurgo", fail)
	mux.HandleFunc("/nwi", fail)
	e, _ := newTestEnricher(t, mux)

	res, err := e.Enrich(context.Background(), 39.9, -77.9)
	if err != nil {
		t.Fatalf("Enrich must not hard-fail when every upstream is down, got err: %v", err)
	}
	if res.Flood != nil || res.Elevation != nil || res.Soil != nil || res.Wetlands != nil {
		t.Errorf("expected all fields nil, got %+v", res)
	}
	for _, name := range []string{"flood", "elevation", "soil", "wetlands"} {
		if res.Errors[name] == "" {
			t.Errorf("expected Errors[%s] to be set", name)
		}
	}
}

func TestEnrich_InvalidCoordinates(t *testing.T) {
	e := NewEnricher(nil)
	cases := []struct{ lat, lon float64 }{
		{91, 0},
		{-91, 0},
		{0, 181},
		{0, -181},
	}
	for _, c := range cases {
		_, err := e.Enrich(context.Background(), c.lat, c.lon)
		if err == nil {
			t.Errorf("Enrich(%v, %v): expected error, got nil", c.lat, c.lon)
			continue
		}
		var coordErr *InvalidCoordinateError
		if !asInvalidCoordinateError(err, &coordErr) {
			t.Errorf("Enrich(%v, %v): expected InvalidCoordinateError, got %T: %v", c.lat, c.lon, err, err)
		}
	}
}

func asInvalidCoordinateError(err error, target **InvalidCoordinateError) bool {
	ice, ok := err.(*InvalidCoordinateError)
	if ok {
		*target = ice
	}
	return ok
}

func TestEnrich_ContextTimeout(t *testing.T) {
	mux := http.NewServeMux()
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
		}
		writeJSON(t, w, map[string]any{})
	}
	mux.HandleFunc("/fema", slow)
	mux.HandleFunc("/usgs", slow)
	mux.HandleFunc("/ssurgo", slow)
	mux.HandleFunc("/nwi", slow)
	e, _ := newTestEnricher(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	res, err := e.Enrich(ctx, 39.9, -77.9)
	if err != nil {
		t.Fatalf("Enrich must not hard-fail on context timeout, got err: %v", err)
	}
	if res.Flood != nil || res.Elevation != nil || res.Soil != nil || res.Wetlands != nil {
		t.Errorf("expected all fields nil after timeout, got %+v", res)
	}
	for _, name := range []string{"flood", "elevation", "soil", "wetlands"} {
		if res.Errors[name] == "" {
			t.Errorf("expected Errors[%s] to be set after timeout", name)
		}
	}
}

func TestEnrich_ContextAlreadyCanceled(t *testing.T) {
	mux := allSourcesOKMux(t)
	e, _ := newTestEnricher(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := e.Enrich(ctx, 39.9, -77.9)
	if err != nil {
		t.Fatalf("Enrich must not hard-fail on a pre-canceled context, got err: %v", err)
	}
	if len(res.Errors) != 4 {
		t.Errorf("expected all 4 sources to report an error, got %v", res.Errors)
	}
}

func TestGeocode_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/census", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("address"); got != "100 Main St, Anytown, PA" {
			t.Errorf("unexpected address query param: %q", got)
		}
		writeJSON(t, w, censusGeocodeResponse{
			Result: struct {
				AddressMatches []struct {
					Coordinates struct {
						X float64 `json:"x"`
						Y float64 `json:"y"`
					} `json:"coordinates"`
				} `json:"addressMatches"`
			}{
				AddressMatches: []struct {
					Coordinates struct {
						X float64 `json:"x"`
						Y float64 `json:"y"`
					} `json:"coordinates"`
				}{
					{Coordinates: struct {
						X float64 `json:"x"`
						Y float64 `json:"y"`
					}{X: -77.9, Y: 39.9}},
				},
			},
		})
	})
	e, _ := newTestEnricher(t, mux)

	lat, lon, err := e.Geocode(context.Background(), "100 Main St, Anytown, PA")
	if err != nil {
		t.Fatalf("Geocode returned error: %v", err)
	}
	if lat != 39.9 || lon != -77.9 {
		t.Errorf("Geocode = (%v, %v), want (39.9, -77.9)", lat, lon)
	}
}

func TestGeocode_NoMatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/census", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, censusGeocodeResponse{})
	})
	e, _ := newTestEnricher(t, mux)

	_, _, err := e.Geocode(context.Background(), "nowhere, nowhere")
	if err == nil {
		t.Fatal("expected error for no address match, got nil")
	}
	if !strings.Contains(err.Error(), "no address match") {
		t.Errorf("error = %v, want message mentioning 'no address match'", err)
	}
}

func TestGeocode_EmptyAddress(t *testing.T) {
	e := NewEnricher(nil)
	_, _, err := e.Geocode(context.Background(), "   ")
	if err == nil {
		t.Fatal("expected error for empty address, got nil")
	}
}

func TestGeocode_UpstreamDown(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/census", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	e, _ := newTestEnricher(t, mux)

	_, _, err := e.Geocode(context.Background(), "100 Main St, Anytown, PA")
	if err == nil {
		t.Fatal("expected error when census geocoder is down, got nil")
	}
}

func TestResult_ApplyTo(t *testing.T) {
	res := Result{
		Flood:     &FloodInfo{Zone: "AE", ZoneSubtype: "FLOODWAY"},
		Elevation: &ElevationInfo{Meters: 123.45},
		Soil:      &SoilInfo{Series: "Chester", MapUnitName: "Chester silt loam", ComponentPct: 60},
		Wetlands:  &WetlandsInfo{Type: "Freshwater Forested/Shrub Wetland", Code: "PFO1A"},
	}

	it := &item.Item{Category: "land", Class: item.ClassDurable, SourceID: "test", SourceKey: "1"}
	res.ApplyTo(it)

	want := map[string]string{
		AttrFloodZone:   "AE",
		AttrElevationM:  "123.45",
		AttrSoilSeries:  "Chester",
		AttrWetlandType: "Freshwater Forested/Shrub Wetland",
	}
	for k, v := range want {
		if got := it.Attributes[k]; got != v {
			t.Errorf("Attributes[%q] = %q, want %q", k, got, v)
		}
	}
	if len(it.Attributes) != len(want) {
		t.Errorf("Attributes = %v, want exactly %v", it.Attributes, want)
	}
}

func TestResult_ApplyTo_PartialResultOmitsMissingKeys(t *testing.T) {
	res := Result{
		Flood: &FloodInfo{Zone: "X"},
		// Elevation, Soil, Wetlands unavailable.
		Errors: map[string]string{"elevation": "boom", "soil": "boom", "wetlands": "boom"},
	}

	it := &item.Item{Category: "land", Class: item.ClassDurable, SourceID: "test", SourceKey: "1"}
	res.ApplyTo(it)

	if got := it.Attributes[AttrFloodZone]; got != "X" {
		t.Errorf("Attributes[flood_zone] = %q, want X", got)
	}
	for _, k := range []string{AttrElevationM, AttrSoilSeries, AttrWetlandType} {
		if _, ok := it.Attributes[k]; ok {
			t.Errorf("Attributes[%s] should be absent for unavailable source, got %q", k, it.Attributes[k])
		}
	}
}

func TestResult_ApplyTo_NilItemDoesNotPanic(t *testing.T) {
	res := Result{Flood: &FloodInfo{Zone: "X"}}
	res.ApplyTo(nil)
}

func TestResult_ApplyTo_PreservesExistingAttributes(t *testing.T) {
	res := Result{Flood: &FloodInfo{Zone: "X"}}
	it := &item.Item{
		Category:   "land",
		Class:      item.ClassDurable,
		SourceID:   "test",
		SourceKey:  "1",
		Attributes: map[string]string{"acreage": "5.2"},
	}
	res.ApplyTo(it)

	if it.Attributes["acreage"] != "5.2" {
		t.Errorf("expected pre-existing attribute preserved, got %v", it.Attributes)
	}
	if it.Attributes[AttrFloodZone] != "X" {
		t.Errorf("expected flood_zone set, got %v", it.Attributes)
	}
}

// TestEnrich_Concurrent_NoDataRace exercises Enrich many times concurrently
// against a single Enricher to give the race detector a chance to catch any
// shared-state mutation bugs across the goroutines each Enrich call spawns.
func TestEnrich_Concurrent_NoDataRace(t *testing.T) {
	mux := allSourcesOKMux(t)
	e, _ := newTestEnricher(t, mux)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Enrich(context.Background(), 39.9, -77.9); err != nil {
				t.Errorf("Enrich: %v", err)
			}
		}()
	}
	wg.Wait()
}
