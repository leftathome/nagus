package sqliteoffer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
)

// TestNewFailsWhenParentIsNotADirectory exercises New's failure path when the
// underlying file cannot be opened at all: the DSN's parent path component is
// a regular file, not a directory. sql.Open itself never errors here
// (modernc.org/sqlite only implements driver.Driver, so database/sql defers
// dialing until first use), so the failure surfaces at the first real access
// -- the journal_mode/busy_timeout PRAGMA -- which is exactly the branch this
// test is after.
func TestNewFailsWhenParentIsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	dsn := filepath.Join(blocker, "offers.db")

	s, err := New(dsn)
	if err == nil {
		s.Close()
		t.Fatal("expected New to fail when the DSN's parent is a regular file")
	}
}

// TestNewFailsOnCorruptFile proves New surfaces an error rather than silently
// "succeeding" against a file that exists but is not a valid SQLite database.
func TestNewFailsOnCorruptFile(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(dsn, []byte("this is not a sqlite database file at all, just garbage bytes"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}

	s, err := New(dsn)
	if err == nil {
		s.Close()
		t.Fatal("expected New to fail against a corrupt/non-database file")
	}
}

func newTestOfferStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "offers.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func validOffer(sourceID, sourceKey string, price int64, seen time.Time) offer.Offer {
	return offer.Offer{
		SourceID: sourceID, SourceKey: sourceKey,
		SourceURL: "https://example.test/" + sourceKey,
		Title:     "A drive", PriceCents: price, Currency: "USD",
		LastSeen: seen,
	}
}

// TestClosedStoreOperationsReturnErrors drives every Store method against a
// Store whose handle has already been Close'd: the documented contract is an
// error return, never a panic, and this exercises the begin/exec/query
// failure branches a healthy round trip never reaches.
func TestClosedStoreOperationsReturnErrors(t *testing.T) {
	s := newTestOfferStore(t)
	ctx := context.Background()
	seen := time.Unix(1_700_000_000, 0).UTC()

	if err := s.Put(ctx, validOffer("test:src", "before-close", 1000, seen)); err != nil {
		t.Fatalf("put before close: %v", err)
	}
	id := offer.DeterministicID("test:src", "before-close")

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Run("Put", func(t *testing.T) {
		err := s.Put(ctx, validOffer("test:src", "after-close", 1000, seen))
		if err == nil {
			t.Fatal("expected Put on a closed store to return an error")
		}
	})

	t.Run("Get", func(t *testing.T) {
		got, ok, err := s.Get(ctx, id)
		if err == nil {
			t.Fatalf("expected Get on a closed store to return an error, got offer=%+v ok=%v", got, ok)
		}
		if ok {
			t.Fatal("expected ok=false alongside the error")
		}
	})

	t.Run("Query", func(t *testing.T) {
		got, err := s.Query(ctx, offer.Query{})
		if err == nil {
			t.Fatalf("expected Query on a closed store to return an error, got %v", got)
		}
	})

	t.Run("MarkExpired", func(t *testing.T) {
		n, err := s.MarkExpired(ctx, "test:src", seen.Add(time.Hour), seen.Add(2*time.Hour))
		if err == nil {
			t.Fatalf("expected MarkExpired on a closed store to return an error, got n=%d", n)
		}
	})

	t.Run("ApplyRetention", func(t *testing.T) {
		n, err := s.ApplyRetention(ctx, "test:src", offer.Retention{Policy: offer.Purge, Window: time.Hour}, seen.Add(2*time.Hour))
		if err == nil {
			t.Fatalf("expected ApplyRetention on a closed store to return an error, got n=%d", n)
		}
	})
}

// TestGetScanErrorOnCorruptAspectsJSON reaches the scan-error branch inside
// Get (distinct from the closed-handle QueryContext error and the
// not-found/len==0 branch): a row whose aspects_json column is not valid JSON.
// The only way to produce that row is to bypass Put (which always marshals a
// valid map) and write it directly, which is legitimate here since the test
// lives in-package and is deliberately simulating on-disk corruption /
// out-of-band tampering rather than anything the adapter itself could write.
func TestGetScanErrorOnCorruptAspectsJSON(t *testing.T) {
	s := newTestOfferStore(t)
	ctx := context.Background()

	const id = "corrupt-aspects"
	_, err := s.db.ExecContext(ctx, `
INSERT INTO offers (id, source_id, source_key, aspects_json, first_seen_ns, last_seen_ns)
VALUES (?, ?, ?, ?, ?, ?)`,
		id, "test:src", "k", "{not valid json", 1, 1)
	if err != nil {
		t.Fatalf("seed corrupt row: %v", err)
	}

	_, ok, err := s.Get(ctx, id)
	if err == nil {
		t.Fatal("expected Get to surface the aspects_json decode error")
	}
	if ok {
		t.Fatal("expected ok=false alongside the decode error")
	}
}

// TestPutRoundTripsNonZeroExpiredAt exercises the non-zero branch of both
// nsOrZero (encode) and timeOrZero (decode): every other test in this package
// only ever Puts offers with a zero ExpiredAt (an active offer, or one whose
// ExpiredAt was just reset to zero by Put itself), and MarkExpired sets
// expired_at_ns directly via SQL rather than through Put. Constructing an
// already-expired Offer and Putting it directly is the only way through the
// public API to send a non-zero ExpiredAt into Put's encode path.
func TestPutRoundTripsNonZeroExpiredAt(t *testing.T) {
	s := newTestOfferStore(t)
	ctx := context.Background()

	expiredAt := time.Unix(1_700_000_500, 0).UTC()
	o := validOffer("test:src", "already-expired", 500, expiredAt)
	o.Status = offer.StatusExpired
	o.ExpiredAt = expiredAt

	if err := s.Put(ctx, o); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok, err := s.Get(ctx, offer.DeterministicID("test:src", "already-expired"))
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if !got.ExpiredAt.Equal(expiredAt) {
		t.Fatalf("ExpiredAt = %v, want %v -- nsOrZero/timeOrZero non-zero branch did not round-trip", got.ExpiredAt, expiredAt)
	}
}

// TestPutRejectsInvalidOffer proves Put's Validate guard returns the error
// (rather than proceeding to touch the database at all) for a plainly invalid
// offer.
func TestPutRejectsInvalidOffer(t *testing.T) {
	s := newTestOfferStore(t)
	if err := s.Put(context.Background(), offer.Offer{}); err == nil {
		t.Fatal("expected Put to reject an offer with no SourceID/SourceKey")
	}
}

// TestGetMissingReturnsFalse documents the not-found contract Get must match
// (see offer.Store.Get / the MemoryStore reference): absent, no error.
func TestGetMissingReturnsFalse(t *testing.T) {
	s := newTestOfferStore(t)
	got, ok, err := s.Get(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for a missing id, got %+v", got)
	}
}
