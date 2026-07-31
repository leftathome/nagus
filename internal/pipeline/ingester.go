package pipeline

import (
	"context"
	"time"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
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

	// Offers, when non-nil, additionally records every fetched listing in the
	// OFFER layer, BEFORE sanitize/extract and independent of whether this
	// category can evaluate it. That is the point of the layer: offers
	// accumulate cheaply so a category activated later is not cold-started, and
	// price history survives even for goods nothing currently scores.
	//
	// Writing offers is deliberately best-effort: an offer-store failure is
	// logged and recorded as a skip but does NOT stop the item path, because the
	// item path is what feeds the live surface. The offer layer is additive.
	Offers offer.Store
	// OfferRetention is this SOURCE's retention policy, applied after ingest.
	// Retention is a property of the source's terms, not of the category doing
	// the evaluating. The zero value disables offer housekeeping.
	OfferRetention offer.Retention
	// OfferExpireAfter, when > 0, marks this source's offers EXPIRED once they
	// have not been seen for that long. Expiry is not deletion: an expired offer
	// is retained as evidence (what a vendor charged, and when) but stops being
	// purchasable. Deletion is OfferRetention's job alone.
	OfferExpireAfter time.Duration

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
	now := i.now()
	for _, r := range raws {
		// Offer FIRST, and unconditionally: the whole point is to accumulate
		// what a source is selling even when no category extracts it. A listing
		// that fails extraction below is still a real offer that existed.
		if i.Offers != nil {
			if err := i.Offers.Put(ctx, offerFromRaw(r, now)); err != nil {
				res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "offer", Reason: err.Error()})
				i.logf("ingest: offer store dropped %s: %v", r.SourceKey, err)
			} else {
				res.OffersRecorded++
			}
		}
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
	i.keepOffers(ctx, &res)
	return res, nil
}

// keepOffers runs offer housekeeping: expire what the source stopped showing,
// then apply the source's retention policy. These are separate on purpose --
// expiry retains evidence, retention is the only thing that deletes.
func (i *Ingester) keepOffers(ctx context.Context, res *IngestResult) {
	if i.Offers == nil || i.Connector == nil {
		return
	}
	src := i.Connector.SourceID()
	now := i.now()
	if i.OfferExpireAfter > 0 {
		n, err := i.Offers.MarkExpired(ctx, src, now.Add(-i.OfferExpireAfter), now)
		if err != nil {
			i.logf("ingest: expire offers %s failed: %v", src, err)
		} else {
			res.OffersExpired = n
		}
	}
	if i.OfferRetention.Policy != "" {
		n, err := i.Offers.ApplyRetention(ctx, src, i.OfferRetention, now)
		if err != nil {
			// Includes the deliberate refusal of an unimplemented policy: a
			// misconfigured retention must be loud, never silently downgraded.
			i.logf("ingest: offer retention %s failed: %v", src, err)
		} else {
			res.OffersPurged = n
		}
	}
}

// offerFromRaw maps a fetched listing into the offer layer. Title/Body cross
// unchanged and UNINTERPRETED -- offers hold untrusted bytes at rest.
func offerFromRaw(r listing.Raw, now time.Time) offer.Offer {
	hint := offer.ProductHint{
		Brand: r.Aspects["brand"],
		MPN:   r.Aspects["mpn"],
		GTIN:  r.Aspects["gtin"],
		Model: r.Aspects["model"],
	}
	seen := r.SeenAt
	if seen.IsZero() {
		seen = now
	}
	return offer.Offer{
		ID:             offer.DeterministicID(r.SourceID, r.SourceKey),
		SourceID:       r.SourceID,
		SourceKey:      r.SourceKey,
		SourceURL:      r.SourceURL,
		Title:          r.Title,
		Body:           r.Body,
		PriceCents:     r.PriceCents,
		Currency:       r.Currency,
		Condition:      r.ConditionRaw,
		Seller:         r.Aspects["seller"],
		Aspects:        r.Aspects,
		ProvisionalKey: offer.ComputeProvisionalKey(hint),
		ProductHint:    hint,
		LastSeen:       seen,
		Status:         offer.StatusActive,
	}
}
