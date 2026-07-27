// Package pipeline wires the generic nagus spine (design section 4):
//
//	connector -> sanitize -> extract -> normalize -> STORE
//	                                   (then, at surface time)
//	STORE -> HARD-FILTER -> ENRICH -> SCORE -> SURFACE
//
// It is category-agnostic: every stage is an interface (listing.Connector,
// listing.Sanitizer, listing.Extractor, store.Store, score.Filter) plus a
// Valuate hook that a category bundle fills with its valuation adapter. The
// front half (Ingester) and back half (Surface) are separate units that share
// a Store; the HDD slice is one fill of them, and land and other categories
// reuse the same units.
//
// Ordering invariant (a first-class design constraint, not an optimization):
// the HARD-FILTER runs BEFORE ENRICH so paid/enrichment work touches only
// survivors of the cheap deterministic gate. Surface enforces this ordering.
package pipeline

import (
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/score"
)

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
	Purged  int // items removed by the post-ingest freshness purge (StaleAfter)
	Skips   []Skip
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
