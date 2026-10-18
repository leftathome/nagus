package promo

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
)

// Placeholders only: no real address belongs in this repo. A sender must
// be on the profile's retailer domain, so the placeholder local part is.
const (
	neweggSender = "placeholder@newegg.com"
	wineSender   = "placeholder@e.wine.com"
)

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

type rigOpts struct {
	profile, category string
	perPoll, perDay   int
	vision            *Vision
	state             offer.MailState
}

type prig struct {
	w      *world
	fv     *fakeVision
	p      *Parser
	m      *Metrics
	budget *Budget
	src    string
	idsMu  sync.Mutex
	ids    map[string]uint8 // image src (path+query) -> image id
	cands  []Candidate
	// restart rebuilds the parser (and its budget) as a new process would.
	restart func()
}

// newPRig wires a parser for one fixture: every selected image is served by
// the world with a distinct id, in selection order.
func newPRig(t *testing.T, o rigOpts, fixtureFile string) *prig {
	t.Helper()
	r := &prig{w: newWorld(t), fv: newFakeVision(t), src: "imap:promo-" + o.profile, ids: map[string]uint8{}}
	prof := mustProfile(t, o.profile)
	r.cands = SelectImages(fixture(t, fixtureFile), prof).Candidates
	for i, c := range r.cands {
		u, _ := url.Parse(c.Src)
		r.ids[u.RequestURI()] = uint8(i)
	}
	imgs := map[uint8][]byte{}
	for _, id := range r.ids {
		imgs[id] = testImage(id, 120)
	}
	for _, h := range prof.ImageHosts {
		r.w.handle(h, func(rw http.ResponseWriter, req *http.Request) {
			r.idsMu.Lock()
			id, ok := r.ids[req.URL.RequestURI()]
			b := imgs[id]
			r.idsMu.Unlock()
			if !ok {
				http.NotFound(rw, req)
				return
			}
			rw.Header().Set("Content-Type", "image/png")
			if b == nil {
				b = testImage(id, 120)
			}
			_, _ = rw.Write(b)
		})
	}
	vis := o.vision
	if vis == nil {
		vis = r.fv.vision()
	}
	cl := r.w.client()
	r.restart = func() {
		r.budget = NewBudget(o.perDay, func() time.Time { return testNow })
		r.budget.State = o.state
		r.m = NewMetrics(r.budget, vis.Enabled())
		p, err := NewParser(Config{
			Profile: prof, Category: o.category, SourceID: r.src, Vision: vis,
			Fetcher:  &Fetcher{Client: cl, Retries: 1, Backoff: func(int) time.Duration { return 0 }},
			Resolver: &Resolver{Client: cl}, Budget: r.budget, PerPoll: o.perPoll, Metrics: r.m,
			Now: func() time.Time { return testNow }, Logf: t.Logf, State: o.state,
		})
		if err != nil {
			t.Fatal(err)
		}
		r.p = p
	}
	r.restart()
	return r
}

// addImage serves a further image at uri with id.
func (r *prig) addImage(uri string, id uint8) {
	r.idsMu.Lock()
	defer r.idsMu.Unlock()
	r.ids[uri] = id
}

func (r *prig) parse(t *testing.T, m imapmail.Message) []listing.Raw {
	t.Helper()
	r.p.startPoll()
	raws, err := r.p.Parse(m)
	if err != nil {
		t.Fatal(err)
	}
	return raws
}

func neweggMsg(t *testing.T) imapmail.Message {
	return imapmail.Message{ID: "nl-1@example.com", From: neweggSender, HTML: fixture(t, "newegg.html"), Received: testNow.Add(-time.Hour)}
}

