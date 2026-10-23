package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/go-service-kit/mcp"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/watch"
)

// nagus-417: offer timing on rows, expired hiding, new_deals, list_watches.
//
// offerTestServer is newTestServer (three eBay fixture drives) with the offer
// layer on and each drive's offer written with controlled times:
//
//	HGST He10 (the "great" used drive): first seen 1h ago           -> new
//	Exos X18: first seen 10 days ago, price dropped 2h ago           -> price drop
//	WD Red Pro: first seen 10 days ago, unchanged                    -> not new
type offerFixture struct {
	srv                *server
	offers             *offer.MemoryStore
	now                time.Time
	hgst, exos, wdRed  string // item ids
	exosOld, exosPrice int64
}

const (
	hgstKey = "v1|110599999903|0"
	exosKey = "v1|110599999901|0"
	wdKey   = "v1|110599999902|0"
)

func offerTestServer(t *testing.T) offerFixture {
	t.Helper()
	ctx := context.Background()
	srv := newTestServer(t)
	offers := offer.NewMemoryStore()
	now := time.Now().UTC().Truncate(time.Second)
	f := offerFixture{
		srv: srv, offers: offers, now: now,
		hgst: offer.DeterministicID("ebay", hgstKey), exos: offer.DeterministicID("ebay", exosKey), wdRed: offer.DeterministicID("ebay", wdKey),
		exosOld: 32000, exosPrice: 27999,
	}
	put := func(key string, price int64, seen time.Time) {
		t.Helper()
		if err := offers.Put(ctx, offer.Offer{SourceID: "ebay", SourceKey: key, PriceCents: price, Currency: "USD", LastSeen: seen}); err != nil {
			t.Fatal(err)
		}
	}
	put(hgstKey, 8950, now.Add(-time.Hour))
	put(hgstKey, 8950, now.Add(-10*time.Minute))
	put(exosKey, f.exosOld, now.Add(-10*24*time.Hour))
	put(exosKey, f.exosPrice, now.Add(-2*time.Hour))
	put(exosKey, f.exosPrice, now.Add(-10*time.Minute))
	put(wdKey, 12999, now.Add(-10*24*time.Hour))
	put(wdKey, 12999, now.Add(-10*time.Minute))
	srv.offers = offers
	srv.surfaces["hdd"].Offers = offers
	srv.sourcePolicies = map[string]sourcePolicy{"ebay": {expireAfter: 90 * time.Minute}}
	srv.now = func() time.Time { return now }
	return f
}

type ndResult struct {
	AsOf                string      `json:"as_of"`
	Since               string      `json:"since"`
	NewCount            int         `json:"new_count"`
	HiddenExpired       int         `json:"hidden_expired"`
	CandidatesTruncated bool        `json:"candidates_truncated"`
	Items               []searchRow `json:"items"`
}

