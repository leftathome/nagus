package main

// price_history (nagus-0ta; design docs/design/2026-10-03-offer-price-history.md):
// READ-ONLY per-product price timelines across sellers, from the offer layer's
// append-only offer_price_history. get_item carries a short recent_prices
// excerpt of the same data.
//
// Grouping is the one the row-returning tools already tell the agent to use:
// offer.ComparisonKey -- quark's product id, plus the vintage for a wine whose
// year matters -- and, within a key, one currency (prices in two currencies
// are never compared). Offers of the product that have NO comparison key (a
// vintage wine whose listing states no year) are counted, not compared.
//
// Every list in the answer is capped (offers read, product groups, sellers per
// group, points per seller) and each cap reports whether it cut anything.

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/leftathome/go-service-kit/mcp"

	"github.com/leftathome/nagus/internal/offer"
)

const (
	priceHistoryDefaultDays = 30
	priceHistoryMaxDays     = 365
	// priceHistoryMaxOffers bounds the offers read for one product (most
	// recently seen first). A product sold by more sellers than this is rare
	// in this corpus; offers_truncated says when it happens.
	priceHistoryMaxOffers = 50
	// priceHistoryMaxGroups bounds the comparison groups returned (a wine
	// product id spans its vintages).
	priceHistoryMaxGroups = 10
	// priceHistoryMaxSellers bounds the seller timelines per group.
	priceHistoryMaxSellers = 20
	// priceHistoryMaxPoints bounds the points per seller timeline (newest).
	priceHistoryMaxPoints = 30
	// priceHistoryReadPerOffer bounds the history rows READ per offer, which
	// is what dates the all-time low. History is one row per change, so an
	// offer that changed price more often than this within the retention
	// window is pathological; a low older than the rows read is reported
	// undated rather than guessed.
	priceHistoryReadPerOffer = 500
	// getItemRecentPrices is how many points get_item's recent_prices carries.
	getItemRecentPrices = 5
)

// priceHistoryArgs is price_history's argument object.
type priceHistoryArgs struct {
	ProductID string `json:"product_id"`
	ItemID    string `json:"item_id"`
	Days      *int   `json:"days"`
}

// priceHistorySchema is pinned by TestMCPToolSchemasUnchangedForOpenclaw: a
// change here is deliberate.
func priceHistorySchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"product_id": map[string]any{
				"type":        "string",
				"description": "A product_id or comparison_key from a row (search_items, new_deals, get_item, list_watches). Give this or item_id, not both.",
			},
			"item_id": map[string]any{
				"type":        "string",
				"description": "One listing's id: the history of its product across sellers when quark identified it, else of that listing alone.",
			},
			"days": map[string]any{
				"type": "integer", "minimum": 1, "maximum": priceHistoryMaxDays,
				"description": "Timeline window in days, default 30; also the N of price_days_ago_cents.",
			},
		},
		"additionalProperties": false,
	}
}

func (s *server) priceHistoryTool() mcp.Tool {
	return mcp.NewTool(mcp.ToolSpec{
		Name: "price_history",
		Description: "READ-ONLY price history of one product across sellers, from the price changes nagus recorded (one point per " +
			"CHANGE, not per poll; an unknown price is never a change). Give product_id (a row's product_id or comparison_key) or " +
			"item_id. Offers are grouped like rows should be: per comparison_key (for a wine whose vintage matters, one vintage) " +
			"and currency. Each group has current_lowest_price_cents over ACTIVE listings (expired ones are history only: never " +
			"recommend them), all_time_low_cents with all_time_low_at (absent when the low predates the recorded history), " +
			"price_days_ago_cents (the lowest price then) and change_vs_days_ago_cents (negative = cheaper now), and per-seller " +
			"timelines (each identified by item_id and source_id; get_item describes the listing): price_at_window_start_cents " +
			"plus the points inside the window, newest " + fmt.Sprint(priceHistoryMaxPoints) + " at most. A price nagus did not " +
			"record is absent, never guessed: a listing older than the history (or whose points aged out) has no points, and " +
			"only its current price from price_changed_at on is known. eBay listings keep at most 6 hours of history (eBay's " +
			"terms). Capped: " + fmt.Sprint(priceHistoryMaxOffers) + " offers read, " + fmt.Sprint(priceHistoryMaxGroups) +
			" groups, " + fmt.Sprint(priceHistoryMaxSellers) + " sellers per group; the *_truncated flags say when a cap cut. " +
			"Needs nagus's offer layer; otherwise it answers invalid argument.",
		InputSchema: priceHistorySchema(),
		Noun:        "product(s)",
		Note:        mcpUntrustedNote,
		Access:      mcp.ReadOnly,
	}, s.mcpPriceHistory)
}

