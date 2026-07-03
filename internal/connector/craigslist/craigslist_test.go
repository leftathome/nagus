package craigslist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// testdataBytes reads testdata/search_reo.rss, the fixture that doubles as
// both the offline fixture-mode input and the canned httptest search body.
func testdataBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/search_reo.rss")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return data
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
}

// searchOKHandler asserts the request path and a non-empty User-Agent header
// (Craigslist 403s empty-UA requests, so Fetch must always send one), then
// serves body as the RSS response.
func searchOKHandler(t *testing.T, body []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/reo" {
			t.Errorf("request path = %q, want /search/reo", r.URL.Path)
		}
		if got := r.URL.Query().Get("format"); got != "rss" {
			t.Errorf("format query param = %q, want rss", got)
		}
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("request User-Agent header is empty, want non-empty")
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

func TestFetch_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/reo", searchOKHandler(t, testdataBytes(t)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		City:       "sfbay",
		Category:   "reo",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Now:        fixedNow,
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("len(raws) = %d, want 3", len(raws))
	}

	first := raws[0]
	if first.SourceID != "craigslist" {
		t.Errorf("SourceID = %q, want craigslist", first.SourceID)
	}
	if first.SourceKey != "1234567890" {
		t.Errorf("SourceKey = %q, want 1234567890", first.SourceKey)
	}
	if first.SourceURL != "https://sfbay.craigslist.org/nby/reo/d/santa-rosa/1234567890.html" {
		t.Errorf("SourceURL = %q, unexpected", first.SourceURL)
	}
	if first.Title != "5 Acres with Well & Septic - $45,000 (Sonoma County)" {
		t.Errorf("Title = %q, unexpected", first.Title)
	}
	if first.Body != "<p>Beautiful parcel with mature oaks, seasonal creek, county-approved well and septic already in.</p>" {
		t.Errorf("Body = %q, unexpected", first.Body)
	}
	if first.PriceCents != 4500000 {
		t.Errorf("PriceCents = %d, want 4500000 (from $45,000)", first.PriceCents)
	}
	if first.Currency != "USD" {
		t.Errorf("Currency = %q, want USD", first.Currency)
	}
	if first.ConditionRaw != "" {
		t.Errorf("ConditionRaw = %q, want empty (craigslist has no condition)", first.ConditionRaw)
	}
	if first.Aspects["location"] != "Sonoma County" {
		t.Errorf(`Aspects["location"] = %q, want "Sonoma County"`, first.Aspects["location"])
	}
	wantSeenAt, err := time.Parse(time.RFC3339, "2026-06-30T12:34:56-07:00")
	if err != nil {
		t.Fatalf("test setup: parse want time: %v", err)
	}
	if !first.SeenAt.Equal(wantSeenAt) {
		t.Errorf("SeenAt = %v, want %v", first.SeenAt, wantSeenAt)
	}

	second := raws[1]
	if second.SourceKey != "1234567891" {
		t.Errorf("second.SourceKey = %q, want 1234567891", second.SourceKey)
	}
	if second.PriceCents != 0 {
		t.Errorf("second.PriceCents = %d, want 0 (unpriced)", second.PriceCents)
	}
	if second.Aspects["location"] != "East Oakland" {
		t.Errorf(`second.Aspects["location"] = %q, want "East Oakland"`, second.Aspects["location"])
	}

	third := raws[2]
	if third.SourceKey != "1234567892" {
		t.Errorf("third.SourceKey = %q, want 1234567892", third.SourceKey)
	}
	if third.PriceCents != 19999900 {
		t.Errorf("third.PriceCents = %d, want 19999900 (from $199,999.00)", third.PriceCents)
	}
	if third.Aspects["location"] != "Bayview" {
		t.Errorf(`third.Aspects["location"] = %q, want "Bayview"`, third.Aspects["location"])
	}
}

func TestFetch_FixtureMode_NoNetworkCalls(t *testing.T) {
	failIfCalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected network call in fixture mode: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(failIfCalled.Close)

	c := NewConnector(Config{
		City:        "sfbay",
		Category:    "reo",
		BaseURL:     failIfCalled.URL,
		HTTPClient:  failIfCalled.Client(),
		Now:         fixedNow,
		FixturePath: "testdata/search_reo.rss",
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("len(raws) = %d, want 3", len(raws))
	}
	if raws[0].SourceKey != "1234567890" {
		t.Errorf("SourceKey = %q, want 1234567890", raws[0].SourceKey)
	}
	if raws[0].PriceCents != 4500000 {
		t.Errorf("PriceCents = %d, want 4500000", raws[0].PriceCents)
	}
}

func TestFetch_UnpricedItem_StillEmittedWithZeroPrice(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/reo", searchOKHandler(t, testdataBytes(t)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		City:       "sfbay",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Now:        fixedNow,
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}

	found := false
	for _, r := range raws {
		if r.SourceKey == "1234567891" {
			found = true
			if r.PriceCents != 0 {
				t.Errorf("PriceCents = %d, want 0 for unpriced listing", r.PriceCents)
			}
		}
	}
	if !found {
		t.Fatal("unpriced item 1234567891 not found in results")
	}
}

