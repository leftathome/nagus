package category

import (
	"context"
	"time"

	extrelease "github.com/leftathome/nagus/internal/extract/release"
	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/score"
	"github.com/leftathome/nagus/internal/store"
)

// The release category carries unpriced release signals -- TTB label
// approvals for allocation-only producers (nagus-0ek) -- through the same
// watch -> /watches -> delivery path as deals. A signal is not an offer: there
// is no price, no valuation and no offer history (a price-0 offer would be
// noise in the price layer), so the bundle is only an extractor, a category
// filter that does NOT require a price, and a verdict by age.

// Release verdicts. A watch pings on VerdictNewLabel via strong_verdicts:
// ["new-label"]; an approval older than the fresh window stays a quiet
// candidate as VerdictOldLabel instead of pinging on every poll forever.
const (
	VerdictNewLabel = "new-label"
	VerdictOldLabel = "old-label"
)

// DefaultReleaseFreshDays is how long after approval a label counts as new.
const DefaultReleaseFreshDays = 30

// ReleaseDeps are the injectable dependencies of the release bundle.
type ReleaseDeps struct {
	Store store.Store
	// Sanitizer is the trust boundary listings cross before extraction; nil
	// is the in-process Passthrough (see sanitizerOr).
	Sanitizer listing.Sanitizer
	// FreshDays overrides DefaultReleaseFreshDays.
	FreshDays int
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

// ReleaseFilter admits the category; no price is required.
func ReleaseFilter() score.Filter { return score.Filter{Category: extrelease.Category} }

// NewReleaseIngester builds the ingest half: passthrough sanitizer, the
// release extractor, and the store. No offer layer, no freshness purge.
func NewReleaseIngester(conn listing.Connector, deps ReleaseDeps) *pipeline.Ingester {
	return &pipeline.Ingester{
		Connector: conn,
		Sanitizer: sanitizerOr(deps.Sanitizer, "release"),
		Extractor: extrelease.New(),
		Store:     deps.Store,
		Logf:      deps.Logf,
	}
}

// NewReleaseSurface builds the read half: verdict new-label within the fresh
// window of the approval date, old-label after it (or when the date is absent).
func NewReleaseSurface(deps ReleaseDeps) *pipeline.Surface {
	fresh := deps.FreshDays
	if fresh <= 0 {
		fresh = DefaultReleaseFreshDays
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &pipeline.Surface{
		Store:  deps.Store,
		Filter: ReleaseFilter(),
		Valuate: func(_ context.Context, it item.Item) (score.DealSignal, error) {
			return score.DealSignal{Verdict: releaseVerdict(it, now(), fresh)}, nil
		},
		Logf: deps.Logf,
	}
}

func releaseVerdict(it item.Item, now time.Time, freshDays int) string {
	d, err := time.Parse("2006-01-02", it.Attributes["approval_date"])
	if err != nil {
		return VerdictOldLabel
	}
	if now.Sub(d) <= time.Duration(freshDays)*24*time.Hour {
		return VerdictNewLabel
	}
	return VerdictOldLabel
}
