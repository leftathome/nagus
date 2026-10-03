package pipeline

import (
	"context"
	"fmt"
	"sort"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// Surface runs the back half of the spine for ONE category:
//
//	Store -> HARD-FILTER -> ENRICH (valuate) -> SCORE -> rank best-first
//
// One Surface == one category (its Filter/Valuate are category-specific). The
// hard-filter runs BEFORE enrich so paid work touches only survivors (ordering
// invariant). Read-only: eyes, not hands.
//
// The caller's Limit is applied AFTER ranking (nagus-cb8). It used to go to the
// store, which returned the first Limit rows in STORAGE order; the filter then
// dropped most of them and scoring ranked an arbitrary slice -- limit=3 on the
// live hdd corpus returned 0 rows, and the best deals could be absent from any
// top-N. The store is now asked for up to MaxCandidates, everything is
// filtered, valued and ranked, and only then truncated to Limit. Matched and
// Filtered describe the whole candidate set.
//
// EXPIRED LISTINGS ARE HIDDEN (nagus-417) when Offers is set: an item whose
// offer is expired -- not seen at its source for 3 polls -- is dropped BEFORE
// the filter, valuation and the caller's limit, so it can never be surfaced
// as something to buy. The check is one batched offer read for the whole
// candidate set, and expired rows do not count against MaxCandidates (see
// candidates). An item with no offer row is kept: nothing says it is gone.
type Surface struct {
	Store   store.Store
	Filter  score.Filter
	Valuate func(ctx context.Context, it item.Item) (score.DealSignal, error)
	Logf    func(format string, args ...any)
	// MaxCandidates bounds the store read per request; 0 = DefaultMaxCandidates.
	// Reaching it is logged: ranking then covers only part of the category.
	MaxCandidates int
	// Offers, when set, is the offer layer read to hide expired listings and
	// to attach each row's offer summary (Scored.Offer). nil keeps the
	// pre-offer behaviour exactly: nothing hidden, no summaries.
	Offers OfferSummaries
}

// OfferSummaries is the one read the surface makes of the offer layer;
// offer.Store satisfies it.
type OfferSummaries interface {
	Summaries(ctx context.Context, ids []string) (map[string]offer.Summary, error)
}

// maxTopUps bounds the reads candidates makes when expired rows filled part
// of the cap.
const maxTopUps = 3

// DefaultMaxCandidates bounds one surface request's store read. The largest
// live category held 1,343 items on 2026-09-24.
const DefaultMaxCandidates = 5000

func (s *Surface) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Surface queries the stored corpus and returns ranked, scored survivors.
func (s *Surface) Surface(ctx context.Context, q store.Query) (SurfaceResult, error) {
	limit := q.Limit
	candidates := s.MaxCandidates
	if candidates <= 0 {
		candidates = DefaultMaxCandidates
	}
	items, sums, hidden, capReached, err := s.candidates(ctx, q, candidates)
	if err != nil {
		return SurfaceResult{}, err
	}
	if capReached {
		s.logf("surface: %s: candidate cap %d reached; ranking covers only part of the category", q.Category, candidates)
	}
	out := SurfaceResult{Matched: len(items), HiddenExpired: hidden, CapReached: capReached}
	for _, it := range items {
		if ok, reason := s.Filter.Pass(it); !ok {
			s.logf("surface: filtered %s: %s", it.ID, reason)
			continue
		}
		sig := score.DealSignal{Verdict: "unknown-no-reference"}
		if s.Valuate != nil {
			v, verr := s.Valuate(ctx, it)
			if verr != nil {
				// Enrichment failure degrades to an unscored signal; the item
				// still surfaces (a valuation outage must not hide candidates).
				s.logf("surface: valuate failed %s: %v", it.ID, verr)
			} else {
				sig = v
			}
		}
		sc := Scored{Item: it, Signal: sig, Score: score.ScoreItem(it, sig)}
		if sm, ok := sums[it.ID]; ok {
			sc.Offer = &sm
		}
		out.Items = append(out.Items, sc)
	}
	out.Filtered = len(out.Items)
	sort.SliceStable(out.Items, func(a, b int) bool {
		if out.Items[a].Score.Value != out.Items[b].Score.Value {
			return out.Items[a].Score.Value > out.Items[b].Score.Value
		}
		return out.Items[a].Item.ID < out.Items[b].Item.ID
	})
	if limit > 0 && len(out.Items) > limit {
		out.Items = out.Items[:limit]
	}
	return out, nil
}

// candidates reads up to limit SURFACEABLE items: the store's matches minus
// those whose offer is expired, with each survivor's offer summary.
//
// Expired rows must not use up the cap, or a category full of expired
// listings would push live ones out of the ranking. The store orders by
// seen_at (the last poll), and an expired listing has by definition gone
// unpolled for 3 intervals, so expired rows sort last and the first read
// normally already holds every live candidate. When it does not -- the read
// was full AND some rows were expired -- it is repeated with the cap raised by
// the number hidden, up to maxTopUps reads in all. capReached reports that
// the final read was still full.
//
// An offer-store failure FAILS the read rather than surfacing unchecked
// listings: without the offer status nagus cannot tell a live listing from
// one that is gone, and the safe direction is to show nothing.
func (s *Surface) candidates(ctx context.Context, q store.Query, limit int) (items []item.Item, sums map[string]offer.Summary, hidden int, capReached bool, err error) {
	read := limit
	for attempt := 1; ; attempt++ {
		q.Limit = read
		got, err := s.Store.Search(ctx, q)
		if err != nil {
			return nil, nil, 0, false, err
		}
		full := len(got) >= read
		if s.Offers == nil {
			return got, nil, 0, full, nil
		}
		ids := make([]string, len(got))
		for i, it := range got {
			ids[i] = it.ID
		}
		sums, err = s.Offers.Summaries(ctx, ids)
		if err != nil {
			return nil, nil, 0, false, fmt.Errorf("surface: read offer status: %w", err)
		}
		live := make([]item.Item, 0, len(got))
		hidden = 0
		for _, it := range got {
			if sm, ok := sums[it.ID]; ok && sm.Status == offer.StatusExpired {
				hidden++
				continue
			}
			live = append(live, it)
		}
		if !full || hidden == 0 || len(live) >= limit || attempt >= maxTopUps {
			if full && hidden > 0 && len(live) < limit {
				s.logf("surface: %s: %d expired rows still fill part of the candidate cap after %d reads", q.Category, hidden, attempt)
			}
			if len(live) > limit {
				live = live[:limit]
			}
			return live, sums, hidden, full, nil
		}
		read = limit + hidden
	}
}
