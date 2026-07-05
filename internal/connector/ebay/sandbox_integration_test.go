//go:build ebayintegration

package ebay

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSandboxFetch_Live exercises the real eBay Sandbox Browse API end-to-end
// (OAuth client-credentials -> item_summary/search) using sandbox Application
// Keys. It is deliberately gated so the default `go test ./...` stays fully
// offline:
//
//   - Compiled only under the `ebayintegration` build tag:
//     go test -tags ebayintegration ./internal/connector/ebay/
//   - Skipped unless sandbox creds are present in the environment. In CI/dev they
//     come from Vault eso/nagus/testing, exported as
//     NAGUS_EBAY_SANDBOX_CLIENT_ID / NAGUS_EBAY_SANDBOX_CLIENT_SECRET.
//
// Sandbox use is a separate test environment (License 8.4): it does NOT spend the
// production daily call budget and is not circumvention. Do not publish PII or
// restricted data to the sandbox.
func TestSandboxFetch_Live(t *testing.T) {
	id := os.Getenv("NAGUS_EBAY_SANDBOX_CLIENT_ID")
	secret := os.Getenv("NAGUS_EBAY_SANDBOX_CLIENT_SECRET")
	if id == "" || secret == "" {
		t.Skip("set NAGUS_EBAY_SANDBOX_CLIENT_ID/SECRET (from Vault eso/nagus/testing) to run the sandbox integration test")
	}

	c := NewConnector(Config{
		Sandbox:      true,
		ClientID:     id,
		ClientSecret: secret,
		Query:        "hard drive",
		Limit:        5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raws, err := c.Fetch(ctx)
	if err != nil {
		t.Fatalf("sandbox Fetch: %v", err)
	}
	// Sandbox inventory is sparse; a successful round-trip (auth + search decode)
	// is the signal, not a specific count.
	t.Logf("sandbox Fetch OK: %d listings; budget=%+v", len(raws), c.BudgetStats())
}

// TestSandboxProfile_Live validates the ShoppingProfileSource auth path (the
// GetUserProfile IAF-token header) against the real sandbox. Sandbox test sellers
// have no profile data, so found is typically false -- the signal is that the
// call authenticates cleanly (no error 1.33), which the appid-only auth did not.
func TestSandboxProfile_Live(t *testing.T) {
	id := os.Getenv("NAGUS_EBAY_SANDBOX_CLIENT_ID")
	secret := os.Getenv("NAGUS_EBAY_SANDBOX_CLIENT_SECRET")
	if id == "" || secret == "" {
		t.Skip("set NAGUS_EBAY_SANDBOX_CLIENT_ID/SECRET to run the sandbox profile test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := NewConnector(Config{Sandbox: true, ClientID: id, ClientSecret: secret, Query: "hard drive", Limit: 5})
	tok, err := c.token(ctx)
	if err != nil {
		t.Fatalf("sandbox token: %v", err)
	}
	src := NewShoppingProfileSource(id, true, nil)
	_, found, err := src.Profile(ctx, "esandbox10539", tok)
	if err != nil {
		t.Fatalf("sandbox GetUserProfile auth path: %v", err)
	}
	t.Logf("sandbox GetUserProfile authenticated OK (found=%v)", found)
}
