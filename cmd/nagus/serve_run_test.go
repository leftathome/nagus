package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// wiringFreeAddr claims an ephemeral TCP port on loopback and immediately
// releases it, returning the address for runServe's -listen flag. There is a
// small window where another process could grab the same port before
// runServe binds it; this is the standard workaround for the fact that
// http.Server.ListenAndServe never reports back which port it bound (there is
// no seam to intercept that without changing production code).
func wiringFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr
}

// wiringWaitForHealthy polls /healthz until it answers 200 or deadline
// elapses, so tests never depend on a fixed sleep to know the server is up.
func wiringWaitForHealthy(t *testing.T, addr string, deadline time.Duration) {
	t.Helper()
	cutoff := time.Now().Add(deadline)
	var lastErr error
	for time.Now().Before(cutoff) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become healthy within %s: %v", addr, deadline, lastErr)
}

// --- runServe: error/validation paths (no network, no listener started) ---

func TestWiringRunServeFlagParseErrorPropagates(t *testing.T) {
	if err := runServe([]string{"-not-a-real-flag"}); err == nil {
		t.Fatal("expected a flag-parse error")
	}
}

func TestWiringRunServeBadWatchesPathErrors(t *testing.T) {
	err := runServe([]string{
		"-category", "hdd",
		"-db", filepath.Join(t.TempDir(), "nagus.db"),
		"-watches", filepath.Join(t.TempDir(), "does-not-exist.json"),
	})
	if err == nil {
		t.Fatal("expected an error for a missing watches config file")
	}
}

func TestWiringRunServeBadStorePathErrors(t *testing.T) {
	err := runServe([]string{"-category", "hdd", "-db", storeopenUnwritablePath(t)})
	if err == nil {
		t.Fatal("expected a store-open error for an unwritable db path")
	}
}

func TestWiringRunServeOffersEnabledBadPathErrors(t *testing.T) {
	err := runServe([]string{
		"-category", "hdd",
		"-db", filepath.Join(t.TempDir(), "nagus.db"),
		"-offers-db", storeopenUnwritablePath(t),
	})
	if err == nil {
		t.Fatal("expected an offer-store open error for an unwritable offers db path")
	}
}

func TestWiringRunServeConfigLoadErrorPropagates(t *testing.T) {
	err := runServe([]string{
		"-db", filepath.Join(t.TempDir(), "nagus.db"),
		"-config", filepath.Join(t.TempDir(), "does-not-exist.json"),
	})
	if err == nil {
		t.Fatal("expected an error for a missing -config file")
	}
}

// TestWiringRunServeConfigUnsupportedCategoryBuildSurfaceErrors covers the
// runServe branch that builds one Surface per configured category and
// propagates buildSurface's error. "ghost" is a valid key under categories{}
// but is never referenced by any source, so LoadRunConfig's per-source
// category validation never rejects it -- only runServe's per-category
// buildSurface loop does.
func TestWiringRunServeConfigUnsupportedCategoryBuildSurfaceErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := `{"sources":[],"categories":{"hdd":{"minCapacityTB":6},"ghost":{}}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runServe([]string{
		"-db", filepath.Join(dir, "nagus.db"),
		"-config", cfgPath,
	})
	if err == nil {
		t.Fatal("expected buildSurface to reject the unsupported 'ghost' category")
	}
}

// TestWiringRunServeConfigBuildIngesterErrorPropagates covers the runServe
// branch that builds one Ingester per configured source and propagates
// buildIngester's error -- here, an eBay source with neither a fixture nor
// live credentials.
func TestWiringRunServeConfigBuildIngesterErrorPropagates(t *testing.T) {
	t.Setenv("NAGUS_EBAY_CLIENT_ID", "")
	t.Setenv("NAGUS_EBAY_CLIENT_SECRET", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := `{
	  "sources": [{"name":"ebay-src","category":"hdd","type":"ebay","intervalMinutes":30}],
	  "categories": {"hdd":{"minCapacityTB":6}}
	}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runServe([]string{
		"-db", filepath.Join(dir, "nagus.db"),
		"-config", cfgPath,
	})
	if err == nil {
		t.Fatal("expected buildIngester to fail: no fixture and no live eBay credentials")
	}
}

// TestWiringRunServeHappyPathServesAndShutsDownOnSignal drives runServe's
// full happy path: parse flags, open a real sqlite store, build the hdd
// surface, start the HTTP listener, answer one real request over the
// loopback network, then shut down cleanly on SIGTERM -- the same signal
// signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM) is wired to
// inside runServe.
//
// Sending a real SIGTERM to the test process is only safe because runServe
// registers that signal handler (via signal.NotifyContext) BEFORE it starts
// the HTTP listener goroutine; waiting for /healthz to answer 200 therefore
// guarantees the handler is already armed, so this signal cannot fall through
// to the default terminate-the-process disposition. There is no other seam
// (no context parameter, no injectable listener) to drive this function's
// happy path without touching a real OS signal and a real TCP port.
func TestWiringRunServeHappyPathServesAndShutsDownOnSignal(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nagus.db")
	addr := wiringFreeAddr(t)

	args := []string{
		"-category", "hdd",
		"-db", dbPath,
		"-listen", addr,
		"-offline", // no live reference-feed network call
	}

	errc := make(chan error, 1)
	go func() { errc <- runServe(args) }()

	wiringWaitForHealthy(t, addr, 5*time.Second)

	resp, err := http.Get("http://" + addr + "/search?category=hdd")
	if err != nil {
		t.Fatalf("GET /search: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/search status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Matched int `json:"matched"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /search body: %v", err)
	}
	if body.Matched != 0 {
		t.Fatalf("matched = %d, want 0 (freshly created, empty store)", body.Matched)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("runServe returned an error after the shutdown signal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe did not return within 5s of the shutdown signal")
	}
}
