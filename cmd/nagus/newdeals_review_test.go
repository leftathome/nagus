package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/watch"
)

// Review findings on !37 (nagus-417).

// M1: a listing first seen shortly before a call, then dropping in price,
// must still be reported as a drop on the next call -- and the dedupe key the
// skill uses, (id, price_changed_at), must differ between the two answers.
func TestNewDealsReportsADropOnAListingAlreadyShownAsNew(t *testing.T) {
	f := offerTestServer(t)
	ctx := context.Background()

	// Call N at f.now: the HGST (first seen 1h ago) is new.
	_, _, nd := newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-3*time.Hour))+`","min_verdict":"any"}`)
	first := rowByID(nd.Items)[f.hgst]
	if !slices.Equal(first.NewReasons, []string{reasonFirstSeen}) || nd.AsOf != ts(f.now) {
		t.Fatalf("call N: hgst = %+v (as_of %s)", first, nd.AsOf)
	}

	// Its price drops 5 minutes later.
	if err := f.offers.Put(ctx, offer.Offer{SourceID: "ebay", SourceKey: hgstKey, PriceCents: 7000, LastSeen: f.now.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	// Call N+1 ten minutes later, since = previous as_of - 1h (the skill).
	later := f.now.Add(10 * time.Minute)
	f.srv.now = func() time.Time { return later }
	_, _, nd = newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-time.Hour))+`","min_verdict":"any"}`)
	again, ok := rowByID(nd.Items)[f.hgst]
	if !ok || !slices.Equal(again.NewReasons, []string{reasonFirstSeen, reasonPriceDrop}) || again.NewReason != reasonPriceDrop {
		t.Fatalf("call N+1: hgst = %+v, want new_reasons [first_seen price_drop], new_reason price_drop", again)
	}
	if again.PreviousPriceCents != 8950 || again.PriceChangedAt != ts(f.now.Add(5*time.Minute)) {
		t.Fatalf("call N+1: the drop is not described: %+v", again.OfferTiming)
	}
	// The skill's dedupe key changed, so the agent shows it again.
	if first.ID+"|"+first.PriceChangedAt == again.ID+"|"+again.PriceChangedAt {
		t.Fatal("(id, price_changed_at) did not change with the drop")
	}

	// A caller whose since is after the first sighting but before the drop
	// sees only the drop.
	_, _, nd = newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(time.Minute))+`","min_verdict":"any"}`)
	if r := rowByID(nd.Items)[f.hgst]; !slices.Equal(r.NewReasons, []string{reasonPriceDrop}) {
		t.Fatalf("since between sighting and drop: %+v", r)
	}
}

// L2: a drop BEFORE since is not new.
func TestNewDealsIgnoresDropsBeforeSince(t *testing.T) {
	f := offerTestServer(t) // the Exos dropped 2h ago
	_, _, nd := newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-time.Hour))+`","min_verdict":"any"}`)
	if _, ok := rowByID(nd.Items)[f.exos]; ok {
		t.Fatalf("a drop 2h ago was reported for since = 1h ago: %v", nd.Items)
	}
	_, _, nd = newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-3*time.Hour))+`","min_verdict":"any"}`)
	if r, ok := rowByID(nd.Items)[f.exos]; !ok || r.NewReason != reasonPriceDrop {
		t.Fatalf("the same drop with since = 3h ago is missing: %v", nd.Items)
	}
}

// L3: a drop after a rise is a drop from the previous price, and the row says
// it is NOT a new low.
func TestNewDealsDropAfterRiseIsNotANewLow(t *testing.T) {
	f := offerTestServer(t) // WD: 12999 since 10 days ago
	ctx := context.Background()
	for _, p := range []struct {
		price int64
		at    time.Duration
	}{{15999, -30 * time.Minute}, {13999, -20 * time.Minute}} {
		if err := f.offers.Put(ctx, offer.Offer{SourceID: "ebay", SourceKey: wdKey, PriceCents: p.price, LastSeen: f.now.Add(p.at)}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, nd := newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-time.Hour))+`","min_verdict":"any"}`)
	r, ok := rowByID(nd.Items)[f.wdRed]
	if !ok || r.NewReason != reasonPriceDrop || r.PreviousPriceCents != 15999 || r.LowestPriceCents != 12999 ||
		r.AtLowestPrice == nil || *r.AtLowestPrice {
		t.Fatalf("12999 -> 15999 -> 13999: %+v", r)
	}
}

