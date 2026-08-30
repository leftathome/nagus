package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/item"
)

// storeopenMkItem builds the minimal valid item.Item needed for a Put/Get
// round trip, mirroring internal/store/sqlitestore's own test helper.
func storeopenMkItem(id string) item.Item {
	return item.Item{
		ID:         id,
		Category:   "hdd",
		Class:      item.ClassDurable,
		Title:      "test item",
		PriceCents: 100,
		Currency:   "USD",
		SourceID:   "test",
		SourceKey:  id,
		SeenAt:     time.Unix(1000, 0),
	}
}

// storeopenUnwritablePath returns a path whose parent path component is a
// regular file (not a directory), so any attempt to create a file under it
// must fail.
func storeopenUnwritablePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}
	return filepath.Join(blocker, "sub", "nagus.db")
}

func TestRegisterStoreFlagsDefaults(t *testing.T) {
	// Clear env so defaults come from the literal fallback values, not
	// whatever happens to be set in the test process's environment.
	for _, k := range []string{
		"NAGUS_STORE_BACKEND", "NAGUS_DB", "NAGUS_PG_HOST", "NAGUS_PG_PORT",
		"NAGUS_PG_DB", "NAGUS_PG_USER", "NAGUS_PG_PASSWORD", "NAGUS_PG_SSLMODE",
	} {
		old, had := os.LookupEnv(k)
		_ = os.Unsetenv(k)
		if had {
			t.Cleanup(func(k, v string) func() {
				return func() { _ = os.Setenv(k, v) }
			}(k, old))
		}
	}

	fs := flag.NewFlagSet("storeopen-defaults", flag.ContinueOnError)
	sf := registerStoreFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}

	if got := *sf.backend; got != "sqlite" {
		t.Fatalf("backend default = %q, want sqlite", got)
	}
	if got := *sf.sqlitePath; got != "nagus.db" {
		t.Fatalf("sqlitePath default = %q, want nagus.db", got)
	}
	if got := *sf.pgHost; got != "postgres-rw.databases-app.svc.cluster.local" {
		t.Fatalf("pgHost default = %q", got)
	}
	if got := *sf.pgPort; got != "5432" {
		t.Fatalf("pgPort default = %q, want 5432", got)
	}
	if got := *sf.pgDB; got != "nagus" {
		t.Fatalf("pgDB default = %q, want nagus", got)
	}
	if got := *sf.pgUser; got != "" {
		t.Fatalf("pgUser default = %q, want empty", got)
	}
	if got := *sf.pgPassword; got != "" {
		t.Fatalf("pgPassword default = %q, want empty", got)
	}
	if got := *sf.pgSSLMode; got != "require" {
		t.Fatalf("pgSSLMode default = %q, want require", got)
	}
}

func TestRegisterStoreFlagsParseOverrides(t *testing.T) {
	fs := flag.NewFlagSet("storeopen-overrides", flag.ContinueOnError)
	sf := registerStoreFlags(fs)

	args := []string{
		"-store-backend", "postgres",
		"-db", "/data/other.db",
		"-pg-host", "pg.example.internal",
		"-pg-port", "6543",
		"-pg-db", "otherdb",
		"-pg-user", "nagus_rw",
		"-pg-password", "s3cret",
		"-pg-sslmode", "disable",
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse(%v): %v", args, err)
	}

	if got := *sf.backend; got != "postgres" {
		t.Fatalf("backend = %q, want postgres", got)
	}
	if got := *sf.sqlitePath; got != "/data/other.db" {
		t.Fatalf("sqlitePath = %q, want /data/other.db", got)
	}
	if got := *sf.pgHost; got != "pg.example.internal" {
		t.Fatalf("pgHost = %q", got)
	}
	if got := *sf.pgPort; got != "6543" {
		t.Fatalf("pgPort = %q", got)
	}
	if got := *sf.pgDB; got != "otherdb" {
		t.Fatalf("pgDB = %q", got)
	}
	if got := *sf.pgUser; got != "nagus_rw" {
		t.Fatalf("pgUser = %q", got)
	}
	if got := *sf.pgPassword; got != "s3cret" {
		t.Fatalf("pgPassword = %q", got)
	}
	if got := *sf.pgSSLMode; got != "disable" {
		t.Fatalf("pgSSLMode = %q", got)
	}
}

func storeopenFlags(t *testing.T, backend, sqlitePath string) *storeFlags {
	t.Helper()
	fs := flag.NewFlagSet("storeopen", flag.ContinueOnError)
	sf := registerStoreFlags(fs)
	args := []string{"-store-backend", backend}
	if sqlitePath != "" {
		args = append(args, "-db", sqlitePath)
	}
	if err := fs.Parse(args); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return sf
}

func TestOpenSqliteSucceedsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nagus.db")
	sf := storeopenFlags(t, "sqlite", path)

	st, closeFn, err := sf.open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if closeFn == nil {
		t.Fatal("open returned a nil close func")
	}
	defer closeFn()

	ctx := context.Background()
	it := storeopenMkItem("storeopen-1")
	if err := st.Put(ctx, it); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := st.Get(ctx, "storeopen-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: item not found after Put")
	}
	if got.ID != it.ID || got.Title != it.Title {
		t.Fatalf("Get = %+v, want id/title matching %+v", got, it)
	}
}

func TestOpenSqliteBadPathErrors(t *testing.T) {
	sf := storeopenFlags(t, "sqlite", storeopenUnwritablePath(t))

	_, closeFn, err := sf.open(context.Background())
	if err == nil {
		t.Fatal("open: expected error for a path whose parent is a file")
	}
	if closeFn == nil {
		t.Fatal("open must return a non-nil close func even on error")
	}
	closeFn() // must be a safe no-op
}

func TestOpenUnknownBackendErrors(t *testing.T) {
	sf := storeopenFlags(t, "carrier-pigeon", "")

	_, closeFn, err := sf.open(context.Background())
	if err == nil {
		t.Fatal("open: expected error for an unknown backend")
	}
	if closeFn == nil {
		t.Fatal("open must return a non-nil close func even on error")
	}
	closeFn()
}

func TestOpenOffersDisabledReturnsNilStoreNoError(t *testing.T) {
	sf := storeopenFlags(t, "sqlite", filepath.Join(t.TempDir(), "nagus.db"))

	os_, closeFn, err := sf.openOffers(context.Background(), "", false)
	if err != nil {
		t.Fatalf("openOffers(disabled): unexpected error: %v", err)
	}
	if os_ != nil {
		t.Fatalf("openOffers(disabled): store = %v, want nil", os_)
	}
	if closeFn == nil {
		t.Fatal("openOffers(disabled) must return a non-nil close func")
	}
	closeFn() // must be a safe no-op
}

func TestOpenOffersSqliteSucceeds(t *testing.T) {
	sf := storeopenFlags(t, "sqlite", filepath.Join(t.TempDir(), "nagus.db"))
	offersPath := filepath.Join(t.TempDir(), "offers.db")

	os_, closeFn, err := sf.openOffers(context.Background(), offersPath, true)
	if err != nil {
		t.Fatalf("openOffers: %v", err)
	}
	if os_ == nil {
		t.Fatal("openOffers(enabled, sqlite): store = nil, want a working store")
	}
	if closeFn == nil {
		t.Fatal("openOffers must return a non-nil close func")
	}
	closeFn()
}

func TestOpenOffersSqliteEmptyPathErrors(t *testing.T) {
	sf := storeopenFlags(t, "sqlite", filepath.Join(t.TempDir(), "nagus.db"))

	_, closeFn, err := sf.openOffers(context.Background(), "", true)
	if err == nil {
		t.Fatal("openOffers(enabled, empty path): expected error")
	}
	closeFn()
}

func TestOpenOffersSqliteBadPathErrors(t *testing.T) {
	sf := storeopenFlags(t, "sqlite", filepath.Join(t.TempDir(), "nagus.db"))

	_, closeFn, err := sf.openOffers(context.Background(), storeopenUnwritablePath(t), true)
	if err == nil {
		t.Fatal("openOffers(enabled, bad path): expected error")
	}
	closeFn()
}

func TestOpenOffersUnknownBackendErrors(t *testing.T) {
	sf := storeopenFlags(t, "carrier-pigeon", "")

	_, closeFn, err := sf.openOffers(context.Background(), filepath.Join(t.TempDir(), "offers.db"), true)
	if err == nil {
		t.Fatal("openOffers(enabled, unknown backend): expected error")
	}
	closeFn()
}

func TestOpenPostgresUnreachableSurfacesError(t *testing.T) {
	// No postgres server is available in this environment, so this only
	// exercises that the postgres branch in open() is reached and its error
	// is wrapped with the item-store context -- not that a real connection
	// succeeds.
	sf := storeopenFlags(t, "postgres", "")
	*sf.pgHost = "127.0.0.1"
	*sf.pgPort = "1" // reserved/unassigned port: connection must fail fast-ish

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, closeFn, err := sf.open(ctx)
	if err == nil {
		t.Fatal("open(postgres, unreachable): expected error")
	}
	if closeFn == nil {
		t.Fatal("open must return a non-nil close func even on error")
	}
	closeFn()
}

func TestOpenOffersPostgresBadDSNSurfacesError(t *testing.T) {
	// No postgres server is available in this environment, so this only
	// exercises that the postgres branch is reached and its error is wrapped
	// with the offer-store context -- not that a real connection succeeds.
	sf := storeopenFlags(t, "postgres", "")
	*sf.pgHost = "127.0.0.1"
	*sf.pgPort = "1" // reserved/unassigned port: connection must fail fast-ish

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, closeFn, err := sf.openOffers(ctx, "", true)
	if err == nil {
		t.Fatal("openOffers(postgres, unreachable): expected error")
	}
	closeFn()
}
