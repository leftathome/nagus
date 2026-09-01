package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/store"
	"github.com/leftathome/nagus/internal/store/sqlitestore"
)

// Fixtures reused from sibling connector packages, exactly as the existing
// serve_test.go/config_test.go tests reference them from cmd/nagus.
const (
	wiringEbayFixture    = "../../internal/connector/ebay/testdata/browse_search.json"
	wiringZillapiFixture = "../../internal/connector/zillapi/testdata/search_lots.json"
	wiringShopifyFixture = "../../internal/connector/shopify/testdata/products.json"
)

// wiringCaptureStderr mirrors pureCaptureStdout (emit_test.go) but for
// os.Stderr, needed because usage() writes there directly rather than through
// an injected io.Writer.
func wiringCaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return buf.String()
}

// --- usage ---

func TestWiringUsageWritesExpectedContentToStderr(t *testing.T) {
	out := wiringCaptureStderr(t, usage)
	for _, want := range []string{
		"nagus -- acquisition/watch spine",
		"nagus version",
		"nagus ingest",
		"nagus search",
		"nagus serve",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("usage() output missing %q, got: %q", want, out)
		}
	}
}

// --- buildEbayConnector ---

func TestWiringBuildEbayConnectorFixtureNeedsNoCreds(t *testing.T) {
	conn, err := buildEbayConnector("wire-src", wiringEbayFixture, "", "", "internal hard drive", "EBAY_US", 50)
	if err != nil {
		t.Fatalf("fixture-backed build must not require credentials: %v", err)
	}
	if got := conn.SourceID(); got != "ebay:wire-src" {
		t.Errorf("SourceID() = %q, want ebay:wire-src", got)
	}
}

func TestWiringBuildEbayConnectorMissingCredsErrors(t *testing.T) {
	cases := []struct {
		name, id, secret string
	}{
		{"missing both", "", ""},
		{"missing secret only", "cid", ""},
		{"missing id only", "", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildEbayConnector("wire-live", "", tc.id, tc.secret, "q", "EBAY_US", 10)
			if err == nil {
				t.Fatal("expected an error when live credentials are incomplete and no fixture is set")
			}
		})
	}
}

func TestWiringBuildEbayConnectorLiveCredsWithSellerProfile(t *testing.T) {
	// No network call happens at construction time (NewConnector/
	// NewShoppingProfileSource are pure config assembly), so this exercises the
	// sandbox + seller-profile + daily-budget env branches without touching
	// the network.
	t.Setenv("NAGUS_EBAY_SANDBOX", "true")
	t.Setenv("NAGUS_EBAY_DAILY_BUDGET", "1234")
	t.Setenv("NAGUS_EBAY_SELLER_PROFILE", "true")

	conn, err := buildEbayConnector("wire-live", "", "cid", "secret", "internal hard drive", "EBAY_US", 10)
	if err != nil {
		t.Fatalf("live build with full creds: %v", err)
	}
	if got := conn.SourceID(); got != "ebay:wire-live" {
		t.Errorf("SourceID() = %q, want ebay:wire-live", got)
	}
}

func TestWiringBuildEbayConnectorLiveCredsWithoutSellerProfile(t *testing.T) {
	t.Setenv("NAGUS_EBAY_SANDBOX", "")
	t.Setenv("NAGUS_EBAY_DAILY_BUDGET", "")
	t.Setenv("NAGUS_EBAY_SELLER_PROFILE", "")

	conn, err := buildEbayConnector("wire-plain", "", "cid", "secret", "internal hard drive", "EBAY_US", 10)
	if err != nil {
		t.Fatalf("live build with full creds: %v", err)
	}
	if conn == nil {
		t.Fatal("buildEbayConnector returned a nil connector with no error")
	}
}

// --- buildConnectorForSource ---

func TestWiringBuildConnectorForSourceEbay(t *testing.T) {
	conn, err := buildConnectorForSource(
		SourceConfig{Name: "src", Type: "ebay", Fixture: wiringEbayFixture},
		CategoryConfig{}, categoryOpts{},
	)
	if err != nil {
		t.Fatalf("buildConnectorForSource(ebay): %v", err)
	}
	if got := conn.SourceID(); got != "ebay:src" {
		t.Errorf("SourceID() = %q, want ebay:src", got)
	}
}

func TestWiringBuildConnectorForSourceEbayErrorNamesSource(t *testing.T) {
	_, err := buildConnectorForSource(SourceConfig{Name: "unhappy-src", Type: "ebay"}, CategoryConfig{}, categoryOpts{})
	if err == nil {
		t.Fatal("expected an error: no fixture and no credentials")
	}
	if !strings.Contains(err.Error(), "unhappy-src") {
		t.Errorf("err = %v, want it to name the source", err)
	}
}

