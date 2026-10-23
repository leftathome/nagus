package promo

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/offer"
)

// A restart costs ZERO vision calls, image fetches and link resolutions for
// mail already processed, re-emits the same products, and keeps the day's
// vision usage (nagus-p7x).
func TestRestartDoesNotReprocess(t *testing.T) {
	state := offer.NewMemoryStore()
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine, perDay: 1000, state: state}, "winecom.html")
	targets := map[string]string{}
	for i, c := range r.cands {
		u, _ := url.Parse(c.Href)
		targets[u.RequestURI()] = "https://www.wine.com/product/tolaini-al-passo-toscana-2022/3721589"
		r.fv.set(uint8(i), answers{A: `{"products":[]}`})
	}
	r.fv.set(0, answers{A: `{"products":[{"title":"Tolaini Al Passo Toscana 2022","price":"27.97","was_price":"29","brand":"Tolaini","currency":"USD"}]}`})
	r.w.handle("links.e.wine.com", func(rw http.ResponseWriter, req *http.Request) {
		http.Redirect(rw, req, targets[req.URL.RequestURI()], http.StatusFound)
	})
	msg := imapmail.Message{ID: "wc-restart@example.com", From: wineSender, HTML: fixture(t, "winecom.html"), Received: testNow, UIDValidity: 7, UID: 42}
	first := r.parse(t, msg)
	if len(first) != 1 {
		t.Fatalf("first run: %d products", len(first))
	}
	calls, fetches, links := r.fv.calls.Load(), r.w.count("image.richrelevance.com"), r.w.count("links.e.wine.com")
	usage := r.budget.Stats().UsedToday
	if usage != 2*len(r.cands) {
		t.Fatalf("usage %d", usage)
	}

	r.restart() // a new process: empty memory, the same database
	if st := r.budget.Stats(); st.UsedToday != usage {
		t.Fatalf("restart reset the day's usage: %d, want %d", st.UsedToday, usage)
	}
	again := r.parse(t, msg)
	if !reflect.DeepEqual(again, first) {
		t.Fatalf("re-emitted products differ:\n%+v\n%+v", again, first)
	}
	if r.fv.calls.Load() != calls || r.w.count("image.richrelevance.com") != fetches || r.w.count("links.e.wine.com") != links {
		t.Fatalf("restart reprocessed: calls %d->%d fetches %d->%d links %d->%d", calls, r.fv.calls.Load(),
			fetches, r.w.count("image.richrelevance.com"), links, r.w.count("links.e.wine.com"))
	}
	if r.m.Messages(r.src, MsgRestored) != 1 || r.p.incomplete() {
		t.Fatal("not counted as restored, or reported incomplete")
	}
	pm, ok, _ := state.GetPromoMessage(t.Context(), r.src, MessageKey(wineSender, msg.ID))
	if !ok || pm.UIDValidity != 7 || pm.UID != 42 || pm.Outcome != MsgProcessed {
		t.Fatalf("ledger row %+v %v", pm, ok)
	}
}

// A message deferred by the budget is not final: after a restart it is
// finished, and the images read before the restart cost no call again.
func TestRestartResumesDeferredMessage(t *testing.T) {
	state := offer.NewMemoryStore()
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 8, perDay: 1000, state: state}, "newegg.html")
	r.fv.set(1, answers{A: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "")})
	r.parse(t, neweggMsg(t))
	if !r.p.incomplete() {
		t.Fatal("the first poll was not deferred")
	}
	if _, ok, _ := state.GetPromoMessage(t.Context(), r.src, MessageKey(neweggSender, neweggMsg(t).ID)); ok {
		t.Fatal("a deferred message was recorded as processed")
	}
	before := r.fv.calls.Load()
	for i := 0; i < 20 && (i == 0 || r.p.incomplete()); i++ {
		if i == 0 {
			r.restart()
		}
		r.parse(t, neweggMsg(t))
	}
	if r.p.incomplete() {
		t.Fatal("never completed")
	}
	if got := r.fv.calls.Load(); got != int64(2*len(r.cands)) {
		t.Fatalf("calls %d (before restart %d), want %d: an image was read twice", got, before, 2*len(r.cands))
	}
	if _, ok, _ := state.GetPromoMessage(t.Context(), r.src, MessageKey(neweggSender, neweggMsg(t).ID)); !ok {
		t.Fatal("the finished message was not recorded")
	}
}

// A stored product that no longer validates is not re-emitted: the message
// is processed again.
func TestRestoreRefusesInvalidStoredLine(t *testing.T) {
	state := offer.NewMemoryStore()
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 1000, perDay: 1000, state: state}, "newegg.html")
	key := MessageKey(neweggSender, neweggMsg(t).ID)
	_ = state.PutPromoMessage(t.Context(), offer.PromoMessage{
		SourceID: r.src, MessageKey: key, ProcessedAt: testNow, Outcome: MsgProcessed,
		Products: []byte(`[{"line":"{\"category\":\"hdd\",\"title\":\"x\",\"price\":\"1\",\"url\":\"http://insecure.example.com/\"}"}]`),
	})
	r.parse(t, neweggMsg(t))
	if r.fv.calls.Load() == 0 || r.m.Messages(r.src, MsgRestored) != 0 {
		t.Fatal("an invalid stored line was re-emitted instead of reprocessing")
	}
}
