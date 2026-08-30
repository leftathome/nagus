package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/store"
)

// TestNewFailsWhenParentIsNotADirectory exercises New's failure path when the
// underlying file cannot be opened at all: the DSN's parent path component is
// a regular file, not a directory, so SQLite cannot create the database file
// under it. sql.Open itself never errors here (modernc.org/sqlite only
// implements driver.Driver, so database/sql defers dialing until first use),
// so the failure surfaces at the first real access -- the busy_timeout
// PRAGMA -- which is exactly the branch this test is after.
func TestNewFailsWhenParentIsNotADirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	dsn := filepath.Join(blocker, "db.sqlite")

	s, err := New(dsn)
	if err == nil {
		s.Close()
		t.Fatal("expected New to fail when the DSN's parent is a regular file")
	}
}

// TestNewFailsOnCorruptFile proves New surfaces an error rather than silently
// "succeeding" against a file that exists but is not a valid SQLite database
// -- e.g. leftover garbage from a truncated write or an unrelated file at that
// path.
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

// TestMigrateFailsAfterClose calls the unexported migrate step directly
// against an already-closed handle, which is the cleanest way to force its
// schema Exec to fail without corrupting a live database mid-test.
func TestMigrateFailsAfterClose(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.migrate(); err == nil {
		t.Fatal("expected migrate to fail against a closed database handle")
	}
}

// TestClosedStoreOperationsReturnErrors drives every Store method against a
// Store whose handle has already been Close'd. The documented contract is an
// error return, never a panic, so this both proves that and exercises the
// begin/exec/query failure branches that a healthy round trip never reaches.
func TestClosedStoreOperationsReturnErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Put one item while the store is still open so Get/Search/DeleteStale
	// below have something to (fail to) act on, in case a bug ever turned a
	// "should error" path into a silent no-op.
	if err := s.Put(ctx, mkItem("before-close", "hdd", 100, time.Unix(1, 0), "still here")); err != nil {
		t.Fatalf("put before close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	t.Run("Put", func(t *testing.T) {
		err := s.Put(ctx, mkItem("after-close", "hdd", 100, time.Unix(2, 0), "too late"))
		if err == nil {
			t.Fatal("expected Put on a closed store to return an error")
		}
	})

	t.Run("Get", func(t *testing.T) {
		got, ok, err := s.Get(ctx, "before-close")
		if err == nil {
			t.Fatalf("expected Get on a closed store to return an error, got item=%+v ok=%v", got, ok)
		}
		if ok {
			t.Fatal("expected ok=false alongside the error")
		}
	})

	t.Run("Search", func(t *testing.T) {
		got, err := s.Search(ctx, store.Query{})
		if err == nil {
			t.Fatalf("expected Search on a closed store to return an error, got %v", got)
		}
	})

	t.Run("DeleteStale", func(t *testing.T) {
		n, err := s.DeleteStale(ctx, "test", time.Now())
		if err == nil {
			t.Fatalf("expected DeleteStale on a closed store to return an error, got n=%d", n)
		}
	})
}

// TestGetClosedStoreErrorIsNotErrNoRows proves the closed-handle error taken
// by Get is the generic wrapped-error branch, not the "not found" branch --
// the two must stay distinguishable so callers cannot mistake "database is
// unusable" for "item does not exist".
func TestGetClosedStoreErrorIsNotErrNoRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, ok, err := s.Get(context.Background(), "anything")
	if err == nil {
		t.Fatal("expected an error")
	}
	if ok {
		t.Fatal("expected ok=false")
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("closed-store error must not look like a not-found result: %v", err)
	}
}