func TestWiringBuildConnectorForSourceZillapi(t *testing.T) {
	conn, err := buildConnectorForSource(
		SourceConfig{Name: "land-src", Type: "zillapi", Fixture: wiringZillapiFixture},
		CategoryConfig{MinAcreageAcres: 1}, categoryOpts{},
	)
	if err != nil {
		t.Fatalf("buildConnectorForSource(zillapi): %v", err)
	}
	if got := conn.SourceID(); got != "zillapi:land-src" {
		t.Errorf("SourceID() = %q, want zillapi:land-src", got)
	}
}

func TestWiringBuildConnectorForSourceShopify(t *testing.T) {
	conn, err := buildConnectorForSource(
		SourceConfig{Name: "spd", Type: "shopify", Fixture: wiringShopifyFixture},
		CategoryConfig{}, categoryOpts{},
	)
	if err != nil {
		t.Fatalf("buildConnectorForSource(shopify): %v", err)
	}
	if got := conn.SourceID(); got != "shopify:spd" {
		t.Errorf("SourceID() = %q, want shopify:spd", got)
	}
}

func TestWiringBuildConnectorForSourceUnsupportedTypeErrors(t *testing.T) {
	_, err := buildConnectorForSource(SourceConfig{Name: "x", Type: "carrier-pigeon"}, CategoryConfig{}, categoryOpts{})
	if err == nil {
		t.Fatal("expected an error for an unsupported source type")
	}
}

// --- buildSurface ---

func TestWiringBuildSurfaceHDDOffline(t *testing.T) {
	sf, err := buildSurface("hdd", CategoryConfig{MinCapacityTB: 6}, store.NewMemoryStore(), categoryOpts{hddOffline: true})
	if err != nil {
		t.Fatalf("buildSurface(hdd): %v", err)
	}
	if sf == nil {
		t.Fatal("buildSurface(hdd) returned a nil surface with no error")
	}
}

func TestWiringBuildSurfaceLand(t *testing.T) {
	sf, err := buildSurface("land", CategoryConfig{MinAcreageAcres: 1}, store.NewMemoryStore(), categoryOpts{})
	if err != nil {
		t.Fatalf("buildSurface(land): %v", err)
	}
	if sf == nil {
		t.Fatal("buildSurface(land) returned a nil surface with no error")
	}
}

func TestWiringBuildSurfaceLandWithRentcastKey(t *testing.T) {
	// Exercises the branch that wires a parcel provider; NewRentcastProvider
	// does no network I/O at construction time.
	sf, err := buildSurface("land", CategoryConfig{MinAcreageAcres: 1}, store.NewMemoryStore(),
		categoryOpts{rentcastKey: "rk_test", http: http.DefaultClient})
	if err != nil {
		t.Fatalf("buildSurface(land, rentcastKey set): %v", err)
	}
	if sf == nil {
		t.Fatal("buildSurface(land, rentcastKey set) returned a nil surface with no error")
	}
}

func TestWiringBuildSurfaceUnsupportedCategoryErrors(t *testing.T) {
	_, err := buildSurface("ghost", CategoryConfig{}, store.NewMemoryStore(), categoryOpts{})
	if err == nil {
		t.Fatal("expected an error for an unsupported category")
	}
}

// --- runIngest ---

func TestWiringRunIngestUnsupportedCategoryErrors(t *testing.T) {
	if err := runIngest([]string{"-category", "ghost"}); err == nil {
		t.Fatal("expected an error for an unsupported category")
	}
}

func TestWiringRunIngestLandCategoryErrorsWithConfigGuidance(t *testing.T) {
	err := runIngest([]string{"-category", "land"})
	if err == nil {
		t.Fatal("expected land ingest to be rejected on the legacy CLI path")
	}
	if !strings.Contains(err.Error(), "config.json") {
		t.Errorf("err = %v, want it to point at the config.json multi-source path", err)
	}
}

func TestWiringRunIngestFlagParseErrorPropagates(t *testing.T) {
	if err := runIngest([]string{"-not-a-real-flag"}); err == nil {
		t.Fatal("expected a flag-parse error")
	}
}

func TestWiringRunIngestBadStorePathErrors(t *testing.T) {
	err := runIngest([]string{"-category", "hdd", "-db", storeopenUnwritablePath(t)})
	if err == nil {
		t.Fatal("expected a store-open error for an unwritable db path")
	}
}

