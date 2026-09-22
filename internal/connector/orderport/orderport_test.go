package orderport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// testdata/syncline_all_wines.html is real product cards from
// synclinewine.orderport.net/wines/All-Wines (2026-09-21): a Syrah on sale
// ($58.50, was $65.00), a full-price Chardonnay, a Mourvedre (accented name),
// a gift card, and a copy of the Chardonnay card edited to be sold out.
const fixture = "testdata/syncline_all_wines.html"

func fetch(t *testing.T) map[string]struct {
	price   int64
	aspects map[string]string
	body    string
	key     string
	url     string
} {
	t.Helper()
	c := NewConnector(Config{Name: "syncline", FixturePath: fixture})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !c.FetchComplete() {
		t.Fatal("a parsed page is a complete fetch")
	}
	out := map[string]struct {
		price   int64
		aspects map[string]string
		body    string
		key     string
		url     string
	}{}
	for _, r := range raws {
		if r.SourceID != "orderport:syncline" {
			t.Fatalf("source id %q", r.SourceID)
		}
		out[r.Title] = struct {
			price   int64
			aspects map[string]string
			body    string
			key     string
			url     string
		}{r.PriceCents, r.Aspects, r.Body, r.SourceKey, r.SourceURL}
	}
	return out
}

func TestParsesCardsPricesAndSales(t *testing.T) {
	got := fetch(t)
	syrah, ok := got["2021 Syrah, Estate Grown"]
	if !ok {
		t.Fatalf("Syrah missing; got %v", keys(got))
	}
	if syrah.price != 5850 || syrah.aspects["compare_at_cents"] != "6500" {
		t.Fatalf("Syrah price %d compare %q, want 5850 and 6500", syrah.price, syrah.aspects["compare_at_cents"])
	}
	if !strings.Contains(syrah.body, "94 points, Decanter") || syrah.key != "0454" ||
		!strings.Contains(syrah.url, "/product-details/0454/") {
		t.Fatalf("Syrah body/key/url: key=%q url=%q body=%.80q", syrah.key, syrah.url, syrah.body)
	}
	chard := got["2023 Chardonnay, Oak Ridge Vineyard"]
	if chard.price != 5000 || chard.aspects["compare_at_cents"] != "" {
		t.Fatalf("full-price Chardonnay: %+v", chard)
	}
	var accented bool
	for title := range got {
		if strings.HasPrefix(title, "2024 Mourv\u00e8dre") {
			accented = true
		}
	}
	if !accented {
		t.Fatalf("HTML entities not decoded in titles: %v", keys(got))
	}
}

func TestSkipsSoldOutKeepsGiftCardForTheExtractor(t *testing.T) {
	got := fetch(t)
	if _, sold := got["2022 Chardonnay, Sold Out Test"]; sold {
		t.Fatal("a card without add-to-cart must be skipped")
	}
	// Gift cards are the wine extractor's job to reject (no wine evidence), so
	// the connector passes them through; 4 of 5 cards survive.
	if len(got) != 4 {
		t.Fatalf("got %d products, want 4: %v", len(got), keys(got))
	}
}

func TestRedesignedPageIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>new shop coming soon</body></html>"))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "x", StoreURL: srv.URL})
	if _, err := c.Fetch(context.Background()); err == nil || c.FetchComplete() {
		t.Fatal("a page with no product cards must be an error")
	}
}

func TestFetchOverHTTP(t *testing.T) {
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DefaultCatalogPath {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	raws, err := NewConnector(Config{Name: "syncline", StoreURL: srv.URL}).Fetch(context.Background())
	if err != nil || len(raws) != 4 {
		t.Fatalf("http fetch: %d %v", len(raws), err)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Hedges (2026-09-21): All-Wines carries no cards; current-releases carries
// them all. The connector follows the store's own listing link.
func TestFallsBackToTheStoresListingLink(t *testing.T) {
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DefaultCatalogPath:
			_, _ = w.Write([]byte(`<a href="/wines">Wines</a> <a href="https://x.orderport.net/wines/current-releases">Current</a>`))
		case "/wines/current-releases":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	raws, err := NewConnector(Config{Name: "hedges", StoreURL: srv.URL}).Fetch(context.Background())
	if err != nil || len(raws) != 4 {
		t.Fatalf("fallback fetch: %d %v", len(raws), err)
	}
}
