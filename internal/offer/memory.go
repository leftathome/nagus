package offer

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the in-process reference implementation of Store. It is the
// contract a persistent adapter must satisfy: the same tests run against both.
type MemoryStore struct {
	mu     sync.RWMutex
	offers map[string]Offer
	// history is each offer's price history, oldest first, at most one point
	// per observed_at (offer_price_history's primary key).
	history map[string][]PricePoint
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{offers: map[string]Offer{}, history: map[string][]PricePoint{}}
}

// Put inserts or updates an offer, folding lifecycle against any existing row.
//
// The folding is the whole point of the layer, so it is spelled out:
//   - FirstSeen is preserved from the existing row. A re-observation must not
//     reset how long we have known about an offer.
//   - LastSeen only ever advances, so an out-of-order write cannot make a live
//     offer look stale and get expired by the next housekeeping pass.
//   - MinPriceSeen keeps the lowest price EVER seen. This is what lets us still
//     answer "that vendor ran it at $X last week" after the discount has ended --
//     the signal the operator specifically wants to keep.
//   - Re-appearance REVIVES an expired offer: the listing is purchasable again,
//     so it must become purchasable again, and ExpiredAt is cleared.
func (m *MemoryStore) Put(ctx context.Context, o Offer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.Validate(); err != nil {
		return err
	}
	if o.ID == "" {
		o.ID = DeterministicID(o.SourceID, o.SourceKey)
	}
	if o.Status == "" {
		o.Status = StatusActive
	}
	if o.FirstSeen.IsZero() {
		o.FirstSeen = o.LastSeen
	}
	if o.MinPriceSeen == 0 || (o.PriceCents > 0 && o.PriceCents < o.MinPriceSeen) {
		o.MinPriceSeen = o.PriceCents
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Resolution is never taken from the caller: only RecordResolution writes
	// it. It survives a re-ingest of the SAME hint and resets on a changed one.
	o.Resolution = Resolution{State: ResolutionUnattempted}
	// Price-change fields are derived, never taken from the caller (see
	// Store.Put). A first insert stamps FirstSeen.
	o.PriceChangedAt, o.PreviousPriceCents = o.FirstSeen, 0
	incomingPrice, observedAt := o.PriceCents, o.LastSeen
	prior := PriorPrice{}
	if prev, ok := m.offers[o.ID]; ok {
		o.PriceChangedAt, o.PreviousPriceCents = prev.PriceChangedAt, prev.PreviousPriceCents
		if o.PriceChangedAt.IsZero() {
			o.PriceChangedAt = prev.FirstSeen
		}
		prior = PriorPrice{
			Exists: true, PriceCents: prev.PriceCents, Currency: prev.Currency,
			ChangedAt: o.PriceChangedAt, HasHistory: len(m.history[o.ID]) > 0,
		}
		if o.PriceCents == 0 {
			// An unknown-price poll keeps the last known price.
			o.PriceCents = prev.PriceCents
		} else if prev.PriceCents > 0 && o.PriceCents != prev.PriceCents {
			o.PreviousPriceCents = prev.PriceCents
			if o.LastSeen.After(o.PriceChangedAt) {
				o.PriceChangedAt = o.LastSeen
			}
		}
		if prev.ProductHint.Fingerprint() == o.ProductHint.Fingerprint() {
			o.Resolution = prev.Resolution
		}
		if !prev.FirstSeen.IsZero() && (o.FirstSeen.IsZero() || prev.FirstSeen.Before(o.FirstSeen)) {
			o.FirstSeen = prev.FirstSeen
		}
		if prev.LastSeen.After(o.LastSeen) {
			o.LastSeen = prev.LastSeen
		}
		// Lowest ever, ignoring 0 which means "unknown price" rather than free.
		if prev.MinPriceSeen > 0 && (o.MinPriceSeen == 0 || prev.MinPriceSeen < o.MinPriceSeen) {
			o.MinPriceSeen = prev.MinPriceSeen
		}
	}
	if o.Status == StatusActive {
		o.ExpiredAt = time.Time{}
	}
	m.offers[o.ID] = o
	anchor, row := HistoryRows(prior, incomingPrice, o.Currency, observedAt, o.PriceChangedAt)
	if anchor != nil {
		m.addPoint(o.ID, *anchor, false)
	}
	if row != nil {
		m.addPoint(o.ID, *row, true)
	}
	return nil
}

// addPoint inserts p into an offer's history, kept oldest first. A point at an
// existing observed_at replaces it only when replace is set (the primary-key
// conflict rule every adapter shares; see HistoryRows).
func (m *MemoryStore) addPoint(id string, p PricePoint, replace bool) {
	h := m.history[id]
	i := sort.Search(len(h), func(i int) bool { return !h[i].ObservedAt.Before(p.ObservedAt) })
	if i < len(h) && h[i].ObservedAt.Equal(p.ObservedAt) {
		if replace {
			h[i] = p
		}
		return
	}
	h = append(h, PricePoint{})
	copy(h[i+1:], h[i:])
	h[i] = p
	m.history[id] = h
}

// PriceHistory returns the history of each offer among ids, oldest first:
// points observed at or after since, at most perOffer of the newest.
func (m *MemoryStore) PriceHistory(ctx context.Context, ids []string, since time.Time, perOffer int) (map[string][]PricePoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]PricePoint, len(ids))
	for _, id := range ids {
		var pts []PricePoint
		for _, p := range m.history[id] {
			if since.IsZero() || !p.ObservedAt.Before(since) {
				pts = append(pts, p)
			}
		}
		if perOffer > 0 && len(pts) > perOffer {
			pts = pts[len(pts)-perOffer:]
		}
		if len(pts) > 0 {
			out[id] = pts
		}
	}
	return out, nil
}

