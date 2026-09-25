// Package enrich runs the asynchronous quark resolution pass (quark design
// section 5, "Coupling (D4)").
//
// Offers are stored by ingest with no product id; this pass asks quark, in
// batches, what product each offer's hint names, and stamps the answer. It is
// deliberately OUTSIDE the ingest loop: quark being slow, down, or
// misconfigured leaves offers unattempted -- a state nagus already handles
// honestly -- and never fails, delays, or rolls back an ingest.
package enrich

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/quark"
)

// Resolver is the one quark call this pass makes; *quark.Client satisfies it.
type Resolver interface {
	Resolve(ctx context.Context, hints []quark.Hint, retry bool) (quark.Response, error)
}

// maxBatchesPerPass bounds one pass (x MaxBatch = 100,000 offers). A pass that
// hits it simply resumes next interval; the bound exists so a store that keeps
// returning the same rows cannot spin forever.
const maxBatchesPerPass = 200

// Enricher resolves offers against quark.
type Enricher struct {
	Offers   offer.Store
	Quark    Resolver
	Interval time.Duration
	// CategoryFor maps an offer's source to the category quark should apply.
	// Sources with no category send "" and quark refuses them -- quark decides
	// what it can identify, not nagus.
	CategoryFor func(sourceID string) string
	// BatchSize defaults to quark.MaxBatch.
	BatchSize int
	Now       func() time.Time
	Logf      func(format string, args ...any)

	generation atomic.Int64 // highest catalog generation quark has reported
	stats      Stats
	mu         sync.Mutex
}

// Stats are cumulative counters for /metrics.
type Stats struct {
	BatchesOK     int64
	BatchesFailed int64
	Unauthorized  int64
	// Recorded counts stamped answers by resulting state. Unidentifiable
	// offers are recorded locally without a quark call (empty hint).
	Resolved, Refused, Quarantined, Unidentifiable int64
	// Discarded counts answers dropped because the offer's hint changed while
	// quark was answering (or the offer was deleted).
	Discarded  int64
	Retries    int64
	Generation int64
	// LastCleanPass is when a pass last finished without error (Unix seconds,
	// 0 = never since start). A pass with nothing pending counts: "caught up"
	// is healthy. This is the staleness gauge the stall alert reads, because
	// counters cannot tell an idle nagus from a broken one.
	LastCleanPass int64
}

// Snapshot returns the current counters.
func (e *Enricher) Snapshot() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	s.Generation = e.generation.Load()
	return s
}

// PassResult summarises one pass.
type PassResult struct {
	Batches, Recorded, Discarded int
}

// Run executes a pass immediately and then every Interval until ctx ends.
func (e *Enricher) Run(ctx context.Context) {
	e.pass(ctx)
	if e.Interval <= 0 {
		return
	}
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.pass(ctx)
		}
	}
}

func (e *Enricher) pass(ctx context.Context) {
	res, err := e.RunPass(ctx)
	if err == nil {
		now := time.Now
		if e.Now != nil {
			now = e.Now
		}
		e.mu.Lock()
		e.stats.LastCleanPass = now().Unix()
		e.mu.Unlock()
	}
	switch {
	case errors.Is(err, quark.ErrUnauthorized):
		e.logf("enrich: quark rejected nagus's token; offers stay unattempted until the token is fixed")
	case err != nil:
		e.logf("enrich: pass stopped early, offers stay unattempted: %v", err)
	}
	if res.Batches > 0 {
		e.logf("enrich: batches=%d recorded=%d discarded=%d generation=%d",
			res.Batches, res.Recorded, res.Discarded, e.generation.Load())
	}
}

// RunPass resolves every offer currently pending, in batches. It returns early,
// with the error, on the first failed quark call: offers not yet sent stay
// unattempted and are picked up by the next pass.
func (e *Enricher) RunPass(ctx context.Context) (PassResult, error) {
	var res PassResult
	size := e.BatchSize
	if size <= 0 || size > quark.MaxBatch {
		size = quark.MaxBatch
	}
	for i := 0; i < maxBatchesPerPass; i++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// Retries are only eligible once quark has reported a generation above
		// the one a refusal was recorded under; generation 0 disables them.
		gen := e.generation.Load()
		pending, err := e.Offers.PendingResolution(ctx, size, gen)
		if err != nil {
			return res, fmt.Errorf("pending resolution: %w", err)
		}
		if len(pending) == 0 {
			return res, nil
		}
		// PendingResolution returns never-asked offers before retries, and the
		// retry flag is per batch, so send the leading run of one kind only.
		batch := leadingRun(pending)
		retry := batch[0].Resolution.State == offer.ResolutionRefused

		recorded, discarded, err := e.resolveBatch(ctx, batch, retry)
		res.Batches++
		res.Recorded += recorded
		res.Discarded += discarded
		if err != nil {
			return res, err
		}
		if recorded == 0 && discarded == 0 {
			// Nothing changed state, so the next query would return the same
			// rows. Stop rather than spin; the next pass tries again.
			return res, nil
		}
	}
	return res, nil
}