func newDeals(t *testing.T, srv *server, args string) (rpcEnvelope, mcpCallResult, ndResult) {
	t.Helper()
	env, res := callTool(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"new_deals","arguments":`+args+`}}`)
	var nd ndResult
	if env.Error == nil && !res.IsError {
		if err := json.Unmarshal(res.StructuredContent, &nd); err != nil {
			t.Fatalf("decode new_deals: %v (%s)", err, res.StructuredContent)
		}
	}
	return env, res, nd
}

func rowByID(rows []searchRow) map[string]searchRow {
	out := map[string]searchRow{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestNewDealsFirstSeenAndPriceDrop(t *testing.T) {
	f := offerTestServer(t)
	since := ts(f.now.Add(-3 * time.Hour))
	env, _, nd := newDeals(t, f.srv, `{"since":"`+since+`","min_verdict":"any"}`)
	if env.Error != nil {
		t.Fatalf("new_deals: %+v", env.Error)
	}
	got := rowByID(nd.Items)
	if len(got) != 2 || nd.NewCount != 2 {
		t.Fatalf("rows = %v, want the HGST (first seen) and the Exos (price drop)", nd.Items)
	}
	if r := got[f.hgst]; r.NewReason != reasonFirstSeen || r.OfferTiming == nil || r.FirstSeenAt != ts(f.now.Add(-time.Hour)) {
		t.Fatalf("hgst row = %+v", r)
	}
	r := got[f.exos]
	if r.NewReason != reasonPriceDrop || r.PriceChangedAt != ts(f.now.Add(-2*time.Hour)) ||
		r.PreviousPriceCents != f.exosOld || r.LowestPriceCents != f.exosPrice || r.AtLowestPrice == nil || !*r.AtLowestPrice {
		t.Fatalf("exos row = %+v", r)
	}
	if _, ok := got[f.wdRed]; ok {
		t.Fatal("an unchanged listing first seen 10 days ago is not new")
	}
	if nd.AsOf != ts(f.now) || nd.Since != since {
		t.Fatalf("as_of %q since %q", nd.AsOf, nd.Since)
	}
	// ranked best first, ranks renumbered over the new rows only
	if nd.Items[0].Rank != 1 || nd.Items[1].Rank != 2 || nd.Items[0].Score < nd.Items[1].Score {
		t.Fatalf("ranking: %+v", nd.Items)
	}

	// include_price_drops=false: only the newly listed drive.
	_, _, nd = newDeals(t, f.srv, `{"since":"`+since+`","min_verdict":"any","include_price_drops":false}`)
	if len(nd.Items) != 1 || nd.Items[0].ID != f.hgst {
		t.Fatalf("without price drops: %v", nd.Items)
	}
	// min_verdict great: only rows rated great.
	_, _, nd = newDeals(t, f.srv, `{"since":"`+since+`","min_verdict":"great"}`)
	for _, r := range nd.Items {
		if r.Verdict != "great" {
			t.Fatalf("min_verdict great returned %s (%s)", r.ID, r.Verdict)
		}
	}
	// limit 1
	_, _, nd = newDeals(t, f.srv, `{"since":"`+since+`","min_verdict":"any","limit":1}`)
	if len(nd.Items) != 1 || nd.NewCount != 2 {
		t.Fatalf("limit 1: %d rows, new_count %d", len(nd.Items), nd.NewCount)
	}
	// A price RISE is never "new".
	if err := f.offers.Put(context.Background(), offer.Offer{SourceID: "ebay", SourceKey: wdKey, PriceCents: 15999, LastSeen: f.now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	_, _, nd = newDeals(t, f.srv, `{"since":"`+since+`","min_verdict":"any"}`)
	if _, ok := rowByID(nd.Items)[f.wdRed]; ok {
		t.Fatal("a price rise was reported as new")
	}
}

func TestNewDealsSinceValidation(t *testing.T) {
	f := offerTestServer(t)
	cases := map[string]string{
		`{"since":"yesterday"}`:                                  "since must be an RFC 3339 time",
		`{"since":"2026-10-03"}`:                                 "since must be an RFC 3339 time",
		`{"since":"` + ts(f.now.Add(time.Hour)) + `"}`:           "since is in the future",
		`{"since":"` + ts(f.now.Add(-31*24*time.Hour)) + `"}`:    "more than 30 days ago",
		`{"since":"` + ts(f.now) + `","min_verdict":"amazing"}`:  "min_verdict must be one of",
		`{"since":"` + ts(f.now) + `","limit":0}`:                "limit must be between 1 and 100",
		`{"since":"` + ts(f.now) + `","limit":101}`:              "limit must be between 1 and 100",
		`{"since":"` + ts(f.now) + `","watch":"no-such-watch"}`:  "no active watch has that name",
		`{"since":"` + ts(f.now) + `","category":"no-such-cat"}`: "unknown category",
		`{"since":"` + ts(f.now) + `","surprise":true}`:          "invalid arguments",
		`{}`: "invalid arguments",
	}
	for args, want := range cases {
		env, _, _ := newDeals(t, f.srv, args)
		if env.Error == nil || !strings.Contains(env.Error.Message, want) {
			t.Errorf("%s -> %+v, want an invalid-argument error containing %q", args, env.Error, want)
		}
		if env.Error != nil && strings.Contains(env.Error.Message, "yesterday") {
			t.Errorf("error echoes caller input: %q", env.Error.Message)
		}
	}
	// Within the tolerated clock skew, and exactly 30 days, are accepted.
	for _, since := range []time.Time{f.now.Add(2 * time.Minute), f.now.Add(-30*24*time.Hour + time.Minute)} {
		if env, _, _ := newDeals(t, f.srv, `{"since":"`+ts(since)+`"}`); env.Error != nil {
			t.Errorf("since %s refused: %+v", ts(since), env.Error)
		}
	}
	// Fractional seconds and offsets are RFC 3339 too.
	if env, _, _ := newDeals(t, f.srv, `{"since":"`+f.now.Add(-time.Hour).In(time.FixedZone("x", -7*3600)).Format(time.RFC3339Nano)+`"}`); env.Error != nil {
		t.Errorf("an offset RFC 3339 time was refused: %+v", env.Error)
	}
}

func TestNewDealsNeedsTheOfferLayer(t *testing.T) {
	srv := newTestServer(t)
	env, _, _ := newDeals(t, srv, `{"since":"`+ts(time.Now().Add(-time.Hour))+`"}`)
	if env.Error == nil || !strings.Contains(env.Error.Message, "offer layer") {
		t.Fatalf("offer layer off -> %+v", env.Error)
	}
}

func TestNewDealsNeverReturnsExpired(t *testing.T) {
	f := offerTestServer(t)
	if _, err := f.offers.MarkExpired(context.Background(), "ebay", f.now.Add(-5*time.Minute), f.now); err != nil {
		t.Fatal(err)
	}
	_, _, nd := newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-3*time.Hour))+`","min_verdict":"any"}`)
	if len(nd.Items) != 0 || nd.HiddenExpired != 3 {
		t.Fatalf("after expiry: rows %v hidden %d, want none and 3", nd.Items, nd.HiddenExpired)
	}
}

