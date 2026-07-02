// Package store defines the nagus item store as a swappable adapter.
//
// The design (docs/design section 8 and section 15 decision 3) keeps storage
// behind ONE interface so the same pipeline runs on either backend:
//
//   - SQLite + FTS5      -- single-file, zero-ops, homelab default
//   - Postgres + pgvector -- when semantic/vector search or multi-writer is needed
//
// The push reader (watches = saved queries + threshold) and the pull reader
// (the search_items MCP tool) are BOTH implemented on top of Search, so a watch
// and an ad-hoc agent query are the same operation over one corpus.
//
// MemoryStore in this package is the reference implementation used by tests and
// as the semantic contract the SQLite/Postgres adapters must satisfy.
package store

import (
	"context"
	"time"

	"github.com/leftathome/nagus/internal/item"
)

// Query is a structured item search. Zero-valued fields mean "no constraint",
// so an empty Query returns everything (bounded by Limit). Deal-watching needs
// exactly these structured predicates -- range, recency, category -- which is
// why a pure semantic index (memory-core) is a companion, not a replacement.
type Query struct {
	Category      string     // exact category match; "" = any
	Class         item.Class // exact class match; "" = any
	MaxPriceCents int64      // upper bound; 0 = no bound (note: item price 0 == unknown)
	Since         time.Time  // only items with SeenAt >= Since; zero = no bound
	Text          string     // case-insensitive token/substring match over Title+Tokens
	Limit         int        // max rows; 0 = no limit
}

// Store is the item persistence + query adapter. Implementations MUST be safe
// for concurrent use.
type Store interface {
	// Put inserts or replaces an item by its ID. It returns item.FieldError via
	// Validate for malformed items rather than persisting bad data.
	Put(ctx context.Context, it item.Item) error
	// Get returns the item and true, or the zero item and false if absent.
	Get(ctx context.Context, id string) (item.Item, bool, error)
	// Search returns items matching the Query, most-recent (SeenAt desc) first.
	Search(ctx context.Context, q Query) ([]item.Item, error)
}
