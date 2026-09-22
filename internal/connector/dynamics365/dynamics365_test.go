package dynamics365

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testdata/stemichelle_all_wines.html is real products from Chateau Ste
// Michelle's all-wines category (2026-09-21), trimmed to the embedded list
// state and product links: two full-price bottles, a 3-bottle gift box (an
// "Assembly") and a Cabernet Franc whose BasePrice was edited up to put it on
// sale (the live page had no single-bottle sale that day).
const fixture = "testdata/stemichelle_all_wines.html"

func TestParsesProductsAttributesAndSales(t *testing.T) {
	c := NewConnector(Config{Name: "chateau-ste-michelle", StoreURL: "https://www.ste-michelle.com", FixturePath: fixture})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !c.FetchComplete() {
		t.Fatal("a parsed page is a complete fetch")
	}
	byTitle := map[string]int{}
	for i, r := range raws {
		byTitle[r.Title] = i
		if r.SourceID != "dynamics365:chateau-ste-michelle" {
			t.Fatalf("source id %q", r.SourceID)
		}
	}
	if len(raws) != 3 {
		t.Fatalf("got %d, want 3 (the gift box is skipped): %v", len(raws), byTitle)
	}
	i, ok := byTitle["2024 Evergreen Vineyard Riesling"]
	if !ok {
		t.Fatalf("Riesling missing: %v", byTitle)
	}
	r := raws[i]
	a := r.Aspects
	if r.PriceCents != 3000 || a["vintage"] != "2024" || a["varietal"] != "Riesling" || a["wine_type"] != "White" ||
		a["appellation"] != "Columbia Valley" || a["bottle_ml"] != "750" || a["compare_at_cents"] != "" {
		t.Fatalf("Riesling: price %d aspects %v", r.PriceCents, a)
	}
	if !strings.HasPrefix(r.SourceURL, "https://www.ste-michelle.com/chateau-ste-michelle/shop/") || !strings.HasSuffix(r.SourceURL, ".p") {
		t.Fatalf("link %q", r.SourceURL)
	}
	if !strings.Contains(r.Body, "Riesling") {
		t.Fatalf("tasting notes missing from body: %q", r.Body)
	}
	sale := raws[byTitle["2021 Limited Release Cabernet Franc"]]
	if sale.PriceCents != 4500 || sale.Aspects["compare_at_cents"] != "5625" {
		t.Fatalf("sale: price %d compare %q", sale.PriceCents, sale.Aspects["compare_at_cents"])
	}
}

// page renders a category page holding products [from, to) of total.
func page(from, to, total int) string {
	var ps []map[string]any
	for i := from; i < to; i++ {
		ps = append(ps, map[string]any{"ItemId": fmt.Sprint(i), "Name": fmt.Sprintf("2020 Wine %d", i), "Price": 20, "BasePrice": 20, "RecordId": 1000 + i})
	}
	b, _ := json.Marshal(map[string]any{"LISTPAGESTATE": map[string]any{"LISTPAGESTATE": map[string]any{"item": map[string]any{"result": map[string]any{"totalProductCount": total, "activeProducts": ps}}}}})
	return "<script>window.___initialData___ = " + string(b) + ";</script>"
}

func TestPagesWithSkipAndPauses(t *testing.T) {
	var skips []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		skips = append(skips, r.URL.Query().Get("skip"))
		switch r.URL.Query().Get("skip") {
		case "":
			_, _ = w.Write([]byte(page(0, 50, 60)))
		case "50":
			_, _ = w.Write([]byte(page(50, 60, 60)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var slept []time.Duration
	c := NewConnector(Config{Name: "x", StoreURL: srv.URL, CatalogPath: "/shop/all-wines/1.c",
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }})
	raws, err := c.Fetch(context.Background())
	if err != nil || len(raws) != 60 || !c.FetchComplete() {
		t.Fatalf("got %d %v complete=%v", len(raws), err, c.FetchComplete())
	}
	if strings.Join(skips, ",") != ",50" {
		t.Fatalf("skips %v", skips)
	}
	if len(slept) != 1 || slept[0] != DefaultPause {
		t.Fatalf("crawl delay not honored between pages: %v", slept)
	}
}

func TestStoppingShortIsNotComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page(0, 50, 500)))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "x", StoreURL: srv.URL, CatalogPath: "/c", MaxPages: 2, Pause: -1})
	raws, err := c.Fetch(context.Background())
	if err != nil || len(raws) != 100 {
		t.Fatalf("got %d %v", len(raws), err)
	}
	if c.FetchComplete() {
		t.Fatal("a fetch capped by maxPages before the store's count must not be complete")
	}
}

func TestRedesignedPageIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>new shop</html>"))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "x", StoreURL: srv.URL, CatalogPath: "/c", Pause: -1})
	if _, err := c.Fetch(context.Background()); err == nil || c.FetchComplete() {
		t.Fatal("a page without list state must be an error")
	}
}

func TestBottleML(t *testing.T) {
	for in, want := range map[string]int{"750 ml": 750, "1.5 L": 1500, "375ML": 375, "": 0, "Magnum": 0} {
		if got := bottleML(in); got != want {
			t.Errorf("bottleML(%q) = %d, want %d", in, got, want)
		}
	}
}
