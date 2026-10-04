package main

// Offer timing on agent-facing rows (nagus-417; design
// docs/design/2026-10-03-new-deals-offer-timing.md). Every row a consumer
// sees -- search_items, /search, /watches, new_deals, list_watches, get_item,
// /item -- carries when nagus first and last saw the listing, when its price
// last changed, the lowest price seen, whether its offer is still active, and
// when it is expected to expire WITH THE BASIS of that expectation, so an
// assumption is never presented as a fact.
//
// The fields come from the listing's OFFER (item id == offer id). With the
// offer layer off, or for a row with no offer, they are absent -- never zero.

import (
	"context"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
)

// expires_basis values.
const (
	// basisListingEnd: the source states the end (an eBay auction's end time).
	basisListingEnd = "listing_end"
	// basisNotSeen3Polls: an ASSUMPTION -- last_seen + 3 poll intervals, when
	// nagus will mark the offer expired if no poll sees it again.
	basisNotSeen3Polls = "not_seen_3_polls"
	// basisEmailWindow: a deal submission, listed while its email is inside
	// the mailbox lookback window nagus reads. nagus's retention, not the
	// seller's end date.
	basisEmailWindow = "email_window"
)

// expiredNote is the fixed sentence an expired row carries (get_item, /item).
const expiredNote = "EXPIRED: nagus has not seen this listing at its source since last_seen_at. " +
	"It is kept as price history only; do not recommend it for purchase."

// endedNote is the fixed sentence an active row carries once its source-stated
// end (expires_at) has passed but no poll has expired it yet. Search and
// watches already hide such rows; get_item still answers.
const endedNote = "ENDED: the end time stated for this listing (expires_at) has passed. " +
	"Do not recommend it for purchase."

// sourcePolicy is what the read path needs to know about a source to state an
// expiry: how long an unseen offer lasts, and what kind of source it is.
type sourcePolicy struct {
	// expireAfter is when an unseen offer is marked expired (3 intervals).
	expireAfter time.Duration
	// dealLookback, when > 0, marks a deal-submission source and is its
	// mailbox lookback window.
	dealLookback time.Duration
}

// buildSourcePolicies derives each configured source's policy, keyed by the
// SourceID its offers carry. sources and ingesters are parallel (runServe
// builds one ingester per source, in order).
func buildSourcePolicies(sources []SourceConfig, ingesters []*pipeline.Ingester) map[string]sourcePolicy {
	out := make(map[string]sourcePolicy, len(sources))
	for i, src := range sources {
		if i >= len(ingesters) {
			break
		}
		_, _, expireAfter := retentionForSource(src)
		p := sourcePolicy{expireAfter: expireAfter}
		if isDealSource(src) {
			days := src.IMAPLookbackDays
			if days <= 0 {
				days = imapmail.DefaultLookbackDays
			}
			p.dealLookback = time.Duration(days) * 24 * time.Hour
		}
		out[ingesters[i].SourceID()] = p
	}
	return out
}

