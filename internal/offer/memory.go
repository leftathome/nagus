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
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{offers: map[string]Offer{}}
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
	if prev, ok := m.offers[o.ID]; ok {
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
	return nil
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
		if q.ProvisionalKey != "" && o.ProvisionalKey != q.ProvisionalKey {
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
	if r.Policy == RetainFull {
		return 0, nil
	}
	cutoff := now.Add(-r.Window)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, o := range m.offers {
		if sourceID != "" && o.SourceID != sourceID {
			continue
		}
		if o.LastSeen.Before(cutoff) {
			delete(m.offers, id)
			n++
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