type searchFailStore struct{ store.Store }

func (searchFailStore) Search(context.Context, store.Query) ([]item.Item, error) {
	return nil, errors.New("dial postgres://nagus:hunter2@db.internal: timeout")
}

// One failing category is reported and the others still answer; when every
// category fails it is the fixed internal error.
func TestNewDealsIsolatesAFailingCategory(t *testing.T) {
	f := offerTestServer(t)
	f.srv.surfaces["broken"] = &pipeline.Surface{Store: searchFailStore{}}
	since := ts(f.now.Add(-3 * time.Hour))
	env, res, nd := newDeals(t, f.srv, `{"since":"`+since+`","min_verdict":"any"}`)
	if env.Error != nil || len(nd.Items) != 2 {
		t.Fatalf("with one broken category: %+v, %d rows", env.Error, len(nd.Items))
	}
	if !strings.Contains(string(res.StructuredContent), `"failed_categories":["broken"]`) {
		t.Fatalf("failed category not reported: %s", res.StructuredContent)
	}
	env, _, _ = newDeals(t, f.srv, `{"since":"`+since+`","category":"broken"}`)
	if env.Error == nil || env.Error.Code != mcp.CodeInternalError || strings.Contains(env.Error.Message, "hunter2") {
		t.Fatalf("all categories failing -> %+v", env.Error)
	}
}

func TestNewDealsWithWatchAppliesStrongCriteria(t *testing.T) {
	f := offerTestServer(t)
	f.srv.watches = watch.Config{Watches: []watch.Watch{
		{Name: "great-only", Category: "hdd", StrongVerdicts: []string{"great"}},
		{Name: "gone", Category: "hdd", ExpiresAt: f.now.Add(-time.Hour)},
	}}
	since := ts(f.now.Add(-3 * time.Hour))
	env, _, nd := newDeals(t, f.srv, `{"since":"`+since+`","watch":"great-only"}`)
	if env.Error != nil {
		t.Fatal(env.Error)
	}
	for _, r := range nd.Items {
		if r.Verdict != "great" {
			t.Fatalf("watch great-only returned %s (%s)", r.ID, r.Verdict)
		}
	}
	if _, ok := rowByID(nd.Items)[f.hgst]; !ok {
		t.Fatalf("the new great drive is missing: %v", nd.Items)
	}
	// An expired watch is not usable.
	if env, _, _ := newDeals(t, f.srv, `{"since":"`+since+`","watch":"gone"}`); env.Error == nil {
		t.Fatal("an expired watch was accepted")
	}
}

// --- rows: search_items, get_item, /item, /watches ---------------------------

