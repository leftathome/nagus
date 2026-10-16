package promo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
)

// Two reads whose words agree but whose numbers do not are different
// products: a capacity is the hdd deal metric, a vintage wine identity.
func TestAgreeRequiresSameNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, a, b, drop string
	}{
		{"capacity", "Seagate IronWolf 8TB NAS Hard Drive", "Seagate IronWolf 18TB NAS Hard Drive", DropTitleDisagree},
		{"vintage", "2022 Caymus Cabernet Sauvignon", "2021 Caymus Cabernet Sauvignon", DropTitleDisagree},
		{"number only in one", "Caymus Cabernet Sauvignon 750ml", "Caymus Cabernet Sauvignon", DropTitleDisagree},
		{"spacing", "WD Red Plus 12 TB NAS Hard Drive", "WD Red Plus 12TB NAS Hard Drive", ""},
		{"same numbers", "Tolaini Al Passo Toscana 2022", "Tolaini Al Passo Toscana 2022 Red", ""},
	} {
		_, drop := Agree(read(t, product(tc.a, "10.00", "")), read(t, product(tc.b, "10.00", "")))
		if drop != tc.drop {
			t.Errorf("%s: drop %q, want %q", tc.name, drop, tc.drop)
		}
	}
}

func TestPriceShapeAndWasRatio(t *testing.T) {
	for in, ok := range map[string]bool{
		"1,299.99": true, "1299.99": true, "$1,299": true, "123,456": true,
		"1,2,9": false, "12,99": false, "1,29.99": false, ",99": false, "1.299,99": false,
	} {
		if _, got := normPrice(in); got != ok {
			t.Errorf("normPrice(%q) = %v", in, got)
		}
	}
	// A was-price over 5x the price is not believed; the product is kept.
	p, drop := Agree(read(t, product("Drive 12TB Hard Drive", "10.00", "999.99")), read(t, product("Drive 12TB Hard Drive", "10.00", "999.99")))
	if drop != "" || p.WasPrice != "" || p.Price != "10.00" {
		t.Fatalf("ratio cap: %+v %q", p, drop)
	}
	p, _ = Agree(read(t, product("Drive 12TB Hard Drive", "10.00", "50.00")), read(t, product("Drive 12TB Hard Drive", "10.00", "50.00")))
	if p.WasPrice != "50.00" {
		t.Fatalf("5x was-price dropped: %+v", p)
	}
}

func TestCategoryFitDesktopDrives(t *testing.T) {
	for title, want := range map[string]bool{
		"WD Blue 4TB Desktop Hard Disk Drive":                                        true,
		"Seagate Expansion Desktop 16TB External Hard Drive":                         true,
		"Toshiba X300 Performance 8TB Desktop & Gaming Hard Drive":                   true,
		"Seagate BarraCuda 2TB Internal Hard Drive 7200 RPM for Computer Desktop PC": true,
		"ASUS ROG Gaming Desktop PC 2TB HDD":                                         false,
		"Dell OptiPlex Desktop Computer 1TB":                                         false,
		"MSI Summit E13 Flip Evo 13.4in Laptop 1TB":                                  false,
	} {
		if got := categoryFit(deal.CategoryHDD, title); got != want {
			t.Errorf("categoryFit(%q) = %v, want %v", title, got, want)
		}
	}
}

func TestSlugMatchesTitle(t *testing.T) {
	for _, tc := range []struct {
		u, title string
		want     bool
	}{
		{"https://www.wine.com/product/tolaini-al-passo-toscana-2022/3721589", "Tolaini Al Passo Toscana 2022", true},
		{"https://www.wine.com/product/tolaini-al-passo-toscana-2021/2325474", "Tolaini Al Passo Toscana 2022", false},
		{"https://www.wine.com/product/caymus-cabernet-sauvignon-2021/1", "Tolaini Al Passo Toscana 2022", false},
		{"https://www.totalwine.com/wine/red-wine/cabernet-sauvignon/caymus-vineyards-california-cabernet-sauvignon/p/2126227887", "Caymus Vineyards California Cabernet Sauvignon 750ml", true},
		{"https://www.totalwine.com/wine/red-wine/cabernet-sauvignon/1858-by-caymus-vineyards-cabernet-sauvignon-paso-robles/p/214578750", "Caymus Vineyards California Cabernet Sauvignon", false},
		{"https://www.wine.com/product/x/1", "", false},
	} {
		if got := slugMatchesTitle(tc.u, tc.title, false); got != tc.want {
			t.Errorf("slugMatchesTitle(%s, %q) = %v", tc.u, tc.title, got)
		}
	}
}

