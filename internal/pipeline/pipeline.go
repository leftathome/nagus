// Package pipeline wires the generic nagus spine (design section 4):
//
//	connector -> sanitize -> extract -> normalize -> STORE
//	                                   (then, at surface time)
//	STORE -> HARD-FILTER -> ENRICH -> SCORE -> SURFACE
//
// It is category-agnostic: every stage is an interface (listing.Connector,
// listing.Sanitizer, listing.Extractor, store.Store, score.Filter) plus a
// Valuate hook that a category bundle fills with its valuation adapter. The HDD
// slice is one fill of this struct; land and other categories reuse it.
//
// Ordering invariant (a first-class design constraint, not an optimization):
// the HARD-FILTER runs BEFORE ENRICH so paid/enrichment work touches only
// survivors of the cheap deterministic gate. Surface enforces this ordering.
package pipeline

import (
	"context"
	"sort"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// Pipeline holds the wired stages. The ingest half (Connector, Sanitizer,
// Extractor, Store) and the surface half (Store, Filter, Valuate) share the
// Store. Valuate may be nil, in which case items surface with an
// unknown-no-reference signal.
type Pipeline struct {
	Connector listing.Connector
	Sanitizer listing.Sanitizer
	Extractor listing.Extractor
	Store     store.Store

	Filter  score.Filter
	Valuate func(ctx context.Context, it item.Item) (score.DealSignal, error)

	// Logf is an optional structured-ish log sink; nil disables logging.
	Logf func(format string, args ...any)
}

func (p *Pipeline) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// Skip records one listing dropped during ingest, with the stage and reason, so
// an operator can see why a listing did not become a stored item.
type Skip struct {
	SourceKey string
	Stage     string // "sanitize" | "extract" | "store"
	Reason    string
}

// IngestResult summarizes one Ingest run.
type IngestResult struct {
	Fetched int
	Stored  int
	Skips   []Skip
}

// Ingest runs the front half of the spine: fetch listings, cross the sanitize
// boundary, extract into typed items, and store them. A failure of one listing
// (sanitize refusal, unextractable, store rejection) is recorded as a Skip and
// does not abort the batch; only a connector-level Fetch error aborts.
func (p *Pipeline) Ingest(ctx context.Context) (IngestResult, error) {
	raws, err := p.Connector.Fetch(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	res := IngestResult{Fetched: len(raws)}
	for _, r := range raws {
		san, err := p.Sanitizer.Sanitize(ctx, r)
		if err != nil {
			// A sanitize error is a quarantine/reject verdict: drop, do not pass.
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "sanitize", Reason: err.Error()})
			p.logf("ingest: sanitize dropped %s: %v", r.SourceKey, err)
			continue
		}
		it, err := p.Extractor.Extract(ctx, san)
		if err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "extract", Reason: err.Error()})
			p.logf("ingest: extract dropped %s: %v", r.SourceKey, err)
			continue
		}
		if err := p.Store.Put(ctx, it); err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "store", Reason: err.Error()})
			p.logf("ingest: store dropped %s: %v", r.SourceKey, err)
			continue
		}
		res.Stored++
	}
	return res, nil
}

// Scored is one surfaced item with its deal signal and score.
type Scored struct {
	Item   item.Item
	Signal score.DealSignal
	Score  score.Score
}

// SurfaceResult summarizes one Surface run alongside the ranked hits.
type SurfaceResult struct {
	Matched  int // items returned by the store query
	Filtered int // survivors of the hard-filter (== len(Items))
	Items    []Scored
}

// Surface runs the back half of the spine over the stored corpus: query, then
// HARD-FILTER (cheap, deterministic), then ENRICH (valuation) only on
// survivors, then SCORE, then rank best-first. This is the same read path a
// watch (saved query) and an ad-hoc search_items call use (design section 11);
// it is read-only (eyes, not hands).
func (p *Pipeline) Surface(ctx context.Context, q store.Query) (SurfaceResult, error) {
	items, err := p.Store.Search(ctx, q)
	if err != nil {
		return SurfaceResult{}, err
	}
	out := SurfaceResult{Matched: len(items)}
	for _, it := range items {
		if ok, reason := p.Filter.Pass(it); !ok {
			p.logf("surface: filtered %s: %s", it.ID, reason)
			continue
		}
		sig := score.DealSignal{Verdict: "unknown-no-reference"}
		if p.Valuate != nil {
			s, verr := p.Valuate(ctx, it)
			if verr != nil {
				// Enrichment failure degrades to an unscored signal; the item
				// still surfaces (a valuation outage must not hide candidates).
				p.logf("surface: valuate failed %s: %v", it.ID, verr)
			} else {
				sig = s
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