// pricePointView is one timeline point.
type pricePointView struct {
	At         string `json:"at"`
	PriceCents int64  `json:"price_cents"`
}

// recentPriceView is one get_item recent_prices point.
type recentPriceView struct {
	At         string `json:"at"`
	PriceCents int64  `json:"price_cents"`
	Currency   string `json:"currency,omitempty"`
}

// sellerTimeline is one offer's history inside the window.
//
// It carries NO source free text. Offers at rest are ungated (the glovebox
// crossing happens at the point of use, offer.go), and a glovebox-refused or
// offer-only listing is still recorded as an offer, so its seller, title or
// any aspect could carry refused injection text. source_id (configuration)
// and item_id (a hash) identify the listing; get_item gives the gated item.
type sellerTimeline struct {
	ItemID      string `json:"item_id"`
	SourceID    string `json:"source_id"`
	OfferStatus string `json:"offer_status"`
	// PriceCents is the listing's current (last known) price.
	PriceCents     int64  `json:"price_cents,omitempty"`
	PriceChangedAt string `json:"price_changed_at,omitempty"`
	FirstSeenAt    string `json:"first_seen_at,omitempty"`
	LastSeenAt     string `json:"last_seen_at,omitempty"`
	// PriceAtWindowStartCents is the price in effect when the window opened;
	// absent when the listing was not on offer then or it is unknown.
	PriceAtWindowStartCents int64            `json:"price_at_window_start_cents,omitempty"`
	Points                  []pricePointView `json:"points"`
	PointsTruncated         bool             `json:"points_truncated"`
}

// productHistory is one comparison group's roll-up.
type productHistory struct {
	ProductID     string `json:"product_id,omitempty"`
	ComparisonKey string `json:"comparison_key,omitempty"`
	VintageStatus string `json:"vintage_status,omitempty"`
	Currency      string `json:"currency"`

	OfferCount       int `json:"offer_count"`
	ActiveOfferCount int `json:"active_offer_count"`

	CurrentLowestPriceCents int64  `json:"current_lowest_price_cents,omitempty"`
	CurrentLowestItemID     string `json:"current_lowest_item_id,omitempty"`
	AllTimeLowCents         int64  `json:"all_time_low_cents,omitempty"`
	AllTimeLowAt            string `json:"all_time_low_at,omitempty"`
	AllTimeLowItemID        string `json:"all_time_low_item_id,omitempty"`
	PriceDaysAgoCents       int64  `json:"price_days_ago_cents,omitempty"`
	ChangeVsDaysAgoCents    *int64 `json:"change_vs_days_ago_cents,omitempty"`
	// ChangeVsDaysAgoPct is the change as a percentage of the earlier price,
	// one decimal.
	ChangeVsDaysAgoPct *float64 `json:"change_vs_days_ago_pct,omitempty"`

	Sellers          []sellerTimeline `json:"sellers"`
	SellersTruncated bool             `json:"sellers_truncated"`
}

// historyOffer is one offer being rolled up, with its comparison identity.
type historyOffer struct {
	o             offer.Offer
	sm            offer.Summary
	key           string
	vintageStatus string
}

