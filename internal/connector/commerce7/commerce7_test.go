package commerce7

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// testdata/tablas_for_web.json is Tablas Creek's real product/for-web page
// (tenant tablas-creek-vineyard, 2026-09-21) cut to 7 products: a gift card, a
// library listing with no variants, three purchasable wines, one edited to be
// on sale (comparePrice above price) and one edited to be sold out.
const fixture = "testdata/tablas_for_web.json"

func TestFetchKeepsPurchasableWineVariants(t *testing.T) {
	c := NewConnector(Config{Name: "tablas", FixturePath: fixture, StoreURL: "https://tablascreek.com"})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(raws) != 4 {
		t.Fatalf("got %d listings, want 4 (3 wines + the sale; not the gift card, library listing or sold-out)", len(raws))
	}
	var sale int
	for _, r := range raws {
		if strings.Contains(strings.ToLower(r.Title), "gift") {
			t.Fatalf("gift card passed: %q", r.Title)
		}
		if r.SourceID != "commerce7:tablas" || r.SourceKey == "" || r.PriceCents <= 0 || !strings.HasPrefix(r.SourceURL, "https://tablascreek.com/product/") {
			t.Fatalf("identity/price/link: %+v", r)
		}
		if r.Aspects["vintage"] == "" || r.Aspects["appellation"] != "Paso Robles" || r.Aspects["wine_type"] == "" {
			t.Fatalf("structured wine fields missing: %v", r.Aspects)
		}
		if cmp := r.Aspects["compare_at_cents"]; cmp != "" {
			sale++
			if fmt.Sprint(r.PriceCents+1000) != cmp {
				t.Fatalf("compare_at_cents %s for price %d", cmp, r.PriceCents)
			}
		}
	}
	if sale != 1 {
		t.Fatalf("%d listings on sale, want exactly the edited one", sale)
	}
}

// Pagination reads pages until a short one, sending the tenant header on
// every request -- and only ever the public for-web route.
func TestFetchPaginatesWithTenantHeader(t *testing.T) {
	var fix struct {
		Products []json.RawMessage `json:"products"`
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fix); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/product/for-web" || r.Header.Get("tenant") != "tablas-creek-vineyard" {
			t.Errorf("unexpected request %s tenant=%q", r.URL, r.Header.Get("tenant"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Page 1 is full (PageSize products); page 2 is short.
		var prods []json.RawMessage
		if r.URL.Query().Get("page") == "1" {
			for len(prods) < PageSize {
				prods = append(prods, fix.Products[len(prods)%len(fix.Products)])
			}
		} else {
			prods = fix.Products[:2]
		}
		b, _ := json.Marshal(map[string]any{"products": prods, "total": PageSize + 2})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "tablas", Tenant: "tablas-creek-vineyard", APIBase: srv.URL, Pause: 1})
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if calls.Load() != 2 || !c.FetchComplete() {
		t.Fatalf("calls=%d complete=%v, want 2 pages and a complete walk", calls.Load(), c.FetchComplete())
	}
}

func TestFetchShapeChangeIsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	c := NewConnector(Config{Name: "x", Tenant: "t", APIBase: srv.URL})
	if _, err := c.Fetch(context.Background()); err == nil || c.FetchComplete() {
		t.Fatal("a response without products must be an error, not an empty catalogue")
	}
}
