package shopify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A configured collection walks /collections/<handle>/products.json: the
// whole category in a few pages instead of a mixed catalogue of 40+ (the
// serverpartdeals shape, nagus-bu2). The page parameters and the allow-filter
// are unchanged.
func TestCollectionWalksTheCollectionFeed(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"products":[{"id":1,"title":"D","handle":"d","product_type":"Hard Drives > 8TB > 3.5","variants":[{"id":2,"price":"10.00","available":true}]},{"id":3,"title":"S","handle":"s","product_type":"SSDs > 1TB","variants":[{"id":4,"price":"10.00","available":true}]}]}`))
	}))
	defer srv.Close()
	c := NewConnector(Config{
		Name: "s", BaseURL: srv.URL, Collection: "all-hard-drives", ProductTypePrefixes: []string{"Hard Drives"},
		Now: func() time.Time { return fixedNow },
	})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/collections/all-hard-drives/products.json?limit=250&page=1" {
		t.Fatalf("requested %v, want the collection feed, page 1", paths)
	}
	if len(raws) != 1 || raws[0].SourceKey != "1:2" || !c.FetchComplete() {
		t.Fatalf("raws=%v complete=%v, want the one hard drive and a complete walk", raws, c.FetchComplete())
	}
	// SourceKeys and product URLs do not depend on the collection, so moving
	// a store onto its collection keeps every stored offer's identity.
	if raws[0].SourceURL != srv.URL+"/products/d" {
		t.Errorf("SourceURL = %q, want the product URL independent of the collection", raws[0].SourceURL)
	}
}

// The handle is spliced into a URL path, so anything but a Shopify slug is
// refused before a request is made.
func TestCollectionHandleIsValidated(t *testing.T) {
	for _, h := range []string{"all-hard-drives", "hdd2", "a"} {
		if !ValidCollectionHandle(h) {
			t.Errorf("%q should be a valid handle", h)
		}
	}
	for _, h := range []string{"../admin", "all hard drives", "All-Hard-Drives", "a/b", "x?page=9", "-lead", ""} {
		if ValidCollectionHandle(h) {
			t.Errorf("%q must be rejected", h)
		}
		if h == "" {
			continue // empty means "no collection", which is valid config
		}
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Errorf("made a request for invalid handle %q", h)
		}))
		c := NewConnector(Config{Name: "s", BaseURL: srv.URL, Collection: h, Now: func() time.Time { return fixedNow }})
		if _, err := c.Fetch(context.Background()); err == nil {
			t.Errorf("Fetch with collection %q: want an error", h)
		}
		srv.Close()
	}
}
