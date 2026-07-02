package store

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/leftathome/nagus/internal/item"
)

// MemoryStore is an in-memory Store: the reference implementation and the
// contract the SQLite/Postgres adapters must match. Not durable; fine for tests
// and small ephemeral runs.
type MemoryStore struct {
	mu    sync.RWMutex
	items map[string]item.Item
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string]item.Item)}
}

var _ Store = (*MemoryStore)(nil)

// Put validates then stores (replace-by-ID).
func (m *MemoryStore) Put(_ context.Context, it item.Item) error {
	if err := it.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[it.ID] = it
	return nil
}

// Get returns the item by ID.
func (m *MemoryStore) Get(_ context.Context, id string) (item.Item, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	it, ok := m.items[id]
	return it, ok, nil
}

// Search applies the Query predicates and returns matches newest-first.
func (m *MemoryStore) Search(_ context.Context, q Query) ([]item.Item, error) {
	m.mu.RLock()
	out := make([]item.Item, 0, len(m.items))
	for _, it := range m.items {
		if !matches(it, q) {
			continue
		}
		out = append(out, it)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(a, b int) bool {
		if out[a].SeenAt.Equal(out[b].SeenAt) {
			return out[a].ID < out[b].ID // stable tiebreak
		}
		return out[a].SeenAt.After(out[b].SeenAt)
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func matches(it item.Item, q Query) bool {
	if q.Category != "" && it.Category != q.Category {
		return false
	}
	if q.Class != "" && it.Class != q.Class {
		return false
	}
	// MaxPriceCents bounds only priced items; unknown-price (0) items are
	// excluded from a price-bounded query since we cannot assert they qualify.
	if q.MaxPriceCents > 0 && (it.PriceCents == 0 || it.PriceCents > q.MaxPriceCents) {
		return false
	}
	if !q.Since.IsZero() && it.SeenAt.Before(q.Since) {
		return false
	}
	if q.Text != "" && !textMatch(it, q.Text) {
		return false
	}
	return true
}

func textMatch(it item.Item, text string) bool {
	needle := strings.ToLower(text)
	if strings.Contains(strings.ToLower(it.Title), needle) {
		return true
	}
	for _, tok := range it.Tokens {
		if strings.Contains(strings.ToLower(tok), needle) {
			return true
		}
	}
	return false
}
