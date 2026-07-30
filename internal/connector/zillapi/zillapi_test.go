package zillapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

const fixture = "testdata/search_lots.json"

func testConfig(c Config) Config {
	if c.Name == "" {
		c.Name = "north-sound"
	}
	if c.BBox == (BBox{}) {
		c.BBox = BBox{West: -123.141452, South: 48.055894, East: -121.868411, North: 48.673576}
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Unix(1_750_000_000, 0).UTC() }
	}
	return c
}

// --- identity -----------------------------------------------------------------

func TestSourceIDIsPerSource(t *testing.T) {
	c := NewConnector(testConfig(Config{Name: "north-sound", FixturePath: fixture}))
	if got, want := c.SourceID(), "zillapi:north-sound"; got != want {
		t.Fatalf("SourceID() = %q, want %q", got, want)
	}
	// Without a Name the bare constant identity is used (single-source/back-compat).
	bare := NewConnector(testConfig(Config{Name: " ", FixturePath: fixture}))
	bare.cfg.Name = ""
	if got, want := bare.SourceID(), "zillapi"; got != want {
		t.Fatalf("bare SourceID() = %q, want %q", got, want)
	}
}

// --- fixture mapping ----------------------------------------------------------

func TestFetchFixtureMapsRows(t *testing.T) {
	c := NewConnector(testConfig(Config{FixturePath: fixture}))
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The fixture has 5 rows; the last has no zpid and must be skipped (no
	// source-native key => no provenance => cannot form a valid Raw).
	if len(raws) != 4 {
		t.Fatalf("len(raws) = %d, want 4 (5th row has no zpid and must be skipped)", len(raws))
	}

	r := raws[0]
	if r.SourceID != "zillapi:north-sound" {
		t.Errorf("SourceID = %q", r.SourceID)
	}
	if r.SourceKey != "2078311111" {
		t.Errorf("SourceKey = %q, want the zpid", r.SourceKey)
	}
	if r.PriceCents != 14_900_000 {
		t.Errorf("PriceCents = %d, want 14900000 ($149,000)", r.PriceCents)
	}
	if r.Currency != "USD" {
		t.Errorf("Currency = %q", r.Currency)
	}
	if !strings.Contains(r.Title, "Cascade View") {
		t.Errorf("Title = %q, want the upstream address", r.Title)
	}
}

// A relative detailUrl must be absolutized; an already-absolute one left alone.
func TestFetchAbsolutizesDetailURL(t *testing.T) {
	raws := mustFetch(t, Config{FixturePath: fixture})
	if got, want := raws[0].SourceURL, "https://www.zillow.com/homedetails/1234-Cascade-View-Rd-Sedro-Woolley-WA-98284/2078311111_zpid/"; got != want {
		t.Errorf("relative detailUrl -> %q, want %q", got, want)
	}
	if got, want := raws[1].SourceURL, "https://www.zillow.com/homedetails/5678-Samish-Way-Bellingham-WA-98229/2078322222_zpid/"; got != want {
		t.Errorf("absolute detailUrl -> %q, want it unchanged", got)
	}
}

// Lot area is carried as a TYPED aspect in acres, converting from whatever unit
// upstream used. This is the structured path the land extractor consumes; the
// connector does not compose prose for a regex to re-parse.
func TestFetchCarriesAcreageAspect(t *testing.T) {
	raws := mustFetch(t, Config{FixturePath: fixture})
	cases := []struct {
		idx  int
		want string
		note string
	}{
		{0, "5.19", "lotAreaValue already in acres"},
		{1, "2", "87120 sqft converts to 2 acres"},
		{3, "2.5", "fractional acres preserved"},
	}
	for _, tc := range cases {
		if got := raws[tc.idx].Aspects["acreage"]; got != tc.want {
			t.Errorf("raws[%d].Aspects[acreage] = %q, want %q (%s)", tc.idx, got, tc.want, tc.note)
		}
	}
	// Row 2 has no lotAreaValue at all: the aspect must be ABSENT, not "0".
	// Unknown acreage is not zero acreage -- the hard-filter must be able to
	// tell them apart.
	if v, ok := raws[2].Aspects["acreage"]; ok {
		t.Errorf("row with no lotAreaValue has Aspects[acreage] = %q, want absent", v)
	}
}

// Unpriced land is common and must still be emitted (PriceCents 0 == unknown).
func TestFetchEmitsUnpricedRow(t *testing.T) {
	raws := mustFetch(t, Config{FixturePath: fixture})
	if raws[3].PriceCents != 0 {
		t.Errorf("unpriced row PriceCents = %d, want 0 (unknown)", raws[3].PriceCents)
	}
	if raws[3].SourceKey == "" {
		t.Error("unpriced row must still be emitted with its key")
	}
}

func TestFetchStampsSeenAt(t *testing.T) {
	now := time.Unix(1_750_000_000, 0).UTC()
	raws := mustFetch(t, Config{FixturePath: fixture, Now: func() time.Time { return now }})
	for i, r := range raws {
		if !r.SeenAt.Equal(now) {
			t.Errorf("raws[%d].SeenAt = %v, want %v", i, r.SeenAt, now)
		}
	}
}

// --- request construction -----------------------------------------------------

func TestFetchRequestShape(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"count":0},"request_id":"r1"}`))
	}))
	defer srv.Close()

	c := NewConnector(testConfig(Config{
		APIKey:       "zk_test_key",
		BaseURL:      srv.URL,
		MinLotAcres:  1,
		MaxPriceUSD:  200000,
		DaysOnZillow: "7",
		MaxItems:     25,
	}))
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/search" {
		t.Errorf("path = %s, want /v1/search", gotPath)
	}
	if gotAuth != "Bearer zk_test_key" {
		t.Errorf("Authorization = %q, want bearer key", gotAuth)
	}

	filters, _ := body["filters"].(map[string]any)
	if filters == nil {
		t.Fatalf("no filters in body: %v", body)
	}
	if filters["status"] != "for_sale" {
		t.Errorf("status = %v, want for_sale", filters["status"])
	}
	// bbox is REQUIRED upstream: a free-text location alone is a 400.
	bbox, _ := filters["bbox"].(map[string]any)
	if bbox == nil || bbox["west"] == nil || bbox["south"] == nil || bbox["east"] == nil || bbox["north"] == nil {
		t.Fatalf("bbox must carry all four edges, got %v", filters["bbox"])
	}
	ht, _ := filters["homeTypes"].([]any)
	if len(ht) != 1 || ht[0] != "lot" {
		t.Errorf("homeTypes = %v, want [lot]", filters["homeTypes"])
	}
	// Acreage floor is pushed UPSTREAM as square feet (cost control, not just
	// filtering): 1 acre -> 43560 sqft.
	lot, _ := filters["lotSize"].(map[string]any)
	if lot == nil || lot["min"] != float64(SqftPerAcre) {
		t.Errorf("lotSize = %v, want min %d sqft (1 acre)", filters["lotSize"], SqftPerAcre)
	}
	price, _ := filters["price"].(map[string]any)
	if price == nil || price["max"] != float64(200000) {
		t.Errorf("price = %v, want max 200000 dollars", filters["price"])
	}
	if filters["daysOnZillow"] != "7" {
		t.Errorf("daysOnZillow = %v, want 7", filters["daysOnZillow"])
	}
	if body["maxItems"] != float64(25) {
		t.Errorf("maxItems = %v, want 25", body["maxItems"])
	}
	if body["async"] == true {
		t.Error("async must be false: v1 stays synchronous")
	}
}

// maxItems above the sync ceiling is clamped rather than silently triggering an
// async job the connector cannot consume.
func TestMaxItemsClampedToSyncCeiling(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = w.Write([]byte(`{"data":[],"meta":{"count":0}}`))
	}))
	defer srv.Close()
	c := NewConnector(testConfig(Config{APIKey: "k", BaseURL: srv.URL, MaxItems: 500}))
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if body["maxItems"] != float64(MaxSyncItems) {
		t.Errorf("maxItems = %v, want clamped to %d", body["maxItems"], MaxSyncItems)
	}
}

func TestMissingAPIKeyIsConfigError(t *testing.T) {
	c := NewConnector(testConfig(Config{BaseURL: "http://127.0.0.1:1"}))
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("want an error when no API key is configured")
	}
	if !strings.Contains(err.Error(), "api key") {
		t.Errorf("err = %v, want it to name the missing api key", err)
	}
}

// --- error mapping ------------------------------------------------------------

func TestFetchMapsErrorEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantSubstr string
		wantRate   bool
	}{
		{
			name:       "invalid_filters",
			status:     400,
			body:       `{"error":{"code":"invalid_filters","message":"bbox required","request_id":"r9"}}`,
			wantSubstr: "invalid_filters",
		},
		{
			name:       "unauthorized",
			status:     401,
			body:       `{"error":{"code":"unauthorized","message":"bad key","request_id":"r8"}}`,
			wantSubstr: "unauthorized",
		},
		{
			name:       "rate limited",
			status:     429,
			body:       `{"error":{"code":"rate_limited","message":"slow down","request_id":"r7"}}`,
			wantSubstr: "rate_limited",
			wantRate:   true,
		},
		{
			name:       "non-envelope body still surfaces status",
			status:     500,
			body:       `upstream exploded`,
			wantSubstr: "500",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewConnector(testConfig(Config{APIKey: "k", BaseURL: srv.URL}))
			_, err := c.Fetch(context.Background())
			if err == nil {
				t.Fatalf("want an error for status %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantSubstr)
			}
			if tc.wantRate && !errors.Is(err, ErrRateLimited) {
				t.Errorf("429 must be identifiable as ErrRateLimited, got %v", err)
			}
		})
	}
}

// A 202 means the request went async and results arrive via a job. v1 does not
// implement job polling, so this must be an explicit error -- never silently
// treated as "no listings", which would look like a healthy empty poll.
func TestAsyncResponseIsExplicitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"job_id":"job_1","status":"queued"}}`))
	}))
	defer srv.Close()
	c := NewConnector(testConfig(Config{APIKey: "k", BaseURL: srv.URL}))
	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("a 202 async response must be an error, not an empty success")
	}
	if !errors.Is(err, ErrAsyncUnsupported) {
		t.Errorf("err = %v, want ErrAsyncUnsupported", err)
	}
}

func TestEmptyDataIsEmptySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"meta":{"count":0},"request_id":"r0"}`))
	}))
	defer srv.Close()
	c := NewConnector(testConfig(Config{APIKey: "k", BaseURL: srv.URL}))
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("an empty result set is a healthy poll, got error: %v", err)
	}
	if len(raws) != 0 {
		t.Fatalf("len(raws) = %d, want 0", len(raws))
	}
}

func TestMalformedJSONIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[ this is not json`))
	}))
	defer srv.Close()
	c := NewConnector(testConfig(Config{APIKey: "k", BaseURL: srv.URL}))
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("malformed JSON must be an error")
	}
}

// --- helpers ------------------------------------------------------------------

func mustFetch(t *testing.T, c Config) []listing.Raw {
	t.Helper()
	conn := NewConnector(testConfig(c))
	raws, err := conn.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return raws
}

// --- real captured response ---------------------------------------------------

// search_lots_live.json is a REAL Zillapi response captured 2026-07-30 (5 credits,
// bbox = North Sound, homeTypes=[lot], lotSize.min=43560, price.max=200000). It
// exists so the mapping is proven against genuine upstream output -- which carries
// many fields the published OpenAPI schema does not declare -- rather than only
// against a fixture we wrote ourselves.
func TestFetchRealCapturedResponse(t *testing.T) {
	raws := mustFetch(t, Config{FixturePath: "testdata/search_lots_live.json"})
	if len(raws) != 5 {
		t.Fatalf("len(raws) = %d, want 5", len(raws))
	}

	// Every row in the real capture is a priced lot with a known acreage above the
	// 1-acre floor we asked for -- which is also the proof that lotSize.min is in
	// SQUARE FEET: had 43560 been read as acres, the filter would have demanded
	// ~68 square miles and returned nothing.
	for i, r := range raws {
		if r.SourceKey == "" {
			t.Errorf("raws[%d] has no SourceKey", i)
		}
		if !strings.HasPrefix(r.SourceURL, "https://www.zillow.com/") {
			t.Errorf("raws[%d].SourceURL = %q, want an absolute zillow URL", i, r.SourceURL)
		}
		if r.PriceCents <= 0 {
			t.Errorf("raws[%d].PriceCents = %d, want > 0", i, r.PriceCents)
		}
		acr, ok := r.Aspects["acreage"]
		if !ok {
			t.Errorf("raws[%d] has no acreage aspect", i)
			continue
		}
		v, err := strconv.ParseFloat(acr, 64)
		if err != nil || v < 1 {
			t.Errorf("raws[%d].acreage = %q, want a number >= 1 (the requested floor)", i, acr)
		}
	}

	// Spot-check the first row against the captured values.
	if raws[0].SourceKey != "456629829" {
		t.Errorf("raws[0].SourceKey = %q, want 456629829", raws[0].SourceKey)
	}
	if raws[0].PriceCents != 16_000_000 {
		t.Errorf("raws[0].PriceCents = %d, want 16000000 ($160,000)", raws[0].PriceCents)
	}
	if got, want := raws[0].Aspects["acreage"], "6.129"; got != want {
		t.Errorf("raws[0].acreage = %q, want %q (top-level lotArea.value, in acres)", got, want)
	}
	if got, want := raws[0].Aspects["location"], "Sedro Woolley, WA"; got != want {
		t.Errorf("raws[0].location = %q, want %q", got, want)
	}
}

// taxAssessedValue is undeclared upstream and present on only SOME rows, so it
// must be carried when there and absent (not zero) when not.
func TestRealCaptureAssessedValueIsOptional(t *testing.T) {
	raws := mustFetch(t, Config{FixturePath: "testdata/search_lots_live.json"})
	if got, want := raws[1].Aspects["assessed_value_cents"], "4610000"; got != want {
		t.Errorf("raws[1].assessed_value_cents = %q, want %q ($46,100)", got, want)
	}
	if v, ok := raws[0].Aspects["assessed_value_cents"]; ok {
		t.Errorf("raws[0] has assessed_value_cents = %q, want absent (upstream omitted it)", v)
	}
}
