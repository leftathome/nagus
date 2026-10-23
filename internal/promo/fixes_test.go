package promo

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/offer"
)

// wine.com's real chain (traced 2026-10-17, hosts and paths only):
// links.e.wine.com -302-> image.richrelevance.com -303-> http://recs.richrelevance.com
// -301-> https://www.wine.com/product/<id>. The http hop on a listed
// redirect host is upgraded; http elsewhere is still refused.
func TestResolveWineComChain(t *testing.T) {
	w := newWorld(t)
	w.handle("links.e.wine.com", func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/uni/ls/click":
			http.Redirect(rw, req, "https://image.richrelevance.com/rrmail/click/recs?t=1", http.StatusFound)
		case "/to-http-product":
			http.Redirect(rw, req, "http://www.wine.com/product/3721589", http.StatusFound)
		case "/to-http-other":
			http.Redirect(rw, req, "http://evil.example.org/x", http.StatusFound)
		}
	})
	w.handle("image.richrelevance.com", func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, "http://recs.richrelevance.com/rrserver/click?t=2", http.StatusSeeOther)
	})
	w.handle("recs.richrelevance.com", func(rw http.ResponseWriter, req *http.Request) {
		if req.TLS == nil {
			t.Error("the http hop was requested over http")
		}
		http.Redirect(rw, req, "https://www.wine.com/product/3721589?uid=SECRET", http.StatusMovedPermanently)
	})
	r := &Resolver{Client: w.client()}
	wc := mustProfile(t, "winecom")
	for href, want := range map[string][2]string{
		"https://links.e.wine.com/uni/ls/click?t=0":  {"https://www.wine.com/product/3721589", ""},
		"https://links.e.wine.com/to-http-product":   {"", DropHTTPDowngrade},
		"https://links.e.wine.com/to-http-other?t=0": {"", DropHTTPDowngrade},
	} {
		got, drop, _ := r.Resolve(context.Background(), wc, Candidate{Href: href})
		if got != want[0] || drop != want[1] {
			t.Errorf("Resolve(%s) = %q, %q; want %q, %q", href, got, drop, want[0], want[1])
		}
	}
	if w.count("www.wine.com") != 0 || w.count("evil.example.org") != 0 {
		t.Fatal("a product page or a foreign host was requested")
	}
}

// A slug-less wine.com link is accepted (the profile declares it) and the
// product is marked link not name-verified, also when restored.
func TestParserSluglessLinkMarked(t *testing.T) {
	state := offer.NewMemoryStore()
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine, perPoll: 1000, perDay: 1000, state: state}, "winecom.html")
	r.fv.set(0, answers{A: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":null,"brand":"Tolaini","currency":"USD"}]}`})
	r.w.handle("links.e.wine.com", func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, "https://www.wine.com/product/3721589", http.StatusFound)
	})
	msg := imapmail.Message{ID: "wc-slugless@example.com", From: wineSender, HTML: fixture(t, "winecom.html"), Received: testNow}
	raws := r.parse(t, msg)
	if len(raws) != 1 || raws[0].SourceURL != "https://www.wine.com/product/3721589" || raws[0].Aspects[AspectLinkNameVerified] != "false" {
		t.Fatalf("raws %+v", raws)
	}
	r.restart()
	again := r.parse(t, msg)
	if len(again) != 1 || again[0].Aspects[AspectLinkNameVerified] != "false" || r.m.Messages(r.src, MsgRestored) != 1 {
		t.Fatalf("restored %+v", again)
	}
	// A profile that does not declare slug-less links drops them.
	tw := mustProfile(t, "totalwine")
	p := &Parser{cfg: Config{Profile: tw}}
	if _, why := p.linkCheck(true, "https://www.totalwine.com/p/123", "Caymus Cabernet"); why != DropLinkUnverifiable {
		t.Fatalf("totalwine slug-less: %q", why)
	}
}

