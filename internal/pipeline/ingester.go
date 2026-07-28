package pipeline

import (
	"context"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/store"
)

// Ingester runs the front half of the spine for ONE source:
//
//	Connector -> Sanitizer -> Extractor -> Store  (+ freshness purge)
//
// One Ingester == one source. Multiple Ingesters share a Store; each purges only
// its own source's stale content (scoped by Connector.SourceID()). This is the
// unit cmd/nagus runs one-per-source on its own interval.
type Ingester struct {
	Connector listing.Connector
	Sanitizer listing.Sanitizer
	Extractor listing.Extractor
	Store     store.Store

	// StaleAfter, when > 0, enables a post-ingest freshness purge of this
	// source's items older than the window (eBay License 8.1(b)). 0 disables it.
	StaleAfter time.Duration
	// Now returns the current time for the purge cutoff; nil defaults to time.Now.
	Now func() time.Time
	// Logf is an optional log sink; nil disables logging.
	Logf func(format string, args ...any)
}

func (i *Ingester) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Ingester) logf(format string, args ...any) {
	if i.Logf != nil {
		i.Logf(format, args...)
	}
}

// SourceID is the identity of the source this Ingester pulls (its connector's).
func (i *Ingester) SourceID() string { return i.Connector.SourceID() }

// Ingest fetches, sanitizes, extracts, and stores one batch, then purges this
// source's stale items. A per-listing failure is recorded as a Skip and does not
// abort the batch; only a connector Fetch error aborts.
func (i *Ingester) Ingest(ctx context.Context) (IngestResult, error) {
	raws, err := i.Connector.Fetch(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	res := IngestResult{Fetched: len(raws)}
	for _, r := range raws {
		san, err := i.Sanitizer.Sanitize(ctx, r)
		if err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "sanitize", Reason: err.Error()})
			i.logf("ingest: sanitize dropped %s: %v", r.SourceKey, err)
			continue
		}
		it, err := i.Extractor.Extract(ctx, san)
		if err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "extract", Reason: err.Error()})
			i.logf("ingest: extract dropped %s: %v", r.SourceKey, err)
			continue
		}
		if err := i.Store.Put(ctx, it); err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "store", Reason: err.Error()})
			i.logf("ingest: store dropped %s: %v", r.SourceKey, err)
			continue
		}
		res.Stored++
	}
	if i.StaleAfter > 0 && i.Connector != nil {
		cutoff := i.now().Add(-i.StaleAfter)
		purged, derr := i.Store.DeleteStale(ctx, i.Connector.SourceID(), cutoff)
		if derr != nil {
			i.logf("ingest: purge stale %s failed: %v", i.Connector.SourceID(), derr)
		} else {
			res.Purged = purged
			if purged > 0 {
				i.logf("ingest: purged %d stale %s items older than %s", purged, i.Connector.SourceID(), i.StaleAfter)
			}
		}
	}
	return res, nil
}
