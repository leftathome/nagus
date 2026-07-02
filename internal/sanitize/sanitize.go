// Package sanitize provides nagus's in-process stand-in for the glovebox trust
// boundary (listing.Sanitizer).
//
// IMPORTANT: Passthrough is NOT a security scanner. glovebox's real gate scans
// untrusted content for prompt-injection and quarantines/rejects it; Passthrough
// does neither. It exists for the DETERMINISTIC nagus-direct path (design
// section 4 + the operator's boundary rule): nagus collects from a source
// directly, runs regex/typed extraction, deterministic hard-filter, valuation
// math on typed fields, and deterministic scoring -- no free text ever reaches
// an LLM instruction context. On that path the protection is structural
// (constrained typed schema, section 7) plus quote-at-use at the surface
// (section 11), so a boundary MARKER is sufficient and honest.
//
// Any path that would feed listing free text to an LLM (the future short-list
// ranker, section 10) MUST route through the real out-of-process glovebox gate
// instead of Passthrough. This is called out again on the type itself.
package sanitize

import (
	"context"

	"github.com/leftathome/nagus/internal/listing"
)

// Passthrough marks a Raw listing as having crossed the boundary WITHOUT
// scanning it. Use only on the deterministic path where free text never reaches
// an LLM. It never rewrites content (mirroring glovebox's byte-preserving
// invariant); it copies fields verbatim and stamps Boundary for provenance.
type Passthrough struct {
	// Name identifies this boundary in the Sanitized provenance, e.g.
	// "sanitize.passthrough". Defaults to "sanitize.passthrough" when empty.
	Name string
}

var _ listing.Sanitizer = Passthrough{}

// boundaryName returns the configured name or the default.
func (p Passthrough) boundaryName() string {
	if p.Name == "" {
		return "sanitize.passthrough"
	}
	return p.Name
}

// Sanitize copies r into a Sanitized with the same (byte-preserved) content and
// stamps the boundary. It never returns an error: it does no scanning, so there
// is no verdict that could reject the item. This is the intended behavior for
// the deterministic path only.
func (p Passthrough) Sanitize(_ context.Context, r listing.Raw) (listing.Sanitized, error) {
	var aspects map[string]string
	if r.Aspects != nil {
		aspects = make(map[string]string, len(r.Aspects))
		for k, v := range r.Aspects {
			aspects[k] = v
		}
	}
	return listing.Sanitized{
		SourceID:     r.SourceID,
		SourceKey:    r.SourceKey,
		SourceURL:    r.SourceURL,
		Title:        r.Title,
		Body:         r.Body,
		PriceCents:   r.PriceCents,
		Currency:     r.Currency,
		ConditionRaw: r.ConditionRaw,
		Aspects:      aspects,
		SeenAt:       r.SeenAt,
		Boundary:     p.boundaryName(),
	}, nil
}
