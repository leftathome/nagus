package vinoshipper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// testdata/kiona_wine_list.json is Kiona Vineyards' real feed (account 1106,
// 2026-09-21) trimmed to 8 wines, five of them edited to exercise each rule:
// a store sale (msrp 48 > price 40), hidden, a CIDER, sold out, and a wine
// barred from WA.
const fixture = "testdata/kiona_wine_list.json"

func byTitle(raws []rawLike) map[string]rawLike {
	m := map[string]rawLike{}
	for _, r := range raws {
		m[r.Title] = r
	}
	return m
}

type rawLike struct {
	Title, URL string
	Price      int64
	Aspects    map[string]string
}

func fetchFixture(t *testing.T) []rawLike {
	t.Helper()
	c := NewConnector(Config{Name: "kiona", FixturePath: fixture})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !c.FetchComplete() {
		t.Fatal("a decoded feed is a complete fetch")
	}
	out := make([]rawLike, 0, len(raws))
	for _, r := range raws {
		if r.SourceID != "vinoshipper:kiona" || r.SourceKey == "" || r.Currency != "USD" {
			t.Fatalf("identity fields: %+v", r)
		}
		out = append(out, rawLike{r.Title, r.SourceURL, r.PriceCents, r.Aspects})
	}
	return out
}

func TestFetchKeepsPurchasableWinesOnly(t *testing.T) {
	raws := fetchFixture(t)
	if len(raws) != 5 {
		t.Fatalf("got %d wines, want 5 (8 minus hidden, cider, sold out)", len(raws))
	}
	for _, r := range raws {
		if strings.Contains(r.Title, "Cider") {
			t.Fatalf("a CIDER passed as wine: %q", r.Title)
		}
	}
}

func TestFetchMapsStructuredFields(t *testing.T) {
	raws := fetchFixture(t)
	first := raws[0]
	if first.Aspects["vendor"] == "" || first.Aspects["varietal"] != "Cabernet Sauvignon" ||
		first.Aspects["bottle_ml"] != "750" || first.Aspects["wine_type"] != "red" || first.Price != 11000 {
		t.Fatalf("mapped %+v", first)
	}
	if !strings.HasPrefix(first.URL, "https://vinoshipper.com/shop/") {
		t.Fatalf("url %q", first.URL)
	}
	if _, has := first.Aspects["compare_at_cents"]; has {
		t.Fatal("msrp == price is not a sale")
	}
	sale := byTitle(raws)
	var found bool
	for _, r := range sale {
		if r.Aspects["compare_at_cents"] == "4800" && r.Price == 4000 {
			found = true
		}
	}
	if !found {
		t.Fatal("the msrp 48 / price 40 wine must carry compare_at_cents 4800")
	}
}

func TestShipsToHonoursPerWineBars(t *testing.T) {
	var barred, open rawLike
	for _, r := range fetchFixture(t) {
		if strings.Contains(r.Aspects["ships_to"], "US-WA") {
			open = r
		} else {
			barred = r
		}
	}
	if open.Title == "" || barred.Title == "" {
		t.Fatalf("want one wine barred from WA and others shipping there; open=%q barred=%q", open.Title, barred.Title)
	}
	if !strings.Contains(barred.Aspects["ships_to"], "US-CA") {
		t.Fatalf("the barred wine lost its other states: %q", barred.Aspects["ships_to"])
	}
}

func TestFetchOverHTTPAndShapeChange(t *testing.T) {
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json-api/v2/wine-list" || r.URL.Query().Get("id") != "1106" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	raws, err := NewConnector(Config{Name: "kiona", Account: 1106, BaseURL: srv.URL}).Fetch(context.Background())
	if err != nil || len(raws) != 5 {
		t.Fatalf("http fetch: %d %v", len(raws), err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"products":[]}`))
	}))
	defer bad.Close()
	c := NewConnector(Config{Name: "kiona", Account: 1106, BaseURL: bad.URL})
	if _, err := c.Fetch(context.Background()); err == nil || c.FetchComplete() {
		t.Fatal("a feed without a wines array must be a loud error, not an empty catalogue")
	}
}