func (s *server) mcpPriceHistory(ctx context.Context, a priceHistoryArgs) (mcp.Result, error) {
	if s.offers == nil {
		return mcp.Result{}, mcp.InvalidArgument("price_history needs the offer layer, which is off on this nagus")
	}
	if (a.ProductID == "") == (a.ItemID == "") {
		return mcp.Result{}, mcp.InvalidArgument("give exactly one of product_id and item_id")
	}
	days := priceHistoryDefaultDays
	if a.Days != nil {
		if *a.Days < 1 || *a.Days > priceHistoryMaxDays {
			return mcp.Result{}, mcp.InvalidArgument("days must be between 1 and 365")
		}
		days = *a.Days
	}
	now := s.clock()
	from := now.Add(-time.Duration(days) * 24 * time.Hour)

	var candidates []offer.Offer
	productID, wantKey, scope := "", "", "product"
	offersTruncated := false
	// known holds comparison identities already computed (the item_id's own
	// offer), so no offer pays for its item read twice.
	known := map[string]historyOffer{}
	if a.ItemID != "" {
		o, ok, err := s.offers.Get(ctx, a.ItemID)
		if err != nil {
			return mcp.Result{}, err
		}
		if !ok {
			return mcp.NotFound(), nil
		}
		if o.Resolution.State == offer.ResolutionResolved && o.Resolution.ProductID != "" {
			productID = o.Resolution.ProductID
			ho := s.historyOffer(ctx, o)
			known[o.ID] = ho
			wantKey = ho.key
			if wantKey == "" {
				// The listing itself is not comparable (a vintage wine with
				// no stated year): its own history is all that is honest.
				scope, candidates = "listing", []offer.Offer{o}
			}
		} else {
			scope, candidates = "listing", []offer.Offer{o}
		}
	} else {
		productID = a.ProductID
		if i := strings.IndexByte(productID, '@'); i > 0 {
			wantKey, productID = a.ProductID, a.ProductID[:i]
		}
	}
	if scope == "product" {
		got, err := s.offers.Query(ctx, offer.Query{ProductID: productID, IncludeExpired: true, Limit: priceHistoryMaxOffers + 1})
		if err != nil {
			return mcp.Result{}, err
		}
		if len(got) > priceHistoryMaxOffers {
			got, offersTruncated = got[:priceHistoryMaxOffers], true
		}
		candidates = got
	}

	// Comparison identity and grouping.
	type groupKey struct{ key, currency string }
	groups := map[groupKey][]historyOffer{}
	notComparable := 0
	ids := make([]string, 0, len(candidates))
	for _, o := range candidates {
		ho, ok := known[o.ID]
		if !ok {
			ho = s.historyOffer(ctx, o)
		}
		if scope == "product" {
			if ho.key == "" {
				notComparable++
				continue
			}
			if wantKey != "" && ho.key != wantKey {
				continue
			}
		}
		gk := groupKey{ho.key, currencyCode(o.Currency)}
		groups[gk] = append(groups[gk], ho)
		ids = append(ids, o.ID)
	}
	if len(groups) == 0 && notComparable == 0 {
		// Nothing nagus holds is this product (or comparison key).
		return mcp.NotFound(), nil
	}
	hist, err := s.offers.PriceHistory(ctx, ids, time.Time{}, priceHistoryReadPerOffer)
	if err != nil {
		return mcp.Result{}, err
	}
	for _, g := range groups {
		for _, ho := range g {
			hist[ho.o.ID] = s.servableHistory(ho.o.SourceID, hist[ho.o.ID], now)
		}
	}

	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].key != keys[j].key {
			return keys[i].key < keys[j].key
		}
		return keys[i].currency < keys[j].currency
	})
	groupsTruncated := len(keys) > priceHistoryMaxGroups
	if groupsTruncated {
		keys = keys[:priceHistoryMaxGroups]
	}
	products := make([]productHistory, 0, len(keys))
	for _, k := range keys {
		products = append(products, rollUp(groups[k], hist, now, from, k.currency))
	}
	// comparison_key is null unless a key was asked for or derived AND matched
	// a stored offer: the value served is then the stored key, never bare
	// caller input. product_id likewise is only served once offers matched it.
	var echoKey any
	if wantKey != "" && len(groups) > 0 {
		echoKey = wantKey
	}
	return mcp.Structured(len(products), map[string]any{
		"as_of":                 rfc3339(now),
		"days":                  days,
		"window_start":          rfc3339(from),
		"scope":                 scope,
		"product_id":            productID,
		"comparison_key":        echoKey,
		"products":              products,
		"groups_truncated":      groupsTruncated,
		"offers_truncated":      offersTruncated,
		"not_comparable_offers": notComparable,
	}), nil
}