func TestParserNeweggFixture(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD}, "newegg.html")
	// Image 0 is a laptop, 1 a drive, 2 a misread price, 3 a banner of two
	// products, 4 a drive whose was-price is below its price; the rest show
	// nothing with a price.
	r.fv.set(0, answers{A: product("MSI Summit E13 Flip Evo 13.4in Laptop", "589.99", "999.99")})
	r.fv.set(1, answers{
		A: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "279.99"),
		B: product("WD Red Plus 12TB NAS Hard Drive", "$219.99", "$279.99"),
	})
	r.fv.set(2, answers{A: product("Seagate IronWolf 8TB NAS Hard Drive", "139.99", ""), B: product("Seagate IronWolf 8TB NAS Hard Drive", "189.99", "")})
	r.fv.set(3, answers{A: `{"products":[{"title":"A 1TB Hard Drive","price":"1.00","was_price":null,"brand":null,"currency":null},{"title":"B 2TB Hard Drive","price":"2.00","was_price":null,"brand":null,"currency":null}]}`})
	r.fv.set(4, answers{A: product("Toshiba N300 4TB Hard Drive", "99.99", "89.99")})

	raws := r.parse(t, neweggMsg(t))
	if len(raws) != 1 {
		t.Fatalf("raws = %d, want 1: %+v", len(raws), raws)
	}
	got := raws[0]
	wantURL, _ := NeweggURL(r.cands[1].Item)
	if got.SourceURL != wantURL || got.PriceCents != 21999 || got.Currency != "USD" || got.Title != "WD Red Plus 12TB NAS Internal Hard Drive" {
		t.Fatalf("raw %+v", got)
	}
	if got.SourceKey != Key("newegg", wantURL) || !strings.HasPrefix(got.SourceKey, "promo-") {
		t.Fatalf("source key %q", got.SourceKey)
	}
	for k, v := range map[string]string{
		deal.AspectSubmittedBy: "retailer:newegg", AspectOrigin: OriginEmailImage, AspectProfile: "newegg",
		AspectVisionModel: "test-vision", AspectCompareAt: "27999", "seller": "Newegg", deal.AspectSchema: deal.SchemaID,
	} {
		if got.Aspects[k] != v {
			t.Errorf("aspect %s = %q, want %q", k, got.Aspects[k], v)
		}
	}
	// Nothing tracked is stored: no aspect or field carries the mail's URLs.
	for k, v := range got.Aspects {
		if strings.Contains(v, "REDACTED") || strings.Contains(v, "neweggimages") || strings.Contains(v, "l.promo.newegg.com") {
			t.Errorf("aspect %s carries a mail URL: %q", k, v)
		}
	}
	n := len(r.cands)
	if c := r.fv.calls.Load(); c != int64(2*n) {
		t.Fatalf("vision calls %d, want %d (two per image)", c, 2*n)
	}
	for reason, want := range map[string]int64{
		DropNotInCategory: 1, DropPriceDisagree: 1, DropMultiProduct: 1, DropPriceSanity: 1, DropNoProduct: int64(n - 5),
	} {
		if g := r.m.Drops(r.src, reason); g != want {
			t.Errorf("drops[%s] = %d, want %d", reason, g, want)
		}
	}
	if r.m.Images(r.src, StageSelected) != int64(n) || r.m.Images(r.src, StageFetched) != int64(n) || r.m.Images(r.src, StageKept) != 1 {
		t.Errorf("stages: selected %d fetched %d kept %d", r.m.Images(r.src, StageSelected), r.m.Images(r.src, StageFetched), r.m.Images(r.src, StageKept))
	}
	if r.w.count("l.promo.newegg.com") != 0 {
		t.Fatal("a Newegg tracked link was followed")
	}

	// A re-poll reuses the results: no fetch, no vision call, same product.
	fetches := r.w.count("c2.neweggimages.com")
	again := r.parse(t, neweggMsg(t))
	if len(again) != 1 || again[0].SourceKey != got.SourceKey || r.fv.calls.Load() != int64(2*n) || r.w.count("c2.neweggimages.com") != fetches {
		t.Fatalf("re-poll: %d raws, %d calls, %d fetches", len(again), r.fv.calls.Load(), r.w.count("c2.neweggimages.com"))
	}
	if r.m.Messages(r.src, MsgCached) != 1 || r.m.Messages(r.src, MsgProcessed) != 1 || r.p.incomplete() {
		t.Fatal("message outcomes")
	}
	// Mutating a returned listing does not reach the cache.
	again[0].Aspects["x"] = "y"
	if third := r.parse(t, neweggMsg(t)); third[0].Aspects["x"] != "" {
		t.Fatal("cache shared its aspects map")
	}
}