func leadingRun(offers []offer.Offer) []offer.Offer {
	retry := offers[0].Resolution.State == offer.ResolutionRefused
	for i, o := range offers {
		if (o.Resolution.State == offer.ResolutionRefused) != retry {
			return offers[:i]
		}
	}
	return offers
}

func (e *Enricher) resolveBatch(ctx context.Context, all []offer.Offer, retry bool) (recorded, discarded int, err error) {
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	at := now().UTC()

	// An EMPTY hint gives quark nothing to identify. Record it locally instead
	// of spending a call on a guaranteed refusal -- and, because nagus relays
	// every store as one principal, keep hint-less stores out of quark's
	// refused ratio, where they would hold its alert permanently above
	// threshold. See offer.ResolutionUnidentifiable.
	batch := make([]offer.Offer, 0, len(all))
	var nUnidentifiable int64
	for _, o := range all {
		if !o.ProductHint.Empty() {
			batch = append(batch, o)
			continue
		}
		applied, rerr := e.Offers.RecordResolution(ctx, o.ID, o.ProductHint.Fingerprint(),
			offer.Resolution{State: offer.ResolutionUnidentifiable, At: at})
		if rerr != nil {
			return recorded, discarded, fmt.Errorf("record resolution: %w", rerr)
		}
		if applied {
			recorded++
			nUnidentifiable++
		} else {
			discarded++
		}
	}
	if nUnidentifiable > 0 {
		e.count(func(s *Stats) { s.Unidentifiable += nUnidentifiable })
	}
	if len(batch) == 0 {
		return recorded, discarded, nil
	}

	hints := make([]quark.Hint, len(batch))
	fingerprints := make([]string, len(batch))
	for i, o := range batch {
		// Snapshot the fingerprint of EXACTLY the hint sent: the answer is
		// stamped only if the offer still carries it when quark replies.
		fingerprints[i] = o.ProductHint.Fingerprint()
		cat := ""
		if e.CategoryFor != nil {
			cat = e.CategoryFor(o.SourceID)
		}
		hints[i] = quark.Hint{
			Category: cat,
			Brand:    o.ProductHint.Brand,
			MPN:      o.ProductHint.MPN,
			GTIN:     o.ProductHint.GTIN,
			Model:    o.ProductHint.Model,
			Text:     o.ProductHint.Text,
		}
	}

	resp, err := e.Quark.Resolve(ctx, hints, retry)
	if err != nil {
		e.count(func(s *Stats) {
			s.BatchesFailed++
			if errors.Is(err, quark.ErrUnauthorized) {
				s.Unauthorized++
			}
		})
		// Blank offers recorded above stay recorded; report them honestly.
		return recorded, discarded, err
	}
	if resp.CatalogGeneration > e.generation.Load() {
		e.generation.Store(resp.CatalogGeneration)
	}

	var nResolved, nRefused, nQuarantined int64
	for i, r := range resp.Results {
		stamp := offer.Resolution{Generation: resp.CatalogGeneration, At: at}
		switch r.Route {
		case quark.RouteExact, quark.RouteMinted, quark.RouteText, quark.RouteFuzzy:
			if r.ProductID == "" {
				// A resolved route without an id is a contract violation; treat
				// the hint as not yet answered rather than record a blank id.
				continue
			}
			stamp.State, stamp.ProductID = offer.ResolutionResolved, r.ProductID
		case quark.RouteQuarantined:
			stamp.State = offer.ResolutionQuarantined
		default:
			// refused, unmatched (text named no key quark holds -- yet),
			// adjudicate (a wine match quark would not name; it holds the
			// candidates for review), or a route this client does not know:
			// not resolved, and eligible for retry when quark's catalogue
			// grows.
			stamp.State = offer.ResolutionRefused
		}
		applied, rerr := e.Offers.RecordResolution(ctx, batch[i].ID, fingerprints[i], stamp)
		if rerr != nil {
			return recorded, discarded, fmt.Errorf("record resolution: %w", rerr)
		}
		if !applied {
			discarded++
			continue
		}
		recorded++
		switch stamp.State {
		case offer.ResolutionResolved:
			nResolved++
		case offer.ResolutionQuarantined:
			nQuarantined++
		default:
			nRefused++
		}
	}
	e.count(func(s *Stats) {
		s.BatchesOK++
		s.Resolved += nResolved
		s.Refused += nRefused
		s.Quarantined += nQuarantined
		s.Discarded += int64(discarded)
		if retry {
			s.Retries += int64(len(batch))
		}
	})
	return recorded, discarded, nil
}

func (e *Enricher) count(f func(*Stats)) {
	e.mu.Lock()
	f(&e.stats)
	e.mu.Unlock()
}

func (e *Enricher) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}
