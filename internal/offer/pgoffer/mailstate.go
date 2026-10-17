package pgoffer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/leftathome/nagus/internal/offer"
)

var _ offer.MailState = (*Store)(nil)

// mailSchema is the mail readers' state (nagus-p7x; offer.MailState). New
// tables: CREATE TABLE IF NOT EXISTS is catalog-only, nothing is
// backfilled. Matches the SQLite adapter column for column.
const mailSchema = `
CREATE TABLE IF NOT EXISTS promo_messages (
	source_id       TEXT NOT NULL,
	message_key     TEXT NOT NULL,
	uid_validity    BIGINT NOT NULL DEFAULT 0,
	uid             BIGINT NOT NULL DEFAULT 0,
	processed_at_ns BIGINT NOT NULL,
	outcome         TEXT NOT NULL DEFAULT '',
	products        TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (source_id, message_key)
);
CREATE INDEX IF NOT EXISTS idx_promo_messages_processed ON promo_messages(source_id, processed_at_ns);
CREATE TABLE IF NOT EXISTS promo_readings (
	image_sha256 TEXT PRIMARY KEY,
	readings     TEXT NOT NULL,
	stored_at_ns BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_promo_readings_stored ON promo_readings(stored_at_ns);
CREATE TABLE IF NOT EXISTS vision_usage (
	day   TEXT PRIMARY KEY,
	calls BIGINT NOT NULL DEFAULT 0
);
`

// GetPromoMessage implements offer.MailState.
func (s *Store) GetPromoMessage(ctx context.Context, sourceID, key string) (offer.PromoMessage, bool, error) {
	var pm offer.PromoMessage
	var uv, uid, at int64
	var products string
	err := s.pool.QueryRow(ctx, `SELECT uid_validity, uid, processed_at_ns, outcome, products FROM promo_messages
		WHERE source_id = $1 AND message_key = $2`, sourceID, key).Scan(&uv, &uid, &at, &pm.Outcome, &products)
	if errors.Is(err, pgx.ErrNoRows) {
		return offer.PromoMessage{}, false, nil
	}
	if err != nil {
		return offer.PromoMessage{}, false, fmt.Errorf("pgoffer: get promo message: %w", err)
	}
	pm.SourceID, pm.MessageKey = sourceID, key
	pm.UIDValidity, pm.UID = uint32(uv), uint32(uid)
	pm.ProcessedAt = time.Unix(0, at).UTC()
	pm.Products = []byte(products)
	return pm, true, nil
}

// PutPromoMessage implements offer.MailState.
func (s *Store) PutPromoMessage(ctx context.Context, pm offer.PromoMessage) error {
	if err := pm.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO promo_messages (source_id, message_key, uid_validity, uid, processed_at_ns, outcome, products)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (source_id, message_key) DO UPDATE SET uid_validity = EXCLUDED.uid_validity, uid = EXCLUDED.uid,
			processed_at_ns = EXCLUDED.processed_at_ns, outcome = EXCLUDED.outcome, products = EXCLUDED.products`,
		pm.SourceID, pm.MessageKey, int64(pm.UIDValidity), int64(pm.UID), pm.ProcessedAt.UnixNano(), pm.Outcome, string(pm.Products))
	if err != nil {
		return fmt.Errorf("pgoffer: put promo message: %w", err)
	}
	return nil
}

// GetPromoReading implements offer.MailState.
func (s *Store) GetPromoReading(ctx context.Context, sha string) ([]byte, bool, error) {
	var data string
	err := s.pool.QueryRow(ctx, `SELECT readings FROM promo_readings WHERE image_sha256 = $1`, sha).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("pgoffer: get promo reading: %w", err)
	}
	return []byte(data), true, nil
}

// PutPromoReading implements offer.MailState.
func (s *Store) PutPromoReading(ctx context.Context, sha string, data []byte, at time.Time) error {
	if sha == "" {
		return errors.New("pgoffer: a reading needs its image hash")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO promo_readings (image_sha256, readings, stored_at_ns) VALUES ($1, $2, $3)
		ON CONFLICT (image_sha256) DO UPDATE SET readings = EXCLUDED.readings, stored_at_ns = EXCLUDED.stored_at_ns`,
		sha, string(data), at.UnixNano())
	if err != nil {
		return fmt.Errorf("pgoffer: put promo reading: %w", err)
	}
	return nil
}

// AddVisionUsage implements offer.MailState. One statement, so concurrent
// sources cannot lose an increment.
func (s *Store) AddVisionUsage(ctx context.Context, day string, delta int) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO vision_usage (day, calls) VALUES ($1, GREATEST(0, $2::BIGINT))
		ON CONFLICT (day) DO UPDATE SET calls = GREATEST(0, vision_usage.calls + $2::BIGINT)`, day, int64(delta))
	if err != nil {
		return fmt.Errorf("pgoffer: add vision usage: %w", err)
	}
	return nil
}

// VisionUsage implements offer.MailState.
func (s *Store) VisionUsage(ctx context.Context, day string) (int, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT calls FROM vision_usage WHERE day = $1`, day).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("pgoffer: vision usage: %w", err)
	}
	return int(n), nil
}

// PurgeMailState implements offer.MailState.
func (s *Store) PurgeMailState(ctx context.Context, sourceID string, before time.Time) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM promo_messages WHERE source_id = $1 AND processed_at_ns < $2`, []any{sourceID, before.UnixNano()}},
		{`DELETE FROM promo_readings WHERE stored_at_ns < $1`, []any{before.UnixNano()}},
		{`DELETE FROM vision_usage WHERE day < $1`, []any{offer.UsageDay(before)}},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("pgoffer: purge mail state: %w", err)
		}
	}
	return nil
}
