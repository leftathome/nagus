package offer

// Price history (nagus-0ta; design docs/design/2026-10-03-offer-price-history.md).
//
// Every adapter keeps an APPEND-ONLY table of price points per offer,
// offer_price_history (offer_id, observed_at_ns, price_cents, currency), written
// by Put in the SAME transaction as the offer upsert. It holds one row per price
// CHANGE, never one per poll, and "change" is exactly the nagus-417 rule that
// stamps Offer.PriceChangedAt -- so the history and the offer row cannot
// disagree about when the price moved. HistoryRows is that rule, shared by every
// adapter.

import (
	"sort"
	"time"
)

// PricePoint is one row of an offer's price history: from ObservedAt on, the
// offer's price was PriceCents in Currency (until the next point).
type PricePoint struct {
	ObservedAt time.Time
	PriceCents int64
	Currency   string
}

// DefaultPriceHistoryWindow is how long price history is kept for a source
// whose offers are retained in full (Retention.HistoryWindow), when the
// operator configures nothing: a year, enough to see seasonal sales.
const DefaultPriceHistoryWindow = 365 * 24 * time.Hour

// PriorPrice is what an adapter knew about an offer's price BEFORE a Put: the
// stored row's price fields, and whether any history row exists for it.
type PriorPrice struct {
	// Exists is false for a first insert; the other fields are then unused.
	Exists bool
	// PriceCents and Currency are the stored (last known) price.
	PriceCents int64
	Currency   string
	// ChangedAt is the stored price_changed_at, a pre-column 0 read as
	// first_seen (what every read path does).
	ChangedAt time.Time
	// HasHistory reports whether the offer already has a history row. Only
	// consulted for a change, so an adapter may leave it false otherwise.
	HasHistory bool
}

// HistoryNeedsProbe reports whether HistoryRows will consult
// prior.HasHistory for this Put, so an adapter only queries the history table
// when the answer matters (a real change of a known price).
func HistoryNeedsProbe(prior PriorPrice, incomingPrice int64) bool {
	return prior.Exists && prior.PriceCents > 0 && incomingPrice > 0 && incomingPrice != prior.PriceCents
}

// HistoryRows decides the price-history rows one Put writes, given the stored
// price before it (prior), the incoming price and currency, when the incoming
// poll observed it (observedAt, the caller's LastSeen), and the offer's
// price_changed_at AFTER the fold (changedAt). Rows come oldest first.
//
// The rule mirrors Store.Put's price-change rule exactly:
//   - first insert with a known price: ONE row, the first sighting, at
//     changedAt (= FirstSeen). It gives every timeline a start point.
//   - stored price unknown (0), incoming known: ONE row, the first known
//     price, at observedAt. Not a change (price_changed_at stays), but it is
//     where the series starts.
//   - incoming price 0 (unknown) or equal to the stored one: no row.
//   - both known and different: ONE row at changedAt with the new price.
//     When the offer has NO history yet (it predates the table, or its rows
//     were purged by the history window), an ANCHOR row for the stored price
//     at its stored ChangedAt is written first, so the price the change left
//     is not lost. That is the lazy seeding: at most one extra row per offer,
//     and only for offers whose price actually moves.
//
// A row whose observed_at collides with an existing row of the same offer
// REPLACES it (an out-of-order poll can land on the same stamp; the stored
// price is the one the offer row now holds), except an anchor, which never
// overwrites.
func HistoryRows(prior PriorPrice, incomingPrice int64, incomingCurrency string, observedAt, changedAt time.Time) (anchor *PricePoint, row *PricePoint) {
	if incomingPrice <= 0 {
		return nil, nil
	}
	switch {
	case !prior.Exists:
		return nil, &PricePoint{ObservedAt: changedAt, PriceCents: incomingPrice, Currency: incomingCurrency}
	case prior.PriceCents <= 0:
		return nil, &PricePoint{ObservedAt: observedAt, PriceCents: incomingPrice, Currency: incomingCurrency}
	case incomingPrice == prior.PriceCents:
		return nil, nil
	}
	if !prior.HasHistory && !prior.ChangedAt.IsZero() {
		anchor = &PricePoint{ObservedAt: prior.ChangedAt, PriceCents: prior.PriceCents, Currency: prior.Currency}
	}
	return anchor, &PricePoint{ObservedAt: changedAt, PriceCents: incomingPrice, Currency: incomingCurrency}
}

// HistoryCutoff is the observed_at before which a retention pass deletes a
// source's price history, and whether it deletes any at all.
//
//   - Purge (eBay, License 8.1(b)): the OFFER window. History is eBay Content
//     like the offer itself, so no row outlives the content age, whether or
//     not its offer is still live; and every row of a purged offer is older
//     than the offer's last_seen, so it goes with the offer.
//   - RetainFull: HistoryWindow, when positive; 0 keeps history as long as
//     the offer. Unlike Purge, the window KEEPS each offer's CARRY-IN point:
//     its newest row at or before the cutoff, the price still in effect when
//     the window opens. A price that stood still for longer than the window
//     keeps its start (at most one extra row per offer). See
//     Retention.KeepsCarryIn.
func (r Retention) HistoryCutoff(now time.Time) (time.Time, bool) {
	switch r.Policy {
	case Purge:
		return now.Add(-r.Window), true
	case RetainFull:
		if r.HistoryWindow > 0 {
			return now.Add(-r.HistoryWindow), true
		}
	}
	return time.Time{}, false
}

// KeepsCarryIn reports whether the history window keeps each offer's newest
// row at or before the cutoff (RetainFull) rather than deleting every older
// row (Purge: a licence content-age bound allows nothing older to survive).
func (r Retention) KeepsCarryIn() bool { return r.Policy == RetainFull }

// PriceAt is the price an offer had at time t, from its history (oldest
// first) and its stored row, and whether it is known. It is best-effort and
// says so by returning false rather than guessing:
//   - before the offer was first seen, or after it was marked expired (an
//     expired offer was not on offer), there is no price;
//   - otherwise the latest history point at or before t;
//   - with NO history at all (an offer older than the table that never
//     changed since, or whose rows were purged), the stored row: PriceCents
//     from PriceChangedAt on, and UNKNOWN before it. PreviousPriceCents is
//     deliberately not used: it is the price the LAST change left, which held
//     only since the change before that -- an unknown time;
//   - before the earliest history point, unknown.
func PriceAt(o Summary, hist []PricePoint, t time.Time) (int64, bool) {
	if o.FirstSeen.IsZero() || t.Before(o.FirstSeen) {
		return 0, false
	}
	if o.Status == StatusExpired && !o.ExpiredAt.IsZero() && !t.Before(o.ExpiredAt) {
		return 0, false
	}
	if len(hist) == 0 {
		if o.PriceCents > 0 && !o.PriceChangedAt.IsZero() && !t.Before(o.PriceChangedAt) {
			return o.PriceCents, true
		}
		return 0, false
	}
	i := sort.Search(len(hist), func(i int) bool { return hist[i].ObservedAt.After(t) })
	if i == 0 {
		return 0, false
	}
	return hist[i-1].PriceCents, true
}
