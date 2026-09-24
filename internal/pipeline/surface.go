package pipeline

import (
	"context"
	"sort"

	"github.com/leftathome/nagus/internal/item"
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
type Surface struct {
	Store   store.Store
	Filter  score.Filter
	Valuate func(ctx context.Context, it item.Item) (score.DealSignal, error)
	Logf    func(format string, args ...any)
	// MaxCandidates bounds the store read per request; 0 = DefaultMaxCandidates.
	// Reaching it is logged: ranking then covers only part of the category.
	MaxCandidates int
}

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
	q.Limit = candidates
	items, err := s.Store.Search(ctx, q)
	if err != nil {
		return SurfaceResult{}, err
	}
	if len(items) >= candidates {
		s.logf("surface: %s: candidate cap %d reached; ranking covers only part of the category", q.Category, candidates)
	}
	out := SurfaceResult{Matched: len(items)}
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
		out.Items = append(out.Items, Scored{Item: it, Signal: sig, Score: score.ScoreItem(it, sig)})
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