// historyOffer computes an offer's comparison identity. A wine's key needs the
// listing's extracted vintage, which lives on the ITEM (item id == offer id);
// only offers whose product has a vintage mode pay for that read.
func (s *server) historyOffer(ctx context.Context, o offer.Offer) historyOffer {
	ho := historyOffer{o: o, sm: o.Summarize()}
	if o.Resolution.State != offer.ResolutionResolved || o.Resolution.ProductID == "" {
		return ho
	}
	vintage, nv := "", false
	if o.Resolution.VintageMode != "" && s.store != nil {
		if it, ok, err := s.store.Get(ctx, o.ID); err == nil && ok {
			vintage, nv = it.Attributes["vintage"], it.Attributes["nv"] == "true"
		}
	}
	ho.key, ho.vintageStatus = offer.ComparisonKey(o.Resolution.ProductID, o.Resolution.VintageMode, vintage, nv)
	return ho
}

// purchasableAt mirrors the surface's hiding rule: active, and any
// source-stated end still ahead.
func purchasableAt(sm offer.Summary, now time.Time) bool {
	return sm.Status == offer.StatusActive && (sm.EndsAt.IsZero() || sm.EndsAt.After(now))
}

// rollUp builds one comparison group's answer.
func rollUp(offers []historyOffer, hist map[string][]offer.PricePoint, now, from time.Time, currency string) productHistory {
	ph := productHistory{Currency: currency, OfferCount: len(offers), Sellers: []sellerTimeline{}}
	if len(offers) > 0 {
		first := offers[0]
		ph.ComparisonKey, ph.VintageStatus = first.key, first.vintageStatus
		if first.o.Resolution.State == offer.ResolutionResolved {
			ph.ProductID = first.o.Resolution.ProductID
		}
	}
	var lowAt time.Time
	var then int64
	for _, ho := range offers {
		sm, h := ho.sm, hist[ho.o.ID]
		if purchasableAt(sm, now) {
			ph.ActiveOfferCount++
			if sm.PriceCents > 0 && (ph.CurrentLowestPriceCents == 0 || sm.PriceCents < ph.CurrentLowestPriceCents ||
				(sm.PriceCents == ph.CurrentLowestPriceCents && sm.ID < ph.CurrentLowestItemID)) {
				ph.CurrentLowestPriceCents, ph.CurrentLowestItemID = sm.PriceCents, sm.ID
			}
		}
		// All-time low: the lowest price this offer was ever seen at; dated
		// by the earliest recorded point at that price.
		if low := sm.MinPriceSeen; low > 0 && (ph.AllTimeLowCents == 0 || low <= ph.AllTimeLowCents) {
			at := time.Time{}
			for _, p := range h {
				if p.PriceCents == low {
					at = p.ObservedAt
					break
				}
			}
			better := ph.AllTimeLowCents == 0 || low < ph.AllTimeLowCents ||
				(!at.IsZero() && (lowAt.IsZero() || at.Before(lowAt)))
			if better {
				ph.AllTimeLowCents, lowAt, ph.AllTimeLowItemID = low, at, sm.ID
			}
		}
		if p, ok := offer.PriceAt(sm, h, from); ok && (then == 0 || p < then) {
			then = p
		}
	}
	ph.AllTimeLowAt = rfc3339(lowAt)
	if then > 0 {
		ph.PriceDaysAgoCents = then
		if ph.CurrentLowestPriceCents > 0 {
			d := ph.CurrentLowestPriceCents - then
			pct := math.Round(float64(d)*1000/float64(then)) / 10
			ph.ChangeVsDaysAgoCents, ph.ChangeVsDaysAgoPct = &d, &pct
		}
	}

	// Seller timelines: listings present at some point in the window, active
	// ones first, then by current price.
	var inWindow []historyOffer
	for _, ho := range offers {
		if !ho.sm.LastSeen.Before(from) {
			inWindow = append(inWindow, ho)
		}
	}
	sort.SliceStable(inWindow, func(i, j int) bool {
		a, b := inWindow[i].sm, inWindow[j].sm
		pa, pb := purchasableAt(a, now), purchasableAt(b, now)
		if pa != pb {
			return pa
		}
		if a.PriceCents != b.PriceCents {
			return a.PriceCents < b.PriceCents
		}
		return a.ID < b.ID
	})
	if len(inWindow) > priceHistoryMaxSellers {
		inWindow, ph.SellersTruncated = inWindow[:priceHistoryMaxSellers], true
	}
	for _, ho := range inWindow {
		sm := ho.sm
		st := sellerTimeline{
			ItemID: sm.ID, SourceID: sm.SourceID, OfferStatus: string(sm.Status),
			PriceCents: sm.PriceCents, PriceChangedAt: rfc3339(sm.PriceChangedAt),
			FirstSeenAt: rfc3339(sm.FirstSeen), LastSeenAt: rfc3339(sm.LastSeen),
			Points: []pricePointView{},
		}
		if p, ok := offer.PriceAt(sm, hist[sm.ID], from); ok {
			st.PriceAtWindowStartCents = p
		}
		var pts []offer.PricePoint
		for _, p := range hist[sm.ID] {
			if !p.ObservedAt.Before(from) && !p.ObservedAt.After(now) {
				pts = append(pts, p)
			}
		}
		if len(pts) > priceHistoryMaxPoints {
			pts, st.PointsTruncated = pts[len(pts)-priceHistoryMaxPoints:], true
		}
		for _, p := range pts {
			st.Points = append(st.Points, pricePointView{At: rfc3339(p.ObservedAt), PriceCents: p.PriceCents})
		}
		ph.Sellers = append(ph.Sellers, st)
	}
	return ph
}