// Get returns one offer by id.
func (m *MemoryStore) Get(ctx context.Context, id string) (Offer, bool, error) {
	if err := ctx.Err(); err != nil {
		return Offer{}, false, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.offers[id]
	return o, ok, nil
}

// Summaries returns the summary of every stored offer among ids, expired ones
// included.
func (m *MemoryStore) Summaries(ctx context.Context, ids []string) (map[string]Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Summary, len(ids))
	for _, id := range ids {
		if o, ok := m.offers[id]; ok {
			out[id] = o.Summarize()
		}
	}
	return out, nil
}

// Query returns offers matching q, most-recently-seen first. Expired offers are
// EXCLUDED unless q.IncludeExpired is set.
func (m *MemoryStore) Query(ctx context.Context, q Query) ([]Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Offer, 0, len(m.offers))
	for _, o := range m.offers {
		if !q.IncludeExpired && !o.Purchasable() {
			continue
		}
		if q.SourceID != "" && o.SourceID != q.SourceID {
			continue
		}
		if q.ProductID != "" && o.Resolution.ProductID != q.ProductID {
			continue
		}
		if q.Seller != "" && o.Seller != q.Seller {
			continue
		}
		if !q.Since.IsZero() && o.LastSeen.Before(q.Since) {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].ID < out[j].ID // stable tiebreak
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// MarkExpired transitions a source's offers not seen since the cutoff to
// StatusExpired. It RETAINS them -- an expired offer is still evidence about
// what a vendor charged and when. Deletion is ApplyRetention's job alone.
func (m *MemoryStore) MarkExpired(ctx context.Context, sourceID string, notSeenSince time.Time, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, o := range m.offers {
		if sourceID != "" && o.SourceID != sourceID {
			continue
		}
		if o.Status != StatusActive {
			continue
		}
		if o.LastSeen.Before(notSeenSince) {
			o.Status = StatusExpired
			o.ExpiredAt = now
			m.offers[id] = o
			n++
		}
	}
	return n, nil
}

// ApplyRetention enforces a source's retention policy and reports how many rows
// it deleted. It is the only operation in this package that removes data.
//
// RetainFull deletes nothing. Purge hard-deletes offers last seen before the
// window. SummarizeDecay is rejected by Retention.Validate rather than being
// silently downgraded, because both plausible downgrades are wrong in ways
// nobody would notice.
func (m *MemoryStore) ApplyRetention(ctx context.Context, sourceID string, r Retention, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.Validate(); err != nil {
		return 0, err
	}
	hcut, purgeHistory := r.HistoryCutoff(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, o := range m.offers {
		if sourceID != "" && o.SourceID != sourceID {
			continue
		}
		if r.Policy != RetainFull && o.LastSeen.Before(now.Add(-r.Window)) {
			// The offer goes, and every history row with it.
			delete(m.offers, id)
			delete(m.history, id)
			n++
			continue
		}
		if purgeHistory {
			h := m.history[id]
			// carry is the newest point at or before the cutoff: the price in
			// effect when the window opens. RetainFull keeps it.
			carry := -1
			if r.KeepsCarryIn() {
				carry = sort.Search(len(h), func(i int) bool { return h[i].ObservedAt.After(hcut) }) - 1
			}
			kept := h[:0]
			for i, p := range h {
				if !p.ObservedAt.Before(hcut) || i == carry {
					kept = append(kept, p)
				}
			}
			if len(kept) == 0 {
				delete(m.history, id)
			} else {
				m.history[id] = kept
			}
		}
	}
	return n, nil
}

// Len reports how many offers are stored. Test/diagnostic helper.
func (m *MemoryStore) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.offers)
}

// PendingResolution returns never-asked offers first, then refusals recorded
// under a catalog generation below retryBelowGeneration, each group in id
// order, capped at limit (0 = no cap). Unattempted offers come first so a
// generation advance can never starve offers quark has not seen at all.
func (m *MemoryStore) PendingResolution(ctx context.Context, limit int, retryBelowGeneration int64) ([]Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var fresh, retry []Offer
	for _, o := range m.offers {
		switch {
		case o.Resolution.State == ResolutionUnattempted || o.Resolution.State == "":
			fresh = append(fresh, o)
		case o.Resolution.State == ResolutionRefused && retryBelowGeneration > 0 && o.Resolution.Generation < retryBelowGeneration:
			retry = append(retry, o)
		}
	}
	byID := func(s []Offer) {
		sort.Slice(s, func(i, j int) bool { return s[i].ID < s[j].ID })
	}
	byID(fresh)
	byID(retry)
	out := append(fresh, retry...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordResolution stamps r only while the offer still carries the hint whose
// fingerprint is given.
func (m *MemoryStore) RecordResolution(ctx context.Context, offerID, hintFingerprint string, r Resolution) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.offers[offerID]
	if !ok || o.ProductHint.Fingerprint() != hintFingerprint {
		return false, nil
	}
	o.Resolution = r
	m.offers[offerID] = o
	return true, nil
}
