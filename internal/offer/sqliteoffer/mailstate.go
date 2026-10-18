package sqliteoffer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

var _ offer.MailState = (*Store)(nil)

// mailSchema is the mail readers' state (nagus-p7x; offer.MailState). New
// tables: creating them is catalog-only, nothing is backfilled.
const mailSchema = `
CREATE TABLE IF NOT EXISTS promo_messages (
	source_id       TEXT NOT NULL,
	message_key     TEXT NOT NULL,
	uid_validity    INTEGER NOT NULL DEFAULT 0,
	uid             INTEGER NOT NULL DEFAULT 0,
	processed_at_ns INTEGER NOT NULL,
	outcome         TEXT NOT NULL DEFAULT '',
	products        TEXT NOT NULL DEFAULT '',
	rules_version   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (source_id, message_key)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_promo_messages_processed ON promo_messages(source_id, processed_at_ns);
CREATE TABLE IF NOT EXISTS promo_readings (
	image_sha256 TEXT PRIMARY KEY,
	readings     TEXT NOT NULL,
	stored_at_ns INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_promo_readings_stored ON promo_readings(stored_at_ns);
CREATE TABLE IF NOT EXISTS vision_usage (
	day   TEXT PRIMARY KEY,
	calls INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
`

// migrateMailState adds columns added after promo_messages first shipped.
// SQLite has no ADD COLUMN IF NOT EXISTS.
func migrateMailState(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(promo_messages)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		have[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !have["rules_version"] {
		if _, err := db.Exec(`ALTER TABLE promo_messages ADD COLUMN rules_version TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add column rules_version: %w", err)
		}
	}
	return nil
}

// GetPromoMessage implements offer.MailState.
func (s *Store) GetPromoMessage(ctx context.Context, sourceID, key string) (offer.PromoMessage, bool, error) {
	var pm offer.PromoMessage
	var uv, uid, at int64
	var products string
	err := s.db.QueryRowContext(ctx, `SELECT uid_validity, uid, processed_at_ns, outcome, products, rules_version FROM promo_messages
		WHERE source_id = ? AND message_key = ?`, sourceID, key).Scan(&uv, &uid, &at, &pm.Outcome, &products, &pm.RulesVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return offer.PromoMessage{}, false, nil
	}
	if err != nil {
		return offer.PromoMessage{}, false, fmt.Errorf("sqliteoffer: get promo message: %w", err)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO promo_messages (source_id, message_key, uid_validity, uid, processed_at_ns, outcome, products, rules_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source_id, message_key) DO UPDATE SET uid_validity = excluded.uid_validity, uid = excluded.uid,
			processed_at_ns = excluded.processed_at_ns, outcome = excluded.outcome, products = excluded.products,
			rules_version = excluded.rules_version`,
		pm.SourceID, pm.MessageKey, int64(pm.UIDValidity), int64(pm.UID), pm.ProcessedAt.UnixNano(), pm.Outcome, string(pm.Products), pm.RulesVersion)
	if err != nil {
		return fmt.Errorf("sqliteoffer: put promo message: %w", err)
	}
	return nil
}

// GetPromoReading implements offer.MailState.
func (s *Store) GetPromoReading(ctx context.Context, sha string) ([]byte, bool, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT readings FROM promo_readings WHERE image_sha256 = ?`, sha).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqliteoffer: get promo reading: %w", err)
	}
	return []byte(data), true, nil
}

// PutPromoReading implements offer.MailState.
func (s *Store) PutPromoReading(ctx context.Context, sha string, data []byte, at time.Time) error {
	if sha == "" {
		return errors.New("sqliteoffer: a reading needs its image hash")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO promo_readings (image_sha256, readings, stored_at_ns) VALUES (?, ?, ?)
		ON CONFLICT (image_sha256) DO UPDATE SET readings = excluded.readings, stored_at_ns = excluded.stored_at_ns`,
		sha, string(data), at.UnixNano())
	if err != nil {
		return fmt.Errorf("sqliteoffer: put promo reading: %w", err)
	}
	return nil
}

// AddVisionUsage implements offer.MailState.
func (s *Store) AddVisionUsage(ctx context.Context, day string, delta int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO vision_usage (day, calls) VALUES (?, MAX(0, ?))
		ON CONFLICT (day) DO UPDATE SET calls = MAX(0, vision_usage.calls + ?)`, day, delta, delta)
	if err != nil {
		return fmt.Errorf("sqliteoffer: add vision usage: %w", err)
	}
	return nil
}

// VisionUsage implements offer.MailState.
func (s *Store) VisionUsage(ctx context.Context, day string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT calls FROM vision_usage WHERE day = ?`, day).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sqliteoffer: vision usage: %w", err)
	}
	return n, nil
}

// PurgeMailState implements offer.MailState.
func (s *Store) PurgeMailState(ctx context.Context, sourceID string, before time.Time) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM promo_messages WHERE source_id = ? AND processed_at_ns < ?`, []any{sourceID, before.UnixNano()}},
		{`DELETE FROM promo_readings WHERE stored_at_ns < ?`, []any{before.UnixNano()}},
		{`DELETE FROM vision_usage WHERE day < ?`, []any{offer.UsageDay(before)}},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("sqliteoffer: purge mail state: %w", err)
		}
	}
	return nil
}
