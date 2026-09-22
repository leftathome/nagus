package fingerprint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// site serves a homepage and optional extra paths; everything else 404s.
func site(t *testing.T, home string, extra map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("request without a User-Agent: %s", r.URL)
		}
		if body, ok := extra[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(home))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func probe(t *testing.T, srv *httptest.Server) Result {
	t.Helper()
	p := &Prober{HTTP: srv.Client(), Pause: 1}
	return p.Probe(context.Background(), srv.URL)
}

func TestShopifyIsSettledByProductsJSON(t *testing.T) {
	srv := site(t, `<html><link href="//cdn.shopify.com/x.css"></html>`,
		map[string]string{"/products.json": `{"products":[{"id":1}]}`, "/robots.txt": "User-agent: *\nDisallow: /cart\nDisallow: /collections/*sort_by*\n"})
	r := probe(t, srv)
	if r.Platform != Shopify || r.Endpoint != srv.URL+"/products.json" {
		t.Fatalf("got %+v", r)
	}
	if len(r.RobotsDisallow) != 1 || r.RobotsDisallow[0] != "/collections/*sort_by*" {
		t.Fatalf("robots catalogue rules = %v", r.RobotsDisallow)
	}
}

// Taken from vinoshipper.com/shop/cascade_winery (2026-09-21): the account id
// is only in the init call.
func TestVinoshipperAccountFromInit(t *testing.T) {
	srv := site(t, `<script>window.document.addEventListener('vinoshipper:loaded', () => {
	window.Vinoshipper.init(960, { allowMultiProducer: true });</script>`, nil)
	r := probe(t, srv)
	if r.Platform != Vinoshipper || r.VinoshipperAccount != 960 ||
		r.Endpoint != "https://vinoshipper.com/json-api/v2/wine-list?id=960" {
		t.Fatalf("got %+v", r)
	}
}

func TestOrderPortHostAndCatalogue(t *testing.T) {
	srv := site(t, `<a href="https://synclinewine.orderport.net/wines/All-Wines">Shop</a>`, nil)
	r := probe(t, srv)
	if r.Platform != OrderPort || r.Endpoint != "https://synclinewine.orderport.net/wines/All-Wines" {
		t.Fatalf("got %+v", r)
	}
}

func TestCommerce7TenantFromAdminHost(t *testing.T) {
	srv := site(t, `<div id="c7-content"></div><script>var c7 = {}; // see jason-demo-site.admin.platform.commerce7.com</script>`, nil)
	r := probe(t, srv)
	if r.Platform != Commerce7 || r.Commerce7Tenant != "jason-demo-site" {
		t.Fatalf("got %+v", r)
	}
}

func TestWooCommerceStoreAPI(t *testing.T) {
	srv := site(t, `<link href="/wp-content/themes/x.css">`, map[string]string{"/wp-json/wc/store/v1/products": `[{"id":1}]`})
	r := probe(t, srv)
	if r.Platform != WooCommerce || r.Endpoint != srv.URL+"/wp-json/wc/store/v1/products" {
		t.Fatalf("got %+v", r)
	}
}

// A WordPress site embedding a Vinoshipper catalogue is a Vinoshipper source.
func TestVinoshipperOutranksWordPress(t *testing.T) {
	srv := site(t, `<link href="/wp-content/x.css"><div data-vs-list="1234"></div>`, nil)
	if r := probe(t, srv); r.Platform != Vinoshipper || r.VinoshipperAccount != 1234 {
		t.Fatalf("got %+v", r)
	}
}

func TestProductJSONLDAndUnknown(t *testing.T) {
	srv := site(t, `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Product","name":"Syrah"}</script>`, nil)
	r := probe(t, srv)
	if r.Platform != Unknown || !r.ProductJSONLD {
		t.Fatalf("got %+v", r)
	}
}

func TestBareDomainAndUnreachable(t *testing.T) {
	r := (&Prober{Pause: 1, HTTP: &http.Client{Timeout: 500 * time.Millisecond}}).Probe(context.Background(), "127.0.0.1:1")
	if r.Error == "" || r.URL != "https://127.0.0.1:1/" {
		t.Fatalf("got %+v", r)
	}
}
