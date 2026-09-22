// Package release implements the "release" category listing.Extractor:
// unpriced release signals such as TTB label approvals (nagus-0ek).
//
// A release signal is a fact about an upcoming bottling, not an offer, so the
// extractor only lifts the connector's typed aspects into Attributes and never
// looks at price. Title is carried verbatim as inert data, as in every other
// extractor.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
)

// Category is the category name.
const Category = "release"

// Keys are the aspects a release connector may supply; anything else is dropped.
var Keys = []string{"brand", "fanciful_name", "class_type", "origin", "permit", "ttb_id", "approval_date"}

// Extractor implements listing.Extractor for category "release".
type Extractor struct{}

var _ listing.Extractor = (*Extractor)(nil)

// New returns an Extractor.
func New() *Extractor { return &Extractor{} }

// Category returns "release".
func (e *Extractor) Category() string { return Category }

// Extract lifts one release signal into an item.
func (e *Extractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	it := item.Item{
		ID:         deterministicID(s.SourceID, s.SourceKey),
		Category:   Category,
		Class:      item.ClassDurable,
		Title:      s.Title, // untrusted-as-data: carried verbatim, never interpreted
		SourceID:   s.SourceID,
		SourceKey:  s.SourceKey,
		SourceURL:  s.SourceURL,
		SeenAt:     s.SeenAt,
		Attributes: map[string]string{},
		Tokens:     tokenize(s.Title + " " + s.Aspects["brand"]),
	}
	for _, k := range Keys {
		if v := strings.TrimSpace(s.Aspects[k]); v != "" {
			it.Attributes[k] = v
		}
	}
	if err := it.Validate(); err != nil {
		return item.Item{}, fmt.Errorf("release: %w", err)
	}
	return it, nil
}

func deterministicID(sourceID, sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceID + "\x00" + sourceKey))
	return hex.EncodeToString(sum[:16])
}

func tokenize(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(f) > 1 && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}