func TestWiringRunIngestMissingCredsAndNoFixtureErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nagus.db")
	err := runIngest([]string{"-category", "hdd", "-db", dbPath})
	if err == nil {
		t.Fatal("expected an error: no -ebay-fixture and no -client-id/-client-secret")
	}
}

// TestWiringRunIngestFixtureSucceedsAndPersists drives the whole front-half
// spine (connector -> sanitize -> extract -> store) through runIngest against
// a real sqlite file, then reopens that same file with a fresh
// sqlitestore.New to prove the data actually landed on disk (not just held in
// an in-memory handle) -- the "evidence over absence of error" habit from
// .agent/system: a printed "stored=N" line is not proof by itself.
func TestWiringRunIngestFixtureSucceedsAndPersists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nagus.db")

	out := pureCaptureStdout(t, func() {
		if err := runIngest([]string{"-category", "hdd", "-db", dbPath, "-ebay-fixture", wiringEbayFixture}); err != nil {
			t.Fatalf("runIngest: %v", err)
		}
	})
	if !strings.Contains(out, "ingest[hdd]:") || !strings.Contains(out, "backend=sqlite") {
		t.Fatalf("runIngest stdout missing expected summary line, got: %q", out)
	}

	st, err := sqlitestore.New(dbPath)
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	defer func() { _ = st.Close() }()

	items, err := st.Search(context.Background(), store.Query{Category: "hdd"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("runIngest reported success but no items are readable from a freshly reopened store")
	}
}

// --- runSearch ---

func TestWiringRunSearchUnsupportedCategoryErrors(t *testing.T) {
	if err := runSearch([]string{"-category", "ghost"}); err == nil {
		t.Fatal("expected an error for an unsupported category")
	}
}

func TestWiringRunSearchFlagParseErrorPropagates(t *testing.T) {
	if err := runSearch([]string{"-not-a-real-flag"}); err == nil {
		t.Fatal("expected a flag-parse error")
	}
}

func TestWiringRunSearchBadStorePathErrors(t *testing.T) {
	err := runSearch([]string{"-category", "hdd", "-db", storeopenUnwritablePath(t)})
	if err == nil {
		t.Fatal("expected a store-open error for an unwritable db path")
	}
}

// wiringSeedHDDStore runs one real ingest pass against the ebay fixture into
// a fresh sqlite file at dbPath, so runSearch tests exercise the same
// persisted-then-reopened store an operator's ingest/search pair would use.
func wiringSeedHDDStore(t *testing.T, dbPath string) {
	t.Helper()
	if err := runIngest([]string{"-category", "hdd", "-db", dbPath, "-ebay-fixture", wiringEbayFixture}); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
}

func TestWiringRunSearchTableOutput(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nagus.db")
	wiringSeedHDDStore(t, dbPath)

	out := pureCaptureStdout(t, func() {
		// -offline is required: without it the surface would hit the live
		// reference-product feed over the network, which is not allowed here.
		if err := runSearch([]string{"-category", "hdd", "-db", dbPath, "-offline"}); err != nil {
			t.Fatalf("runSearch (table): %v", err)
		}
	})
	if !strings.Contains(out, "search[hdd]: matched=") {
		t.Fatalf("table output missing summary line, got: %q", out)
	}
	if !strings.Contains(out, "VERDICT") || !strings.Contains(out, "TITLE") {
		t.Fatalf("table output missing header columns, got: %q", out)
	}
}

func TestWiringRunSearchJSONOutput(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nagus.db")
	wiringSeedHDDStore(t, dbPath)

	out := pureCaptureStdout(t, func() {
		if err := runSearch([]string{"-category", "hdd", "-db", dbPath, "-offline", "-json"}); err != nil {
			t.Fatalf("runSearch (json): %v", err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("runSearch -json output not valid JSON: %v\noutput: %q", err, out)
	}
	if len(rows) == 0 {
		t.Fatal("expected at least one row in JSON output from the seeded fixture")
	}
}

func TestWiringRunSearchTextFilterNoMatches(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nagus.db")
	wiringSeedHDDStore(t, dbPath)

	out := pureCaptureStdout(t, func() {
		if err := runSearch([]string{
			"-category", "hdd", "-db", dbPath, "-offline",
			"-text", "zzz-nothing-should-ever-match-this-string",
		}); err != nil {
			t.Fatalf("runSearch: %v", err)
		}
	})
	if !strings.Contains(out, "matched=0") {
		t.Fatalf("expected matched=0 for a non-matching text filter, got: %q", out)
	}
}