func TestSearchItemsHidesExpiredAndCarriesTiming(t *testing.T) {
	f := offerTestServer(t)
	// Expire only the WD (not seen since 10 days ago in this variant).
	if err := f.offers.Put(context.Background(), offer.Offer{
		SourceID: "ebay", SourceKey: wdKey, PriceCents: 12999,
		LastSeen: f.now.Add(-10 * time.Minute), Status: offer.StatusExpired, ExpiredAt: f.now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	_, res := callTool(t, f.srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_items","arguments":{}}}`)
	var sc struct {
		Matched       int         `json:"matched"`
		HiddenExpired int         `json:"hidden_expired"`
		Items         []searchRow `json:"items"`
	}
	if err := json.Unmarshal(res.StructuredContent, &sc); err != nil {
		t.Fatal(err)
	}
	got := rowByID(sc.Items)
	if _, ok := got[f.wdRed]; ok || sc.HiddenExpired != 1 || len(sc.Items) != 2 {
		t.Fatalf("search_items: rows %d hidden %d (WD present: %v)", len(sc.Items), sc.HiddenExpired, ok)
	}
	h := got[f.hgst]
	if h.OfferTiming == nil || h.OfferStatus != "active" || h.LastSeenAt != ts(f.now.Add(-10*time.Minute)) ||
		h.ExpiresBasis != basisNotSeen3Polls || h.ExpiresAt != ts(f.now.Add(80*time.Minute)) {
		t.Fatalf("hgst timing = %+v", h.OfferTiming)
	}
	// /search is the same projection.
	if rows := searchRows(t, f.srv, "/search?category=hdd"); len(rows) != 2 || rows[0].OfferTiming == nil {
		t.Fatalf("/search rows = %+v", rows)
	}

	// get_item still answers for the expired listing, marked.
	_, gi := callTool(t, f.srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"`+f.wdRed+`"}}}`)
	var item map[string]any
	if err := json.Unmarshal(gi.StructuredContent, &item); err != nil {
		t.Fatal(err)
	}
	if item["offer_status"] != "expired" || item["offer_note"] != expiredNote || item["expired_at"] != ts(f.now.Add(-time.Minute)) ||
		item["expires_at"] != nil || item["seen_at"] == nil || item["first_seen_at"] != ts(f.now.Add(-10*24*time.Hour)) {
		t.Fatalf("get_item of an expired listing = %v", item)
	}
	// /item too.
	rec := do(t, f.srv, http.MethodGet, "/item?id="+f.wdRed)
	var hitem map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &hitem); err != nil || hitem["offer_status"] != "expired" {
		t.Fatalf("/item = %s", rec.Body.String())
	}
}

// With the offer layer off (or no offer row) the fields are ABSENT, not zero.
func TestRowsOmitOfferFieldsWithoutAnOffer(t *testing.T) {
	srv := newTestServer(t)
	_, res := callTool(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_items","arguments":{}}}`)
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(res.StructuredContent, &raw); err != nil || len(raw.Items) == 0 {
		t.Fatal(err)
	}
	offerKeys := []string{
		"first_seen_at", "last_seen_at", "price_changed_at", "previous_price_cents", "lowest_price_cents",
		"at_lowest_price", "offer_status", "expired_at", "expires_at", "expires_basis", "offer_note", "new_reason",
	}
	for _, it := range raw.Items {
		for _, k := range offerKeys {
			if _, ok := it[k]; ok {
				t.Fatalf("row carries %q with the offer layer off: %v", k, it)
			}
		}
	}
	id := raw.Items[0]["id"].(string)
	_, gi := callTool(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"`+id+`"}}}`)
	var item map[string]any
	_ = json.Unmarshal(gi.StructuredContent, &item)
	for _, k := range offerKeys {
		if _, ok := item[k]; ok {
			t.Fatalf("get_item carries %q with the offer layer off", k)
		}
	}

	// Offer layer ON but this item has no offer row: absent as well.
	f := offerTestServer(t)
	f.offers = offer.NewMemoryStore() // empty
	f.srv.offers, f.srv.surfaces["hdd"].Offers = f.offers, f.offers
	_, res = callTool(t, f.srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_items","arguments":{}}}`)
	raw.Items = nil
	_ = json.Unmarshal(res.StructuredContent, &raw)
	if len(raw.Items) != 3 {
		t.Fatalf("items with no offer row must still surface: %d", len(raw.Items))
	}
	for _, it := range raw.Items {
		if _, ok := it["offer_status"]; ok {
			t.Fatalf("row with no offer carries offer fields: %v", it)
		}
	}
}

