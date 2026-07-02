// Package listing defines the contracts that carry a source listing through the
// front half of the nagus spine, up to the point it becomes a normalized
// item.Item:
//
//	Connector -> Raw -> Sanitizer -> Sanitized -> Extractor -> item.Item
//
// The split exists to make the trust boundary explicit (design sections 4, 7,
// 13). A Connector produces Raw straight from a source: its free-text fields
// (Title, Body) and its Aspects values are UNTRUSTED.
//
// A Sanitizer is the glovebox boundary. Per glovebox's own design (its spec 04
// and 08), glovebox is a GATE, not a rewriter: it scans untrusted content for
// prompt-injection and either passes, quarantines, or rejects it, and it never
// modifies the bytes. Trust is therefore POSITIONAL -- content that has crossed
// the gate is trusted-as-data by construction, not by a flag on the record.
// Sanitized records that fact (the Boundary field) rather than transforming the
// text.
//
// The deterministic HDD slice does not send free text to an LLM: a category
// Extractor lifts the (untrusted) text into the constrained typed schema of
// item.Item via regex/dictionary, so the worst a malicious listing can do is
// yield a wrong field value. The strict glovebox crossing is REQUIRED before any
// free text reaches an LLM instruction context (the future short-list ranker,
// design section 10) or is surfaced to an agent (search_items quotes it as
// data, section 11). For v1 nagus-direct sources the in-process Sanitizer is a
// boundary marker + provenance stamp; the production path routes through the
// out-of-process glovebox service.
//
// Structured, source-native scalar fields (PriceCents, Currency, provenance,
// the source condition code) are trusted-ish and pass through unchanged; it is
// only free text (Title, Body, Aspects values) that must cross the Sanitizer.
package listing

import (
	"context"
	"time"

	"github.com/leftathome/nagus/internal/item"
)

// Raw is one listing exactly as a Connector pulled it from a source. Title,
// Body, and the values in Aspects are UNTRUSTED free text and must not reach an
// LLM instruction context before passing a Sanitizer.
type Raw struct {
	SourceID     string            // connector that produced it, e.g. "ebay"
	SourceKey    string            // source-native id (listing id / item id)
	SourceURL    string            // canonical link back to the listing
	Title        string            // UNTRUSTED free-text title
	Body         string            // UNTRUSTED free-text description (may be empty)
	PriceCents   int64             // minor units; 0 == unknown/unpriced (trusted scalar)
	Currency     string            // ISO 4217, e.g. "USD"
	ConditionRaw string            // source-native condition token/code (e.g. eBay conditionId "2500")
	Aspects      map[string]string // source structured aspects; keys trusted, VALUES untrusted
	SeenAt       time.Time         // when the connector observed it
}

// Sanitized is a Raw that has crossed the glovebox gate. glovebox does not
// modify content, so the fields carry the SAME bytes as the Raw; what changes is
// that the content is now trusted-as-data by construction (it passed the scan;
// quarantined/rejected content never reaches here). The Boundary field records
// which gate passed it, so downstream stages can assert the crossing happened
// (defense in depth: gate-once-at-ingest, quote-at-use).
type Sanitized struct {
	SourceID     string
	SourceKey    string
	SourceURL    string
	Title        string            // sanitized free text (quote as data, never as instructions)
	Body         string            // sanitized free text
	PriceCents   int64             // unchanged trusted scalar
	Currency     string            // unchanged
	ConditionRaw string            // unchanged source condition code
	Aspects      map[string]string // sanitized values
	SeenAt       time.Time
	Boundary     string // identifier of the sanitizer that produced this (provenance of the crossing)
}

// Connector pulls listings from one source. Implementations MUST be safe for
// concurrent use only if documented as such; the spine calls Fetch serially per
// source. Fetch returns Raw listings; it does not sanitize or normalize.
type Connector interface {
	// SourceID is the stable connector identity stamped onto every Raw it emits.
	SourceID() string
	// Fetch returns the current batch of listings for this source. A source with
	// no new listings returns an empty slice and a nil error.
	Fetch(ctx context.Context) ([]Raw, error)
}

// Sanitizer is the glovebox trust boundary: a GATE over untrusted Raw content.
// It scans and either passes (returning Sanitized), or refuses (returning an
// error) for quarantine/reject verdicts. It does not rewrite content. The
// production implementation is the out-of-process glovebox service; nagus ships
// an in-process boundary-marking stand-in for the deterministic reference slice
// and tests. Sanitize MUST NOT fail open -- a scan error or an
// injection/quarantine verdict must return an error so the item is dropped, not
// passed through unmarked.
type Sanitizer interface {
	Sanitize(ctx context.Context, r Raw) (Sanitized, error)
}

// Extractor lifts a Sanitized listing into a normalized item.Item for one
// category. It is deterministic-first (regex/dictionary) and, where a fuzzy
// signal needs an LLM, emits only typed labels over already-sanitized text
// (injection containment, design section 7). The returned item.Item MUST pass
// item.Validate.
type Extractor interface {
	// Category is the item Category this extractor produces, e.g. "hdd".
	Category() string
	// Extract normalizes one sanitized listing. It returns an error (not a
	// partial item) when the listing lacks the minimum fields to be a valid item
	// of this category.
	Extract(ctx context.Context, s Sanitized) (item.Item, error)
}