// An open-time image and its tracked click can name different products: the
// URL's slug must match what the image showed.
func TestParserDropsLinkTitleMismatch(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine}, "winecom.html")
	r.fv.set(0, answers{A: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":null,"brand":"Tolaini","currency":"USD"}]}`})
	r.w.handle("links.e.wine.com", func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, "https://www.wine.com/product/caymus-cabernet-sauvignon-2021/1234567", http.StatusFound)
	})
	doc := `<a href="https://links.e.wine.com/ls/click?t=1"><img src="` + r.cands[0].Src + `" alt="Wine.com" width="280"></a>`
	raws := r.parse(t, imapmail.Message{ID: "wc-x@example.com", From: wineSender, HTML: doc})
	if len(raws) != 0 || r.m.Drops(r.src, DropLinkTitleMismatch) != 1 {
		t.Fatalf("raws %d, mismatch drops %d", len(raws), r.m.Drops(r.src, DropLinkTitleMismatch))
	}
}

// A failed read A returns the unused read B's reservation.
func TestReadAFailureRefundsReadB(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine, perDay: 100}, "winecom.html")
	r.fv.fail.Store(1)
	doc := `<a href="https://links.e.wine.com/ls/click?t=1"><img src="` + r.cands[0].Src + `" alt="Wine.com" width="280"></a>`
	r.parse(t, imapmail.Message{ID: "wc-y@example.com", From: wineSender, HTML: doc})
	if st := r.budget.Stats(); st.UsedToday != 1 || r.fv.calls.Load() != 1 {
		t.Fatalf("used %d after a failed read A (calls %d), want 1", st.UsedToday, r.fv.calls.Load())
	}
}

func TestVisionFourXX(t *testing.T) {
	for code, final := range map[int]bool{400: true, 404: true, 413: true, 422: true, 401: false, 403: false, 408: false, 429: false, 500: false, 503: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		v := &Vision{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
		_, err := v.Read(context.Background(), Image{Bytes: []byte("x"), ContentType: "image/png"}, PromptA)
		srv.Close()
		if err == nil || errors.Is(err, ErrVisionRefused) != final {
			t.Errorf("HTTP %d: err %v, final %v", code, err, final)
		}
	}
}

func TestParseReadingStrictKeys(t *testing.T) {
	for _, s := range []string{
		`{"products":[{"title":"A","title":"B","price":"1"}]}`,
		`{"Products":[]}`,
		`{"products":[{"Title":"A","price":"1"}]}`,
		`{"products":[{"title":"A","PRICE":"1"}]}`,
		`{"products":[],"products":[]}`,
		`{"products":["x"]}`,
	} {
		if _, err := ParseReading(s); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("ParseReading(%s) accepted", s)
		}
	}
}

func TestDialerSpecialPurpose(t *testing.T) {
	for _, a := range []string{
		"0.1.2.3:443", "240.0.0.1:443", "192.0.0.8:443", "198.18.0.1:443", "203.0.113.5:443",
		"[64:ff9b::a00:1]:443", "[2002:a00:1::1]:443", "[2001:db8::1]:443",
	} {
		if err := publicOnly("tcp", a, nil); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("%s allowed", a)
		}
	}
}

func TestUpgradeURLKeepsIPv6Brackets(t *testing.T) {
	u, ok := upgradeURL("http://[2001:4860::1]/x.png")
	if !ok || u.Host != "[2001:4860::1]" || u.String() != "https://[2001:4860::1]/x.png" {
		t.Fatalf("got %v %v", u, ok)
	}
}

// Product URLs are bounded, and a slug segment that is an id is refused.
func TestCanonicalBounds(t *testing.T) {
	tw := mustProfile(t, "totalwine")
	wc := mustProfile(t, "winecom")
	for _, tc := range []struct {
		p    *Profile
		u    string
		want bool
	}{
		{tw, "https://www.totalwine.com/wine/red-wine/cabernet-sauvignon/caymus/p/2126227887", true},
		{tw, "https://www.totalwine.com/a/b/c/d/e/f/p/1", false},
		{tw, "https://www.totalwine.com/wine/p/12345678901", false},
		{tw, "https://www.totalwine.com/wine/caymus-0123456789abcdef0123/p/1", false},
		{tw, "https://www.totalwine.com/wine/caymus-123456789/p/1", false},
		{wc, "https://www.wine.com/product/tolaini-al-passo-toscana-2022/3721589", true},
		{wc, "https://www.wine.com/product/tolaini/3721589/extra", false},
	} {
		u, _ := url.Parse(tc.u)
		if _, got := canonical(tc.p, u); got != tc.want {
			t.Errorf("canonical(%s) = %v", tc.u, got)
		}
	}
}
