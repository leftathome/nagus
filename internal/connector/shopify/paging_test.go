package shopify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// --- nagus-bu2: walk the whole catalogue, politely ---------------------------

// pagedStore serves `total` products at the requested limit per page, numbered
// from 1, so the last page is short exactly as a real storefront's is. custom,
// when it returns non-empty JSON for a product number, replaces that product.
func pagedStore(t *testing.T, total int, custom func(n int) string) (*httptest.Server, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		limit, page := 0, 0
		_, _ = fmt.Sscan(r.URL.Query().Get("limit"), &limit)
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		var parts []string
		for n := (page-1)*limit + 1; n <= page*limit && n <= total; n++ {
			if custom != nil {
				if p := custom(n); p != "" {
					parts = append(parts, p)
					continue
				}
			}
			parts = append(parts, fmt.Sprintf(`{"id":%d,"title":"D%d","handle":"d%d","product_type":"Hard Drives > 8TB > 3.5","variants":[{"id":%d,"price":"10.00","available":true}]}`, n, n, n, 1000+n))
		}
		_, _ = w.Write([]byte(`{"products":[` + strings.Join(parts, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// Every page after the first is preceded by the courtesy delay, and the first
// is not: a long walk is a trickle, not a burst.
func TestPagesArePacedWithACourtesyDelay(t *testing.T) {
	srv, hits := pagedStore(t, 5, nil) // limit 2 -> pages of 2, 2, 1
	var waited []time.Duration
	c := NewConnector(Config{
		Name: "s", BaseURL: srv.URL, Limit: 2, MaxPages: 10, Now: func() time.Time { return fixedNow },
		Sleep: func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil },
	})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if *hits != 3 || len(raws) != 5 || !c.FetchComplete() {
		t.Fatalf("hits=%d raws=%d complete=%v, want 3 pages, 5 listings, complete", *hits, len(raws), c.FetchComplete())
	}
	if len(waited) != 2 || waited[0] != DefaultPageDelay || waited[1] != DefaultPageDelay {
		t.Fatalf("waited %v, want exactly two pauses of %v (none before page 1)", waited, DefaultPageDelay)
	}
}

// A one-page store never waits.
func TestSinglePageStoreNeverWaits(t *testing.T) {
	srv, _ := pagedStore(t, 1, nil)
	c := NewConnector(Config{
		Name: "s", BaseURL: srv.URL, MaxPages: 10, Now: func() time.Time { return fixedNow },
		Sleep: func(context.Context, time.Duration) error { t.Fatal("a one-page store must not wait"); return nil },
	})
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// A configured PageDelay is honoured, and cancelling the context during the
// pause stops the walk instead of carrying on.
func TestPageDelayIsConfigurableAndCancellable(t *testing.T) {
	srv, hits := pagedStore(t, 5, nil)
	var waited []time.Duration
	c := NewConnector(Config{
		Name: "s", BaseURL: srv.URL, Limit: 2, MaxPages: 10, PageDelay: 9 * time.Second, Now: func() time.Time { return fixedNow },
		Sleep: func(_ context.Context, d time.Duration) error { waited = append(waited, d); return context.Canceled },
	})
	if _, err := c.Fetch(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled from the paced wait", err)
	}
	if *hits != 1 || len(waited) != 1 || waited[0] != 9*time.Second {
		t.Fatalf("hits=%d waited=%v, want one page then one 9s pause", *hits, waited)
	}
	if c.FetchComplete() {
		t.Fatal("an aborted walk must not report complete")
	}
}

// The truncation warning counts what the STORE returned as well as the
// listings that survived the allow-filter: serverpartdeals' "143 products
// fetched" was 3000 store products read, and the smaller number hid how far
// short of the catalogue the walk stopped.
func TestTruncationLogCountsStoreProductsAndKeptListings(t *testing.T) {
	srv, _ := pagedStore(t, 10, func(n int) string {
		if n%2 == 0 { // every other product is an SSD the allow-filter drops
			return fmt.Sprintf(`{"id":%d,"title":"S%d","handle":"s%d","product_type":"SSDs > 1TB","variants":[{"id":%d,"price":"10.00","available":true}]}`, n, n, n, 1000+n)
		}
		return ""
	})
	var logged []string
	c := NewConnector(Config{
		Name: "s", BaseURL: srv.URL, Limit: 2, MaxPages: 3, PageDelay: -1,
		ProductTypePrefixes: []string{"Hard Drives"},
		Now:                 func() time.Time { return fixedNow },
		Logf:                func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) },
	})
	if _, err := c.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "6 store products read, 3 listings kept") {
		t.Fatalf("log = %v, want it to count 6 store products read and 3 listings kept", logged)
	}
}

// THE BUG (nagus-bu2): a product past the page cap was never re-fetched, so
// after the QUARK-02 re-key its offer kept the old seller-SKU hint and the
// quark id resolved from it. With the cap above the store's page count the
// tail product is read, carries the MPN-based hint, and the walk is complete
// (so offer expiry runs too).
func TestTailProductIsRefreshedWithItsMPNWhenTheCapCoversTheStore(t *testing.T) {
	custom := func(n int) string {
		if n != 7 {
			return ""
		}
		return `{"id":7,"title":"HGST Ultrastar 7K6000 HUS726040AL4215 4TB SAS","handle":"t","product_type":"Hard Drives > 4TB > 3.5","tags":["brand:HGST"],"variants":[{"id":1007,"price":"40.00","available":true,"sku":"HUS726040AL4215-DELL_DELLG13_SR"}]}`
	}
	fetch := func(maxPages int) ([]listing.Raw, bool) {
		srv, _ := pagedStore(t, 7, custom) // limit 2 -> 4 pages, the last holding product 7
		c := NewConnector(Config{
			Name: "serverpartdeals", BaseURL: srv.URL, Limit: 2, MaxPages: maxPages, PageDelay: -1,
			BrandTag: "brand:", SKUIsMPN: true, SKUSuffixes: []string{"_SR", "_MR", "_NB"},
			Now: func() time.Time { return fixedNow },
		})
		raws, err := c.Fetch(context.Background())
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		return raws, c.FetchComplete()
	}
	find := func(raws []listing.Raw) (listing.Raw, bool) {
		for _, r := range raws {
			if r.SourceKey == "7:1007" {
				return r, true
			}
		}
		return listing.Raw{}, false
	}

	raws, complete := fetch(3) // the cap stops short of the tail
	if _, ok := find(raws); ok || complete {
		t.Fatalf("capped walk: tail found=%v complete=%v, want the tail missed and the walk incomplete", ok, complete)
	}
	raws, complete = fetch(5)
	r, ok := find(raws)
	if !ok || !complete {
		t.Fatalf("covering walk: tail found=%v complete=%v, want both", ok, complete)
	}
	if r.Aspects["mpn"] != "HUS726040AL4215" || r.Aspects["brand"] != "HGST" {
		t.Fatalf("tail hint aspects mpn=%q brand=%q, want the part number HUS726040AL4215 and HGST", r.Aspects["mpn"], r.Aspects["brand"])
	}
}
