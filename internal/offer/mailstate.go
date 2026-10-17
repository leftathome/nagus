package offer

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MailState is state the deals-mailbox readers keep BESIDE the offers, in the
// same database, so a restart does not redo work (nagus-p7x; design
// docs/design/2026-10-16-promo-vision.md, "Persistence"):
//
//   - promo_messages: one row per (source, message) a promo-vision-v1 parser
//     processed to a FINAL outcome, holding the products it agreed on as
//     opaque JSON (deal lines plus their extra aspects), so the next poll --
//     after a restart too -- re-emits them without fetching an image,
//     calling the vision model or following a link;
//   - promo_readings: the two vision readings of an image, keyed by the
//     SHA-256 of its bytes, shared by every source;
//   - vision_usage: vision calls per UTC day, shared by every source, so a
//     restart does not reset the daily budget.
//
// Payloads are opaque to the store. Every adapter of Store implements it;
// it is a separate interface because only the mail readers use it.
type MailState interface {
	// GetPromoMessage returns the processed record of one message.
	GetPromoMessage(ctx context.Context, sourceID, messageKey string) (PromoMessage, bool, error)
	// PutPromoMessage inserts or replaces a message's record.
	PutPromoMessage(ctx context.Context, m PromoMessage) error
	// GetPromoReading returns the stored readings of an image.
	GetPromoReading(ctx context.Context, imageSHA256 string) ([]byte, bool, error)
	// PutPromoReading inserts or replaces an image's readings.
	PutPromoReading(ctx context.Context, imageSHA256 string, readings []byte, at time.Time) error
	// AddVisionUsage adds delta (which may be negative: a refund) to the
	// calls of a UTC day ("2006-01-02"), never below zero.
	AddVisionUsage(ctx context.Context, day string, delta int) error
	// VisionUsage returns a day's calls (0 when none).
	VisionUsage(ctx context.Context, day string) (int, error)
	// PurgeMailState deletes sourceID's promo messages processed before
	// before, every reading stored before before, and the usage of days
	// before before's UTC day. It runs in the retention pass.
	PurgeMailState(ctx context.Context, sourceID string, before time.Time) error
}

// PromoMessage is one processed promo message.
type PromoMessage struct {
	SourceID string
	// MessageKey identifies the message: a hash of the verified sender and
	// the Message-ID (the caller's).
	MessageKey string
	// UIDValidity and UID are where the message was seen (a hint only).
	UIDValidity, UID uint32
	ProcessedAt      time.Time
	Outcome          string
	// Products is the caller's encoding of what to re-emit.
	Products []byte
}

// ErrNoMessageKey is a PromoMessage without its identity.
var ErrNoMessageKey = errors.New("offer: a promo message needs a source and a message key")

// Validate checks the identity fields.
func (m PromoMessage) Validate() error {
	if m.SourceID == "" || m.MessageKey == "" {
		return ErrNoMessageKey
	}
	return nil
}

// UsageDay is the UTC day key of t.
func UsageDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// memMailState is MemoryStore's MailState.
type memMailState struct {
	mu       sync.Mutex
	messages map[[2]string]PromoMessage
	readings map[string]memReading
	usage    map[string]int
}

type memReading struct {
	data []byte
	at   time.Time
}

func (m *MemoryStore) mail() *memMailState {
	m.mailOnce.Do(func() {
		m.mailState = &memMailState{messages: map[[2]string]PromoMessage{}, readings: map[string]memReading{}, usage: map[string]int{}}
	})
	return m.mailState
}

// GetPromoMessage implements MailState.
func (m *MemoryStore) GetPromoMessage(ctx context.Context, sourceID, key string) (PromoMessage, bool, error) {
	if err := ctx.Err(); err != nil {
		return PromoMessage{}, false, err
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	pm, ok := s.messages[[2]string{sourceID, key}]
	pm.Products = append([]byte(nil), pm.Products...)
	return pm, ok, nil
}

// PutPromoMessage implements MailState.
func (m *MemoryStore) PutPromoMessage(ctx context.Context, pm PromoMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pm.Validate(); err != nil {
		return err
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	pm.Products = append([]byte(nil), pm.Products...)
	pm.ProcessedAt = pm.ProcessedAt.UTC()
	s.messages[[2]string{pm.SourceID, pm.MessageKey}] = pm
	return nil
}

// GetPromoReading implements MailState.
func (m *MemoryStore) GetPromoReading(ctx context.Context, sha string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.readings[sha]
	return append([]byte(nil), r.data...), ok, nil
}

// PutPromoReading implements MailState.
func (m *MemoryStore) PutPromoReading(ctx context.Context, sha string, data []byte, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sha == "" {
		return errors.New("offer: a reading needs its image hash")
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readings[sha] = memReading{data: append([]byte(nil), data...), at: at}
	return nil
}

// AddVisionUsage implements MailState.
func (m *MemoryStore) AddVisionUsage(ctx context.Context, day string, delta int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage[day] = max(0, s.usage[day]+delta)
	return nil
}

// VisionUsage implements MailState.
func (m *MemoryStore) VisionUsage(ctx context.Context, day string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage[day], nil
}

// PurgeMailState implements MailState.
func (m *MemoryStore) PurgeMailState(ctx context.Context, sourceID string, before time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s := m.mail()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, pm := range s.messages {
		if k[0] == sourceID && pm.ProcessedAt.Before(before) {
			delete(s.messages, k)
		}
	}
	for k, r := range s.readings {
		if r.at.Before(before) {
			delete(s.readings, k)
		}
	}
	cut := UsageDay(before)
	for d := range s.usage {
		if d < cut {
			delete(s.usage, d)
		}
	}
	return nil
}