// L4: with a watch and no explicit min_verdict, no verdict floor is reported
// as applied.
func TestNewDealsReportsWhatWasApplied(t *testing.T) {
	f := offerTestServer(t)
	f.srv.watches = watch.Config{Watches: []watch.Watch{{Name: "great-only", Category: "hdd", StrongVerdicts: []string{"great"}}}}
	since := ts(f.now.Add(-3 * time.Hour))
	_, res, _ := newDeals(t, f.srv, `{"since":"`+since+`","watch":"great-only"}`)
	var sc map[string]any
	if err := json.Unmarshal(res.StructuredContent, &sc); err != nil {
		t.Fatal(err)
	}
	if v, ok := sc["min_verdict"]; !ok || v != nil || sc["watch"] != "great-only" {
		t.Fatalf("watch without min_verdict: min_verdict=%v watch=%v", sc["min_verdict"], sc["watch"])
	}
	_, res, _ = newDeals(t, f.srv, `{"since":"`+since+`","watch":"great-only","min_verdict":"great"}`)
	_ = json.Unmarshal(res.StructuredContent, &sc)
	if sc["min_verdict"] != "great" {
		t.Fatalf("explicit min_verdict not reported: %v", sc["min_verdict"])
	}
	_, res, _ = newDeals(t, f.srv, `{"since":"`+since+`"}`)
	_ = json.Unmarshal(res.StructuredContent, &sc)
	if w, ok := sc["watch"]; sc["min_verdict"] != "good" || !ok || w != nil {
		t.Fatalf("without a watch: min_verdict %v, watch %v (present %v); want good and null", sc["min_verdict"], w, ok)
	}
}

// The skill's dedupe key: id + price_changed_at only when new_reasons holds
// price_drop, else id. A listing shown as new whose price then RISES comes
// back first_seen-only (same key: not shown again); one whose price DROPS
// comes back with price_drop (new key: shown again).
func TestNewDealsDedupeKeySurvivesARise(t *testing.T) {
	f := offerTestServer(t)
	ctx := context.Background()
	key := func(r searchRow) string {
		if slices.Contains(r.NewReasons, reasonPriceDrop) {
			return r.ID + "@" + r.PriceChangedAt
		}
		return r.ID
	}
	_, _, nd := newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-3*time.Hour))+`","min_verdict":"any"}`)
	shown := key(rowByID(nd.Items)[f.hgst])

	// The price rises 5 minutes later.
	if err := f.offers.Put(ctx, offer.Offer{SourceID: "ebay", SourceKey: hgstKey, PriceCents: 9900, LastSeen: f.now.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	f.srv.now = func() time.Time { return f.now.Add(10 * time.Minute) }
	_, _, nd = newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-time.Hour))+`","min_verdict":"any"}`)
	r := rowByID(nd.Items)[f.hgst]
	if !slices.Equal(r.NewReasons, []string{reasonFirstSeen}) || key(r) != shown {
		t.Fatalf("after a rise: reasons %v key %q, want first_seen only and the shown key %q", r.NewReasons, key(r), shown)
	}

	// Then it drops below the risen price: a new key.
	if err := f.offers.Put(ctx, offer.Offer{SourceID: "ebay", SourceKey: hgstKey, PriceCents: 7900, LastSeen: f.now.Add(15 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	f.srv.now = func() time.Time { return f.now.Add(20 * time.Minute) }
	_, _, nd = newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-time.Hour))+`","min_verdict":"any"}`)
	if r = rowByID(nd.Items)[f.hgst]; !slices.Contains(r.NewReasons, reasonPriceDrop) || key(r) == shown {
		t.Fatalf("after a drop: reasons %v key %q", r.NewReasons, key(r))
	}
}