// recentPrices is get_item's short excerpt of one listing's price history:
// the newest getItemRecentPrices points, oldest first. A read failure leaves
// it out (logged): it is enrichment, unlike the offer status get_item must
// not lose.
func (s *server) recentPrices(ctx context.Context, sm offer.Summary) []recentPriceView {
	got, err := s.offers.PriceHistory(ctx, []string{sm.ID}, time.Time{}, getItemRecentPrices)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  get_item: price history of %s: %v\n", sm.ID, err)
		return nil
	}
	pts := s.servableHistory(sm.SourceID, got[sm.ID], s.clock())
	if len(pts) == 0 {
		return nil
	}
	out := make([]recentPriceView, 0, len(pts))
	for _, p := range pts {
		out = append(out, recentPriceView{At: rfc3339(p.ObservedAt), PriceCents: p.PriceCents, Currency: currencyCode(p.Currency)})
	}
	return out
}

// servableHistory drops the points a source's terms forbid serving: for a
// source with a content-age bound (eBay, License 8.1(b): historyMaxAge), any
// point older than it. Retention deletes those on its next pass; until then
// (and for an anchor Put wrote with an old stamp) the read path must not
// serve them.
func (s *server) servableHistory(sourceID string, pts []offer.PricePoint, now time.Time) []offer.PricePoint {
	maxAge := s.sourcePolicies[sourceID].historyMaxAge
	if maxAge <= 0 {
		return pts
	}
	cut := now.Add(-maxAge)
	i := sort.Search(len(pts), func(i int) bool { return !pts[i].ObservedAt.Before(cut) })
	return pts[i:]
}

// currencyCode passes a stored currency only when it has the shape of an ISO
// 4217 code (three ASCII capitals). The value comes from the source and the
// offer row is ungated, so anything else is withheld rather than served.
func currencyCode(c string) string {
	if len(c) != 3 {
		return ""
	}
	for i := 0; i < 3; i++ {
		if c[i] < 'A' || c[i] > 'Z' {
			return ""
		}
	}
	return c
}
