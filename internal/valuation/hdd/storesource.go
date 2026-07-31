package hdd

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/store"
)

// StoreSource is a ReferenceSource that derives the $/TB reference from nagus's
// OWN ingested offers instead of fetching a third-party catalog per query.
//
// # Why this exists
//
// The original reference fetched one retailer's products.json on EVERY search.
// That had three problems, all measured: its capacity coverage was whatever the
// first page happened to contain (enterprise 16-24TB drives and a lot of SSDs),
// so the entire 6-14TB band where most consumer listings live scored
// "unknown-no-reference"; it paginated not at all, so coverage drifted with the
// retailer's page order; and it put a live third-party call, against a host that
// rate limits hard, in the read path of a read-only surface.
//
// nagus already ingests that same retailer -- and others -- into the item store
// with capacity and condition extracted. Deriving the reference from stored
// offers removes the live dependency, removes the rate-limit exposure, widens
// coverage past one page, and improves automatically as sources are added.
//
// # What the reference now MEANS
//
// This is a deliberate change of meaning, not just of plumbing. The old
// reference was "what one specialist retailer charges". This one is "the median
// of comparable offers nagus has actually seen across all its sources". That is
// a market reference. It is more useful for spotting a deal, but it moves with
// the market -- if every source is expensive this week, nothing looks cheap.
//
// # The self-comparison trap
//
// A reference computed over the same corpus being scored will, for a listing
// with no comparables, return that listing's own price -- a ratio of exactly
// 1.0, scoring "market" forever while looking authoritative. MinSamples guards
// this: below it, PricePerTB reports ok=false ("no reference") rather than a
// confident tautology. Unknown is an honest answer; self-referential is not.
type StoreSource struct {
	// Store supplies the corpus. Only Search is used.
	Store ItemSearcher
	// Category scopes the corpus, e.g. "hdd".
	Category string

	// MinSamples is the fewest comparable offers required before a median is
	// considered a reference at all. Defaults to DefaultMinSamples. See the
	// self-comparison note above -- do not set this to 1.
	MinSamples int
	// MaxAge bounds how stale a comparable may be. Zero -> DefaultRefMaxAge.
	MaxAge time.Duration
	// CacheTTL is how long a computed corpus snapshot is reused. Zero ->
	// DefaultRefCacheTTL. The read path is hot; recomputing per query is waste.
	CacheTTL time.Duration
	// Now defaults to time.Now.
	Now func() time.Time

	mu       sync.Mutex
	cached   []refOffer
	cachedAt time.Time
}

// ItemSearcher is the narrow slice of store.Store this source needs.
type ItemSearcher interface {
	Search(ctx context.Context, q store.Query) ([]item.Item, error)
}

// Defaults for StoreSource.
const (
	// DefaultMinSamples is deliberately above 1; see the self-comparison note.
	DefaultMinSamples = 3
	// DefaultRefMaxAge bounds staleness of a comparable offer.
	DefaultRefMaxAge = 30 * 24 * time.Hour
	// DefaultRefCacheTTL bounds recomputation on the read path.
	DefaultRefCacheTTL = 5 * time.Minute
)

// refOffer is one comparable reduced to the three fields the reference needs.
type refOffer struct {
	capacityTB float64
	condition  string
	centsPerTB int64
}

// PricePerTB implements ReferenceSource over the stored corpus. It returns the
// median $/TB among stored offers matching capacityTB (same 0.1 TB bucketing the
// live source uses) and the given condition tier.
//
// ok=false means no reference could be formed -- either nothing comparable is
// stored, or fewer than MinSamples comparables exist. That is a first-class
// state, not an error.
func (s *StoreSource) PricePerTB(ctx context.Context, capacityTB float64, condition string) (int64, bool, error) {
	if capacityTB <= 0 {
		return 0, false, ErrInvalidCapacity
	}
	offers, err := s.offers(ctx)
	if err != nil {
		return 0, false, err
	}

	wantCapacity := capacityBucket(capacityTB)
	wantCondition := strings.ToLower(strings.TrimSpace(condition))

	var matched []int64
	for _, o := range offers {
		if capacityBucket(o.capacityTB) != wantCapacity {
			continue
		}
		if o.condition != wantCondition {
			continue
		}
		matched = append(matched, o.centsPerTB)
	}
	if len(matched) < s.minSamples() {
		return 0, false, nil
	}
	return median(matched), true, nil
}

// offers returns the cached corpus snapshot, recomputing when the TTL lapses.
func (s *StoreSource) offers(ctx context.Context) ([]refOffer, error) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && now.Sub(s.cachedAt) < s.cacheTTL() {
		return s.cached, nil
	}

	items, err := s.Store.Search(ctx, store.Query{
		Category: s.Category,
		Since:    now.Add(-s.maxAge()),
	})
	if err != nil {
		return nil, err
	}

	offers := make([]refOffer, 0, len(items))
	for _, it := range items {
		o, ok := toRefOffer(it)
		if !ok {
			continue
		}
		offers = append(offers, o)
	}
	s.cached, s.cachedAt = offers, now
	return offers, nil
}

// toRefOffer reduces a stored item to a comparable. Items missing any of price,
// capacity or condition cannot anchor a $/TB reference and are skipped -- an
// absent fact, not an error.
func toRefOffer(it item.Item) (refOffer, bool) {
	if it.PriceCents <= 0 {
		return refOffer{}, false
	}
	raw, ok := it.Attributes["capacity_tb"]
	if !ok {
		return refOffer{}, false
	}
	capTB, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || capTB <= 0 {
		return refOffer{}, false
	}
	cond := strings.ToLower(strings.TrimSpace(it.Condition))
	if cond == "" {
		return refOffer{}, false
	}
	return refOffer{
		capacityTB: capTB,
		condition:  cond,
		centsPerTB: int64(float64(it.PriceCents)/capTB + 0.5),
	}, true
}

func (s *StoreSource) minSamples() int {
	if s.MinSamples > 0 {
		return s.MinSamples
	}
	return DefaultMinSamples
}

func (s *StoreSource) maxAge() time.Duration {
	if s.MaxAge > 0 {
		return s.MaxAge
	}
	return DefaultRefMaxAge
}

func (s *StoreSource) cacheTTL() time.Duration {
	if s.CacheTTL > 0 {
		return s.CacheTTL
	}
	return DefaultRefCacheTTL
}

func (s *StoreSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
