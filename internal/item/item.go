// Package item defines the normalized item model that every nagus connector
// emits and every store/enrichment/scoring stage consumes.
//
// An Item is the product of: connector -> glovebox sanitize -> extract/tokenize
// -> normalize. By the time an Item exists, all free text has been sanitized and
// the useful signal has been lifted into typed fields (Attributes) plus a
// tokenized index (Tokens). Downstream stages reason over the typed fields;
// free text (Title, and any Attributes value) is treated as data, never as
// instructions (see docs/design section 11, boundary discipline).
package item

import (
	"strings"
	"time"
)

// Class is the product class an item belongs to. The two classes share the
// generic spine but differ in cadence, matching, and delivery (design section 3).
type Class string

const (
	// ClassConsumable is recurring/hyper-local (groceries): basket vs current price.
	ClassConsumable Class = "consumable"
	// ClassDurable is rare "like to have" (land, HDD, ...): criteria vs listing.
	ClassDurable Class = "durable"
)

// Item is the canonical, normalized representation of one source listing.
//
// Price is stored in integer minor units (cents) to avoid float rounding; a
// zero Price means "unknown/unpriced", not "free" -- callers that filter on
// price must treat 0 as absent.
type Item struct {
	ID          string            `json:"id"`           // stable nagus id (source + source_key hash)
	Category    string            `json:"category"`     // e.g. "land", "hdd"
	Class       Class             `json:"class"`        // consumable | durable
	Title       string            `json:"title"`        // sanitized listing title (untrusted data)
	CanonicalID string            `json:"canonical_id"` // set#, part#, APN, style-code+size, ...
	PriceCents  int64             `json:"price_cents"`  // minor units; 0 == unknown
	Currency    string            `json:"currency"`     // ISO 4217, e.g. "USD"
	Condition   string            `json:"condition"`    // new|refurb|used|... (category-defined)
	SourceID    string            `json:"source_id"`    // connector that produced it (e.g. "craigslist")
	SourceKey   string            `json:"source_key"`   // source-native id (listing id / APN)
	SourceURL   string            `json:"source_url"`   // canonical link
	SeenAt      time.Time         `json:"seen_at"`      // when nagus first ingested it
	Attributes  map[string]string `json:"attributes"`   // extracted typed fields (acreage, capacity_tb, ...)
	Tokens      []string          `json:"tokens"`       // FTS tokens over sanitized text
}

// Validate reports whether the Item carries the minimum fields required to be
// stored and matched. It is intentionally strict on provenance (SourceID +
// SourceKey) because those form the dedup/identity key and the audit trail.
func (i Item) Validate() error {
	switch {
	case strings.TrimSpace(i.ID) == "":
		return &FieldError{Field: "id", Reason: "empty"}
	case strings.TrimSpace(i.Category) == "":
		return &FieldError{Field: "category", Reason: "empty"}
	case i.Class != ClassConsumable && i.Class != ClassDurable:
		return &FieldError{Field: "class", Reason: "must be consumable or durable"}
	case strings.TrimSpace(i.SourceID) == "":
		return &FieldError{Field: "source_id", Reason: "empty (provenance required)"}
	case strings.TrimSpace(i.SourceKey) == "":
		return &FieldError{Field: "source_key", Reason: "empty (provenance required)"}
	case i.PriceCents < 0:
		return &FieldError{Field: "price_cents", Reason: "negative"}
	}
	return nil
}

// FieldError names the offending field and why, so ingestion logs point an
// operator straight at the bad value (project convention: informative errors).
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return "item: field " + e.Field + " invalid: " + e.Reason
}