// A Newegg tile without an item alt is resolved through its tracked link:
// the slugged, slug-less and Product.aspx shapes.
func TestNeweggTileWithoutItemAlt(t *testing.T) {
	ne := mustProfile(t, "newegg")
	doc := `<a href="https://l.promo.newegg.com/rts/go2.aspx?t=1"><img src="https://c2.neweggimages.com/tapa/a.jpg" alt="" width="214"></a>` +
		`<img src="https://c2.neweggimages.com/tapa/b.jpg" alt="" width="214">`
	sel := SelectImages(doc, ne)
	if len(sel.Candidates) != 1 || sel.Candidates[0].Item != "" || sel.Candidates[0].Href == "" || sel.Skipped[SkipNoLink] != 1 {
		t.Fatalf("selection %+v %v", sel.Candidates, sel.Skipped)
	}
	w := newWorld(t)
	dest := map[string]string{
		"/rts/go2.aspx/slugless": "https://www.newegg.com:443/p/0PD-0072-00095?item=x&utm=y",
		"/rts/go2.aspx/slugged":  "https://www.newegg.com/seagate-ironwolf-8tb-nas-hard-drive/p/N82E16822179123?t=z",
		"/rts/go2.aspx/aspx":     "https://www.newegg.com/Product/Product.aspx?Item=9SIC11UKGE3529&t=z",
		"/rts/go2.aspx/landing":  "https://www.newegg.com/d/Best-Sellers",
	}
	w.handle("l.promo.newegg.com", func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, dest[req.URL.Path], http.StatusFound)
	})
	w.handle("www.newegg.com", func(rw http.ResponseWriter, req *http.Request) { http.Error(rw, "bot wall", http.StatusForbidden) })
	r := &Resolver{Client: w.client()}
	for path, want := range map[string][2]string{
		"slugless": {"https://www.newegg.com/p/0PD-0072-00095", ""},
		"slugged":  {"https://www.newegg.com/seagate-ironwolf-8tb-nas-hard-drive/p/N82E16822179123", ""},
		"aspx":     {"https://www.newegg.com/Product/Product.aspx?Item=9SIC11UKGE3529", ""},
		"landing":  {"", DropLinkUnresolved},
	} {
		got, drop, _ := r.Resolve(context.Background(), ne, Candidate{Href: "https://l.promo.newegg.com/rts/go2.aspx/" + path})
		if got != want[0] || drop != want[1] {
			t.Errorf("%s: %q %q, want %q %q", path, got, drop, want[0], want[1])
		}
	}
	p := &Parser{cfg: Config{Profile: ne}}
	if v, why := p.linkCheck(true, "https://www.newegg.com/p/0PD-0072-00095", "Seagate IronWolf 8TB NAS Hard Drive"); v || why != "" {
		t.Errorf("slug-less newegg: verified %v, %q", v, why)
	}
	if v, why := p.linkCheck(true, "https://www.newegg.com/seagate-ironwolf-8tb-nas-hard-drive/p/N82E16822179123", "Seagate IronWolf 8TB NAS Hard Drive"); !v || why != "" {
		t.Errorf("slugged newegg: verified %v, %q", v, why)
	}
	if _, why := p.linkCheck(true, "https://www.newegg.com/wd-red-plus-12tb-nas-hard-drive/p/N82E16822234441", "Seagate IronWolf 8TB NAS Hard Drive"); why != DropLinkTitleMismatch {
		t.Errorf("mismatched slug: %q", why)
	}
	// A tile WITH its item number is never followed.
	if v, why := p.linkCheck(false, "https://www.newegg.com/p/N82E16834156677", "x"); !v || why != "" {
		t.Errorf("item-number URL: %v %q", v, why)
	}
}

// A message processed under older rules -- here stored final with no
// products, as the first production polls did -- is processed again; its
// readings are reused, so no vision call is made (nagus-3hc).
func TestOlderRulesMessageIsReprocessed(t *testing.T) {
	state := offer.NewMemoryStore()
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine, perPoll: 1000, perDay: 1000, state: state}, "winecom.html")
	r.fv.set(0, answers{A: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":null,"brand":"Tolaini","currency":"USD"}]}`})
	targets := map[string]string{}
	for _, c := range r.cands {
		u, _ := url.Parse(c.Href)
		targets[u.RequestURI()] = "https://www.wine.com/product/3721589"
	}
	r.w.handle("links.e.wine.com", func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, targets[req.URL.RequestURI()], http.StatusFound)
	})
	msg := imapmail.Message{ID: "wc-oldrules@example.com", From: wineSender, HTML: fixture(t, "winecom.html"), Received: testNow}
	if raws := r.parse(t, msg); len(raws) != 1 {
		t.Fatalf("first run: %d", len(raws))
	}
	calls := r.fv.calls.Load()
	key := MessageKey(wineSender, msg.ID)
	_ = state.PutPromoMessage(context.Background(), offer.PromoMessage{
		SourceID: r.src, MessageKey: key, ProcessedAt: testNow,
		// "" is exactly what production rows carry: the column's default for
		// records written before rules_version existed.
		Outcome: MsgProcessed, RulesVersion: "", Products: []byte(`[]`),
	})
	r.restart()
	raws := r.parse(t, msg)
	if len(raws) != 1 || r.m.Messages(r.src, MsgRestored) != 0 || r.fv.calls.Load() != calls {
		t.Fatalf("reprocess: %d products, restored %d, calls %d -> %d", len(raws), r.m.Messages(r.src, MsgRestored), calls, r.fv.calls.Load())
	}
	if pm, _, _ := state.GetPromoMessage(context.Background(), r.src, key); pm.RulesVersion != RulesVersion || string(pm.Products) == "[]" {
		t.Fatalf("ledger not refreshed: %+v", pm)
	}
	// No reprocess loop: after another restart the refreshed record is
	// restored, with no vision call.
	r.restart()
	restoredBefore := r.m.Messages(r.src, MsgRestored)
	if raws := r.parse(t, msg); len(raws) != 1 || r.m.Messages(r.src, MsgRestored) != restoredBefore+1 || r.fv.calls.Load() != calls {
		t.Fatalf("second restart: %d products, restored %d -> %d, calls %d -> %d",
			len(raws), restoredBefore, r.m.Messages(r.src, MsgRestored), calls, r.fv.calls.Load())
	}
}
