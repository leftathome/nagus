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
type Surface struct {
	Store   store.Store
	Filter  score.Filter
	Valuate func(ctx context.Context, it item.Item) (score.DealSignal, error)
	Logf    func(format string, args ...any)
}

func (s *Surface) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Surface queries the stored corpus and returns ranked, scored survivors.
func (s *Surface) Surface(ctx context.Context, q store.Query) (SurfaceResult, error) {
	items, err := s.Store.Search(ctx, q)
	if err != nil {
		return SurfaceResult{}, err
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
	return out, nil
}