func TestTimingForBasis(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	srv := &server{sourcePolicies: map[string]sourcePolicy{
		"shopify:s":  {expireAfter: 3 * time.Hour},
		"ebay:e":     {expireAfter: 90 * time.Minute},
		"imap:deals": {expireAfter: 45 * time.Minute, dealLookback: 14 * 24 * time.Hour},
	}, now: func() time.Time { return now }}
	base := offer.Summary{Status: offer.StatusActive, FirstSeen: now.Add(-48 * time.Hour), LastSeen: now, PriceCents: 100, MinPriceSeen: 90}
	cases := []struct {
		name      string
		mod       func(*offer.Summary)
		at, basis string
	}{
		{"assumed", func(s *offer.Summary) { s.SourceID = "shopify:s" }, ts(now.Add(3 * time.Hour)), basisNotSeen3Polls},
		{"auction end", func(s *offer.Summary) { s.SourceID = "ebay:e"; s.EndsAt = now.Add(5 * time.Hour) }, ts(now.Add(5 * time.Hour)), basisListingEnd},
		{"ebay fixed price", func(s *offer.Summary) { s.SourceID = "ebay:e" }, ts(now.Add(90 * time.Minute)), basisNotSeen3Polls},
		{"deal with arrival", func(s *offer.Summary) { s.SourceID = "imap:deals"; s.EndsAt = now.Add(13 * 24 * time.Hour) }, ts(now.Add(13 * 24 * time.Hour)), basisEmailWindow},
		{"deal before ends_at", func(s *offer.Summary) { s.SourceID = "imap:deals" }, ts(now.Add(12 * 24 * time.Hour)), basisEmailWindow},
		{"unknown source", func(s *offer.Summary) { s.SourceID = "gone:x" }, "", ""},
		{"expired", func(s *offer.Summary) { s.SourceID = "shopify:s"; s.Status = offer.StatusExpired; s.ExpiredAt = now }, "", ""},
	}
	for _, c := range cases {
		sm := base
		c.mod(&sm)
		got := srv.timingFor(sm)
		if got.ExpiresAt != c.at || got.ExpiresBasis != c.basis {
			t.Errorf("%s: expires %q/%q, want %q/%q", c.name, got.ExpiresAt, got.ExpiresBasis, c.at, c.basis)
		}
		if got.AtLowestPrice == nil || *got.AtLowestPrice {
			t.Errorf("%s: at_lowest_price = %v, want false (100 > 90)", c.name, got.AtLowestPrice)
		}
	}
	if got := srv.timingFor(offer.Summary{Status: offer.StatusExpired, SourceID: "shopify:s", ExpiredAt: now}); got.OfferNote != expiredNote || got.ExpiredAt != ts(now) {
		t.Errorf("expired note: %+v", got)
	}
	// A stated end in the past: the row says it ENDED; one in the future says nothing.
	ended := base
	ended.SourceID, ended.EndsAt = "ebay:e", now.Add(-time.Minute)
	if got := srv.timingFor(ended); got.OfferNote != endedNote || got.ExpiresBasis != basisListingEnd {
		t.Errorf("ended auction: %+v", got)
	}
	running := base
	running.SourceID, running.EndsAt = "ebay:e", now.Add(time.Minute)
	if got := srv.timingFor(running); got.OfferNote != "" {
		t.Errorf("running auction carries a note: %+v", got)
	}
	// Unknown prices: no at_lowest_price at all.
	if got := srv.timingFor(offer.Summary{Status: offer.StatusActive, SourceID: "shopify:s"}); got.AtLowestPrice != nil || got.LowestPriceCents != 0 {
		t.Errorf("unknown price: %+v", got)
	}
}