// The budget defers images to later polls and never reads one twice.
func TestParserBudgetDefers(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 8, perDay: 1000}, "newegg.html")
	r.fv.set(1, answers{A: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "")})
	n := len(r.cands)
	polls := 0
	var raws []listing.Raw
	for {
		polls++
		raws = r.parse(t, neweggMsg(t))
		if !r.p.incomplete() {
			break
		}
		if polls > n {
			t.Fatal("never completed")
		}
	}
	if want := (n + 3) / 4; polls != want {
		t.Errorf("polls = %d, want %d (4 images a poll)", polls, want)
	}
	if len(raws) != 1 || r.fv.calls.Load() != int64(2*n) {
		t.Fatalf("raws %d, calls %d (want %d)", len(raws), r.fv.calls.Load(), 2*n)
	}
	if r.m.Messages(r.src, MsgDeferred) != int64(polls-1) {
		t.Errorf("deferred %d", r.m.Messages(r.src, MsgDeferred))
	}
	// Every deferring poll counted its exhausted budget.
	if ex := r.budget.Stats().Exhausted["poll"]; ex < int64(polls-1) {
		t.Errorf("poll budget exhausted %d times, want >= %d", ex, polls-1)
	}

	// The DAILY budget is shared and holds across polls.
	d := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 100, perDay: 6}, "newegg.html")
	d.parse(t, neweggMsg(t))
	d.parse(t, neweggMsg(t))
	if d.fv.calls.Load() != 6 || !d.p.incomplete() || d.budget.Stats().Exhausted["day"] == 0 {
		t.Fatalf("day budget: %d calls, stats %+v", d.fv.calls.Load(), d.budget.Stats())
	}
}

func TestParserVisionOffAndTransient(t *testing.T) {
	off := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, vision: &Vision{}}, "newegg.html")
	if raws := off.parse(t, neweggMsg(t)); len(raws) != 0 || !off.p.incomplete() {
		t.Fatal("vision off produced offers or reported complete")
	}
	if off.w.count("c2.neweggimages.com") != 0 || off.fv.calls.Load() != 0 || off.m.Messages(off.src, MsgDisabled) != 1 {
		t.Fatal("vision off still fetched or called")
	}

	// The endpoint failing transiently: the image is retried on the next
	// poll, and the drop is final only after maxAttempts.
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD}, "newegg.html")
	r.fv.set(1, answers{A: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "")})
	// The failure is aimed at the drive's own image (id 1), not at whichever
	// image happens to be read first: no product this poll, retried next.
	r.fv.failOn(1, 1)
	if raws := r.parse(t, neweggMsg(t)); len(raws) != 0 {
		t.Fatalf("the image whose read failed produced %d offers", len(raws))
	}
	if !r.p.incomplete() {
		t.Fatal("a transient vision failure left the poll complete")
	}
	if raws := r.parse(t, neweggMsg(t)); len(raws) != 1 || r.p.incomplete() {
		t.Fatalf("retry: %d raws, incomplete %v", len(raws), r.p.incomplete())
	}
	if r.m.Drops(r.src, DropVisionFailed) != 0 {
		t.Fatal("a transient failure was counted as a drop")
	}
	if st := r.budget.Stats(); st.CallsErr != 1 {
		t.Fatalf("errors %d", st.CallsErr)
	}
}

// A sender outside the profile, or a forward, is never read.
func TestParserRefusesForeignSender(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD}, "newegg.html")
	for _, m := range []imapmail.Message{
		{ID: "a@example.com", From: "someone@example.com", HTML: fixture(t, "newegg.html")},
		{ID: "b@example.com", From: neweggSender, ForwardedBy: "household@example.com", HTML: fixture(t, "newegg.html")},
	} {
		if raws := r.parse(t, m); len(raws) != 0 {
			t.Fatal("foreign mail read")
		}
	}
	if r.fv.calls.Load() != 0 || r.m.Drops(r.src, DropSenderNotProfile) != 2 {
		t.Fatal("foreign mail reached vision")
	}
}