func TestFetch_ItemWithNoParseableListingID_IsSkipped(t *testing.T) {
	body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<item>
<title>Unlinkable listing, must be skipped</title>
<link></link>
<description>no link or rdf:about at all</description>
<dc:date>2026-06-30T12:00:00-07:00</dc:date>
</item>
<item rdf:about="https://sfbay.craigslist.org/nby/reo/d/town/9998887770.html">
<title>Has a usable link - $1,000 (Town)</title>
<link>https://sfbay.craigslist.org/nby/reo/d/town/9998887770.html</link>
<description>kept</description>
<dc:date>2026-06-30T12:00:00-07:00</dc:date>
</item>
</rdf:RDF>`)

	mux := http.NewServeMux()
	mux.HandleFunc("/search/reo", searchOKHandler(t, body))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		City:       "sfbay",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Now:        fixedNow,
	})

	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned error: %v", err)
	}
	if len(raws) != 1 {
		t.Fatalf("len(raws) = %d, want 1 (unlinkable item skipped)", len(raws))
	}
	if raws[0].SourceKey != "9998887770" {
		t.Errorf("SourceKey = %q, want 9998887770", raws[0].SourceKey)
	}
}

func TestFetch_Non200_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/reo", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewConnector(Config{
		City:       "sfbay",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Now:        fixedNow,
	})

	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 response, got nil")
	}
}

func TestSourceID(t *testing.T) {
	c := NewConnector(Config{City: "sfbay"})
	if got := c.SourceID(); got != "craigslist" {
		t.Errorf("SourceID() = %q, want craigslist", got)
	}
}

func TestNewConnector_Defaults(t *testing.T) {
	c := NewConnector(Config{City: "sfbay"})
	if c.cfg.Category != DefaultCategory {
		t.Errorf("Category = %q, want %q", c.cfg.Category, DefaultCategory)
	}
	if c.cfg.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.cfg.BaseURL, DefaultBaseURL)
	}
	if c.cfg.UserAgent != DefaultUserAgent {
		t.Errorf("UserAgent = %q, want %q", c.cfg.UserAgent, DefaultUserAgent)
	}
	if c.cfg.HTTPClient != http.DefaultClient {
		t.Errorf("HTTPClient = %v, want http.DefaultClient", c.cfg.HTTPClient)
	}
	if c.cfg.Now == nil {
		t.Error("Now should default to a non-nil func")
	}
}

func TestFeedURL(t *testing.T) {
	c := NewConnector(Config{City: "sfbay", Category: "reo"})
	want := "https://sfbay.craigslist.org/search/reo?format=rss"
	if got := c.feedURL(); got != want {
		t.Errorf("feedURL() = %q, want %q", got, want)
	}

	c2 := NewConnector(Config{City: "sfbay", Category: "sss", BaseURL: "http://127.0.0.1:9999"})
	want2 := "http://127.0.0.1:9999/search/sss?format=rss"
	if got := c2.feedURL(); got != want2 {
		t.Errorf("feedURL() with overridden BaseURL = %q, want %q", got, want2)
	}
}

func TestParsePriceCents(t *testing.T) {
	cases := []struct {
		title string
		want  int64
	}{
		{"5 Acres with Well & Septic - $45,000 (Sonoma County)", 4500000},
		{"Rare City Lot - $199,999.00 (Bayview)", 19999900},
		{"Vacant Lot Near Downtown, Owner Financing Available (East Oakland)", 0},
		{"No dollar sign here at all", 0},
	}
	for _, c := range cases {
		if got := parsePriceCents(c.title); got != c.want {
			t.Errorf("parsePriceCents(%q) = %d, want %d", c.title, got, c.want)
		}
	}
}

func TestParseLocation(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"5 Acres with Well & Septic - $45,000 (Sonoma County)", "Sonoma County"},
		{"No location at all", ""},
		{"Two groups (A) (B)", "B"},
	}
	for _, c := range cases {
		if got := parseLocation(c.title); got != c.want {
			t.Errorf("parseLocation(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestParseSeenAt(t *testing.T) {
	now := fixedNow()

	got := parseSeenAt("2026-06-30T12:34:56-07:00", now)
	want, _ := time.Parse(time.RFC3339, "2026-06-30T12:34:56-07:00")
	if !got.Equal(want) {
		t.Errorf("parseSeenAt(valid) = %v, want %v", got, want)
	}

	if got := parseSeenAt("", now); !got.Equal(now) {
		t.Errorf("parseSeenAt(empty) = %v, want now %v", got, now)
	}

	if got := parseSeenAt("not-a-date", now); !got.Equal(now) {
		t.Errorf("parseSeenAt(garbage) = %v, want now %v", got, now)
	}
}
