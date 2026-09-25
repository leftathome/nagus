package pipeline

import (
	"context"
	"errors"
	"strings"
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

	// TextHints sends a listing's TITLE to quark as hint text when the source
	// states no product identifiers (quark QUARK-02: eBay titles carry part
	// numbers in the title only). The title is attached ONLY after the listing
	// passed the sanitize gate -- offers are otherwise recorded before it, and
	// quark consumes sanitized text (spec D7) -- and only when every structured
	// hint field is empty. Needs an evaluating ingester (a Sanitizer).
	TextHints bool

	// NameHintProducer, when set, makes this source send quark a NAME hint:
	// the listing's producer (the raw aspect with this key) as the hint brand
	// and its title as the hint text, REPLACING any structured hint fields.
	// It is how wine is identified since the LWIN resolver moved to quark
	// (quark QUARK-04): quark matches producer + title against the LWIN
	// catalog's names and returns a product id only for an auto-band match.
	// Structured fields are dropped rather than sent alongside because they
	// would take quark's key path instead (a wine GTIN would mint a product
	// beside the catalog's). As with TextHints, the title is attached only
	// after the listing passed the sanitize gate; a listing the gate drops
	// keeps its structured hint. Needs an evaluating ingester (a Sanitizer).
	NameHintProducer string

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
	// OFFER-ONLY source (gate-at-eval, nagus-7yq). A source with no evaluation
	// machinery attached feeds the offer store and stops there: no glovebox
	// crossing, no extraction, no typed item. That is the point -- a category
	// nothing is currently asking about should cost ZERO evaluation while its
	// offers still accumulate, so activating it later does not start cold.
	//
	// It is expressed as "no Extractor" rather than a flag because that IS the
	// condition: without an extractor there is nothing to evaluate INTO.
	evaluates := i.Extractor != nil && i.Store != nil
	for _, r := range raws {
		// For a text-hint source the gate runs FIRST, so the title is attached
		// to the offer's hint only when glovebox passed it. The verdict is
		// reused below: one gate call per listing either way.
		var san listing.Sanitized
		var sanErr error
		gated := false
		if (i.TextHints || i.NameHintProducer != "") && evaluates && i.Sanitizer != nil {
			san, sanErr = i.Sanitizer.Sanitize(ctx, r)
			gated = true
		}
		// Offer next, and unconditionally: the whole point is to accumulate
		// what a source is selling even when no category extracts it. A listing
		// that fails extraction below is still a real offer that existed.
		if i.Offers != nil {
			o := offerFromRaw(r, now)
			switch {
			case gated && sanErr == nil && i.NameHintProducer != "":
				o.ProductHint = offer.ProductHint{
					Brand: strings.TrimSpace(r.Aspects[i.NameHintProducer]),
					Text:  r.Title,
				}
			case gated && sanErr == nil && i.TextHints && o.ProductHint.Empty():
				o.ProductHint.Text = r.Title
			}
			if err := i.Offers.Put(ctx, o); err != nil {
				res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "offer", Reason: err.Error()})
				i.logf("ingest: offer store dropped %s: %v", r.SourceKey, err)
			} else {
				res.OffersRecorded++
			}
		}
		if !evaluates {
			// Offers were recorded above; there is deliberately nothing else to do.
			continue
		}
		if !gated {
			san, sanErr = i.Sanitizer.Sanitize(ctx, r)
		}
		if sanErr != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "sanitize", Reason: sanErr.Error()})
			i.logf("ingest: sanitize dropped %s: %v", r.SourceKey, sanErr)
			continue
		}
		it, err := i.Extractor.Extract(ctx, san)
		if err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "extract", Reason: err.Error()})
			i.logf("ingest: extract dropped %s: %v", r.SourceKey, err)
			if errors.Is(err, listing.ErrNotInCategory) && i.Store != nil {
				// An item stored before the category rule existed must not
				// outlive it: Shopify sources have no freshness purge.
				if derr := i.Store.Delete(ctx, offer.DeterministicID(r.SourceID, r.SourceKey)); derr != nil {
					i.logf("ingest: removing out-of-category item %s: %v", r.SourceKey, derr)
				}
			}
			continue
		}
		if err := i.Store.Put(ctx, it); err != nil {
			res.Skips = append(res.Skips, Skip{SourceKey: r.SourceKey, Stage: "store", Reason: err.Error()})
			i.logf("ingest: store dropped %s: %v", r.SourceKey, err)
			continue
		}
		res.Stored++
	}
	if i.StaleAfter > 0 && i.Connector != nil && i.Store != nil {
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

	// EXPIRY REQUIRES COMPLETE COVERAGE. Marking an offer expired asserts "the
	// source no longer lists this", and that conclusion is only sound if we
	// actually saw the whole catalogue. After a truncated or rate-limited walk
	// the unseen tail is indistinguishable from a withdrawn listing, so expiring
	// on a partial fetch would quietly mark live, purchasable offers as gone --
	// exactly the wrong direction, since a purchasable offer wrongly expired
	// disappears from every recommendation.
	//
	// A connector that cannot report completeness is treated as complete, which
	// preserves existing behaviour for sources that fetch in one shot.
	if rc, ok := i.Connector.(interface{ FetchComplete() bool }); ok && !rc.FetchComplete() {
		i.logf("ingest: %s fetch was incomplete; skipping offer expiry (cannot tell a withdrawn listing from one we did not reach)", src)
		return
	}

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
		ID:          offer.DeterministicID(r.SourceID, r.SourceKey),
		SourceID:    r.SourceID,
		SourceKey:   r.SourceKey,
		SourceURL:   r.SourceURL,
		Title:       r.Title,
		Body:        r.Body,
		PriceCents:  r.PriceCents,
		Currency:    r.Currency,
		Condition:   r.ConditionRaw,
		Seller:      r.Aspects["seller"],
		Aspects:     r.Aspects,
		ProductHint: hint,
		LastSeen:    seen,
		Status:      offer.StatusActive,
	}
}