func TestParserWineFollowsLinks(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine}, "winecom.html")
	if len(r.cands) != 4 {
		t.Fatalf("candidates %d", len(r.cands))
	}
	// Each recommendation's tracked link redirects: 0 and 1 to products
	// (with tracking to strip), 2 to another host, 3 to an http product URL.
	targets := map[string]string{}
	dest := []string{
		"https://www.wine.com/product/tolaini-al-passo-toscana-2022/3721589?s=email&uid=SUBSCRIBER#x",
		"https://www.wine.com/product/example-cellars-columbia-valley-syrah-2021/1234567",
		"https://wine.example.net/product/x/1",
		"http://www.wine.com/product/example-cellars-syrah-2021/1234567",
	}
	for i, c := range r.cands {
		u, _ := url.Parse(c.Href)
		targets[u.RequestURI()] = dest[i]
		r.fv.set(uint8(i), answers{A: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":"29","brand":"Tolaini","currency":"USD"}]}`})
	}
	r.fv.set(1, answers{A: `{"products":[{"title":"Example Cellars Columbia Valley Syrah 2021","price":"24.99","was_price":null,"brand":"Example Cellars","currency":"USD"}]}`})
	r.w.handle("links.e.wine.com", func(rw http.ResponseWriter, req *http.Request) {
		if d, ok := targets[req.URL.RequestURI()]; ok {
			http.Redirect(rw, req, d, http.StatusFound)
			return
		}
		http.NotFound(rw, req)
	})
	raws := r.parse(t, imapmail.Message{ID: "wc-1@example.com", From: wineSender, HTML: fixture(t, "winecom.html"), Received: testNow})
	if len(raws) != 2 {
		t.Fatalf("raws %d: %+v", len(raws), raws)
	}
	if raws[0].SourceURL != "https://www.wine.com/product/tolaini-al-passo-toscana-2022/3721589" || raws[0].Aspects["wine_producer"] != "Tolaini" ||
		raws[0].Aspects[AspectCompareAt] != "2900" || raws[0].PriceCents != 2797 {
		t.Fatalf("raw 0 %+v", raws[0])
	}
	if raws[1].SourceURL != "https://www.wine.com/product/example-cellars-columbia-valley-syrah-2021/1234567" {
		t.Fatalf("raw 1 %q", raws[1].SourceURL)
	}
	if r.m.Drops(r.src, DropLinkOffsite) != 1 || r.m.Drops(r.src, DropHTTPDowngrade) != 1 {
		t.Fatalf("drops offsite %d downgrade %d", r.m.Drops(r.src, DropLinkOffsite), r.m.Drops(r.src, DropHTTPDowngrade))
	}
	if r.w.count("www.wine.com") != 0 || r.w.count("wine.example.net") != 0 {
		t.Fatal("a product page or a foreign host was requested")
	}
	// Each tracked link was followed exactly once.
	if r.w.count("links.e.wine.com") != 4 {
		t.Fatalf("tracked link requests %d", r.w.count("links.e.wine.com"))
	}
	r.parse(t, imapmail.Message{ID: "wc-1@example.com", From: wineSender, HTML: fixture(t, "winecom.html"), Received: testNow})
	if r.w.count("links.e.wine.com") != 4 {
		t.Fatal("a re-poll followed the tracked links again")
	}
}

func TestDedupeKeepsNewest(t *testing.T) {
	older := listing.Raw{SourceKey: "k", PriceCents: 100, EndsAt: testNow}
	newer := listing.Raw{SourceKey: "k", PriceCents: 90, EndsAt: testNow.Add(time.Hour)}
	other := listing.Raw{SourceKey: "j"}
	out := Dedupe([]listing.Raw{older, other, newer})
	if len(out) != 2 || out[0].PriceCents != 90 || out[1].SourceKey != "j" {
		t.Fatalf("dedupe %+v", out)
	}
}