func TestBuildSourcePolicies(t *testing.T) {
	srv := newTestServer(t) // one ebay ingester, SourceID "ebay"
	got := buildSourcePolicies([]SourceConfig{{Name: "x", Type: "ebay", IntervalMinutes: 30}}, srv.ingesters)
	if p := got["ebay"]; p.expireAfter != 90*time.Minute || p.dealLookback != 0 {
		t.Fatalf("ebay policy = %+v", p)
	}
}

// --- list_watches ------------------------------------------------------------

func TestListWatches(t *testing.T) {
	f := offerTestServer(t)
	f.srv.watches = watch.Config{Watches: []watch.Watch{
		{Name: "big-hdd", Category: "hdd", Audience: "household", Principal: "someone-private", Limit: 25},
		{Name: "wine-watch", Category: "wine", MinScore: 80},
		{Name: "lapsed", Category: "hdd", ExpiresAt: f.now.Add(-time.Hour)},
	}}
	_, res := callTool(t, f.srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_watches","arguments":{}}}`)
	if res.IsError {
		t.Fatalf("list_watches: %+v", res)
	}
	if strings.Contains(string(res.StructuredContent), "someone-private") || strings.Contains(string(res.StructuredContent), "principal") {
		t.Fatalf("list_watches leaks the principal: %s", res.StructuredContent)
	}
	var lw struct {
		Watches []watchInfo `json:"watches"`
	}
	if err := json.Unmarshal(res.StructuredContent, &lw); err != nil {
		t.Fatal(err)
	}
	if len(lw.Watches) != 2 {
		t.Fatalf("watches = %+v, want the two active ones", lw.Watches)
	}
	big := lw.Watches[0]
	if big.Name != "big-hdd" || big.Audience != "household" || big.CandidateCount == nil || *big.CandidateCount != 3 ||
		big.StrongCount == nil || !reflect.DeepEqual(big.StrongCriteria.Verdicts, []string{"great"}) {
		t.Fatalf("big-hdd = %+v", big)
	}
	if w := lw.Watches[1]; w.Error == "" || w.CandidateCount != nil || len(w.StrongCriteria.Verdicts) != 0 || w.StrongCriteria.MinScore != 80 {
		t.Fatalf("wine-watch (no wine surface) = %+v", w)
	}
	if strings.Contains(string(res.StructuredContent), `"items"`) || strings.Contains(string(res.StructuredContent), `"strong":`) {
		t.Fatalf("the list form must carry no matches: %s", res.StructuredContent)
	}

	// One watch by name: its strong matches as rows with offer fields.
	_, one := callTool(t, f.srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_watches","arguments":{"name":"big-hdd"}}}`)
	var detail struct {
		Watch  watchInfo   `json:"watch"`
		Strong []searchRow `json:"strong"`
	}
	if err := json.Unmarshal(one.StructuredContent, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Watch.Name != "big-hdd" || len(detail.Strong) != *detail.Watch.StrongCount || len(detail.Strong) == 0 ||
		detail.Strong[0].OfferTiming == nil || detail.Strong[0].Verdict != "great" {
		t.Fatalf("big-hdd detail = %+v", detail)
	}
	// Unknown and lapsed names are not found (and not echoed).
	for _, name := range []string{"nope", "lapsed"} {
		_, miss := callTool(t, f.srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_watches","arguments":{"name":"`+name+`"}}}`)
		if !miss.IsError || strings.Contains(miss.Content[0].Text, name) {
			t.Fatalf("list_watches %q = %+v", name, miss)
		}
	}
}

// /watches rows carry the offer fields and hide expired listings too.
func TestServeWatchesHidesExpired(t *testing.T) {
	f := offerTestServer(t)
	f.srv.watches = watch.Config{Watches: []watch.Watch{{Name: "all", Category: "hdd", MinScore: 0.0001}}}
	if _, err := f.offers.MarkExpired(context.Background(), "ebay", f.now.Add(-5*time.Minute), f.now); err != nil {
		t.Fatal(err)
	}
	rec := do(t, f.srv, http.MethodGet, "/watches")
	var body struct {
		Watches []struct {
			CandidateCount int `json:"candidate_count"`
		} `json:"watches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Watches) != 1 || body.Watches[0].CandidateCount != 0 {
		t.Fatalf("/watches after expiry = %s", rec.Body.String())
	}
}