// M2: list_watches with a name is capped (default 25, optional limit) even
// for a watch with no limit of its own, and reports the total.
func TestListWatchesNameIsCapped(t *testing.T) {
	f := offerTestServer(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("extra-%02d", i)
		it := item.Item{ID: offer.DeterministicID("ebay", key), Category: "hdd", Class: item.ClassDurable,
			Title: "Example 16TB drive " + key, PriceCents: int64(5000 + i), Currency: "USD", Condition: "used",
			SourceID: "ebay", SourceKey: key, SeenAt: f.now, Attributes: map[string]string{"capacity_tb": "16"}}
		if err := f.srv.store.Put(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	f.srv.watches = watch.Config{Watches: []watch.Watch{{Name: "everything", Category: "hdd",
		StrongVerdicts: []string{"great", "good", "market", "poor", "unknown-no-reference", "unknown-no-price"}}}}
	call := func(args string) (watchInfo, []searchRow, int) {
		t.Helper()
		_, res := callTool(t, f.srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_watches","arguments":`+args+`}}`)
		var d struct {
			Watch          watchInfo   `json:"watch"`
			Strong         []searchRow `json:"strong"`
			StrongReturned int         `json:"strong_returned"`
		}
		if err := json.Unmarshal(res.StructuredContent, &d); err != nil {
			t.Fatalf("%s: %v %s", args, err, res.StructuredContent)
		}
		return d.Watch, d.Strong, d.StrongReturned
	}
	w, rows, n := call(`{"name":"everything"}`)
	if w.StrongCount == nil || *w.StrongCount != 33 || len(rows) != 25 || n != 25 {
		t.Fatalf("default: strong_count %v, %d rows, strong_returned %d; want 33 / 25 / 25", w.StrongCount, len(rows), n)
	}
	if _, rows, _ = call(`{"name":"everything","limit":3}`); len(rows) != 3 || rows[0].Rank != 1 {
		t.Fatalf("limit 3: %d rows", len(rows))
	}
	for _, bad := range []string{`{"name":"everything","limit":0}`, `{"name":"everything","limit":101}`} {
		env, _ := callTool(t, f.srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_watches","arguments":`+bad+`}}`)
		if env.Error == nil || !strings.Contains(env.Error.Message, "limit must be between 1 and 100") {
			t.Fatalf("%s -> %+v", bad, env.Error)
		}
	}
	// limit means nothing without name: rejected, not silently ignored.
	env, _ := callTool(t, f.srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_watches","arguments":{"limit":5}}}`)
	if env.Error == nil || !strings.Contains(env.Error.Message, "limit applies only with name") {
		t.Fatalf("limit without name -> %+v", env.Error)
	}
}

// L5: an auction whose stated end has passed is hidden from search and
// new_deals before any poll expires it; get_item says it ENDED.
func TestEndedAuctionIsHiddenAndMarked(t *testing.T) {
	f := offerTestServer(t)
	if err := f.offers.Put(context.Background(), offer.Offer{SourceID: "ebay", SourceKey: hgstKey, PriceCents: 8950,
		LastSeen: f.now.Add(-5 * time.Minute), EndsAt: f.now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	_, res := callTool(t, f.srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_items","arguments":{}}}`)
	var sc struct {
		HiddenExpired int         `json:"hidden_expired"`
		Items         []searchRow `json:"items"`
	}
	_ = json.Unmarshal(res.StructuredContent, &sc)
	if _, ok := rowByID(sc.Items)[f.hgst]; ok || sc.HiddenExpired != 1 {
		t.Fatalf("ended auction surfaced (hidden %d)", sc.HiddenExpired)
	}
	_, _, nd := newDeals(t, f.srv, `{"since":"`+ts(f.now.Add(-3*time.Hour))+`","min_verdict":"any"}`)
	if _, ok := rowByID(nd.Items)[f.hgst]; ok {
		t.Fatal("ended auction reported as a new deal")
	}
	_, gi := callTool(t, f.srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"`+f.hgst+`"}}}`)
	var it map[string]any
	_ = json.Unmarshal(gi.StructuredContent, &it)
	if it["offer_note"] != endedNote || it["expires_basis"] != basisListingEnd {
		t.Fatalf("get_item of an ended auction = %v", it)
	}
}