// OfferTiming is the set of offer fields a row carries. It is embedded by
// pointer in searchRow and itemView, so a nil pointer omits every field.
type OfferTiming struct {
	// FirstSeenAt is when nagus first saw this listing at this source.
	FirstSeenAt string `json:"first_seen_at,omitempty"`
	// LastSeenAt is the most recent poll that saw it.
	LastSeenAt string `json:"last_seen_at,omitempty"`
	// PriceChangedAt is when the price last changed (first_seen_at until it
	// does).
	PriceChangedAt string `json:"price_changed_at,omitempty"`
	// PreviousPriceCents is the price before that change; absent when the
	// price never changed or the earlier price was unknown.
	PreviousPriceCents int64 `json:"previous_price_cents,omitempty"`
	// LowestPriceCents is the lowest non-zero price ever seen for the listing.
	LowestPriceCents int64 `json:"lowest_price_cents,omitempty"`
	// AtLowestPrice: the current price equals LowestPriceCents.
	AtLowestPrice *bool `json:"at_lowest_price,omitempty"`
	// OfferStatus is "active" or "expired".
	OfferStatus string `json:"offer_status,omitempty"`
	// ExpiredAt is set only on an expired listing.
	ExpiredAt string `json:"expired_at,omitempty"`
	// ExpiresAt and ExpiresBasis: see the basis* constants.
	ExpiresAt    string `json:"expires_at,omitempty"`
	ExpiresBasis string `json:"expires_basis,omitempty"`
	// OfferNote is expiredNote on an expired listing.
	OfferNote string `json:"offer_note,omitempty"`
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// timingFor projects one offer summary onto the row fields.
func (s *server) timingFor(sm offer.Summary) *OfferTiming {
	t := &OfferTiming{
		FirstSeenAt:        rfc3339(sm.FirstSeen),
		LastSeenAt:         rfc3339(sm.LastSeen),
		PriceChangedAt:     rfc3339(sm.PriceChangedAt),
		PreviousPriceCents: sm.PreviousPriceCents,
		LowestPriceCents:   sm.MinPriceSeen,
		OfferStatus:        string(sm.Status),
	}
	if sm.MinPriceSeen > 0 && sm.PriceCents > 0 {
		at := sm.PriceCents <= sm.MinPriceSeen
		t.AtLowestPrice = &at
	}
	if sm.Status == offer.StatusExpired {
		t.ExpiredAt = rfc3339(sm.ExpiredAt)
		t.OfferNote = expiredNote
		return t
	}
	pol, known := s.sourcePolicies[sm.SourceID]
	switch {
	case known && pol.dealLookback > 0:
		end := sm.EndsAt
		if end.IsZero() && !sm.FirstSeen.IsZero() {
			// A deal recorded before ends_at existed: its first poll is the
			// closest nagus has to the arrival time.
			end = sm.FirstSeen.Add(pol.dealLookback)
		}
		if !end.IsZero() {
			t.ExpiresAt, t.ExpiresBasis = rfc3339(end), basisEmailWindow
			if !sm.EndsAt.IsZero() && !end.After(s.clock()) {
				t.OfferNote = endedNote
			}
		}
	case !sm.EndsAt.IsZero():
		t.ExpiresAt, t.ExpiresBasis = rfc3339(sm.EndsAt), basisListingEnd
		if !sm.EndsAt.After(s.clock()) {
			t.OfferNote = endedNote
		}
	case known && pol.expireAfter > 0 && !sm.LastSeen.IsZero():
		t.ExpiresAt, t.ExpiresBasis = rfc3339(sm.LastSeen.Add(pol.expireAfter)), basisNotSeen3Polls
	}
	return t
}

// rowsFor projects ranked results onto rows with their offer fields and
// quark identity. The surface attached each row's offer summary in its one
// batched read; rows without one (no offer row, or a surface without the
// offer layer) get a single batched lookup here.
func (s *server) rowsFor(ctx context.Context, items []pipeline.Scored) []searchRow {
	rows := scoredItemsToRows(items)
	sums := make(map[string]offer.Summary, len(items))
	for _, sc := range items {
		if sc.Offer != nil {
			sums[sc.Item.ID] = *sc.Offer
		}
	}
	return s.withOffers(ctx, rows, sums)
}

// withOffers stamps each row with its offer's fields: quark's product id and
// comparison key (offer.ComparisonKey: the key rows must share to be the same
// thing to compare on price -- for wine the product id plus the vintage when
// the vintage matters) and the timing fields. known holds summaries already
// read; the rest are fetched in ONE Summaries call. A read failure leaves
// those rows without offer fields: this is enrichment of rows the surface
// already checked, never a reason to fail the response.
func (s *server) withOffers(ctx context.Context, rows []searchRow, known map[string]offer.Summary) []searchRow {
	if s.offers == nil {
		return rows
	}
	var missing []string
	for _, r := range rows {
		if _, ok := known[r.ID]; !ok {
			missing = append(missing, r.ID)
		}
	}
	if len(missing) > 0 {
		if got, err := s.offers.Summaries(ctx, missing); err == nil {
			if known == nil {
				known = make(map[string]offer.Summary, len(got))
			}
			for id, sm := range got {
				known[id] = sm
			}
		}
	}
	for i := range rows {
		sm, ok := known[rows[i].ID]
		if !ok {
			continue
		}
		rows[i].OfferTiming = s.timingFor(sm)
		if sm.Resolution.State == offer.ResolutionResolved {
			rows[i].ProductID = sm.Resolution.ProductID
			rows[i].ComparisonKey, rows[i].VintageStatus = offer.ComparisonKey(
				sm.Resolution.ProductID, sm.Resolution.VintageMode, rows[i].Details["vintage"], rows[i].Details["nv"] == "true")
		}
	}
	return rows
}
