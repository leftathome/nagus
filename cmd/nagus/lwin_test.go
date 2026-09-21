package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/store"
)

const lwinCSV3 = "LWIN,STATUS,PRODUCER_NAME,WINE,COUNTRY,REGION,COLOUR,TYPE\n" +
	"1101245,Live,Leonetti Cellar,Cabernet Sauvignon,United States,Walla Walla,Red,Wine\n" +
	"1234567,Live,Harbinger Winery,El Jefe,United States,Washington,Red,Wine\n" +
	"7654321,Live,Robert Mondavi Winery,Cabernet Sauvignon,United States,California,Red,Wine\n"

// lwinPublisher is a fake Liv-ex: serves the export with an ETag, honours
// If-None-Match, can go down, and counts full downloads.
type lwinPublisher struct {
	body, etag atomic.Value
	down       atomic.Bool
	downloads  atomic.Int32
}

func newLWINPublisher(t *testing.T, body string) (*lwinPublisher, *httptest.Server) {
	p := &lwinPublisher{}
	p.body.Store(body)
	p.etag.Store(`"v1"`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		etag := p.etag.Load().(string)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		p.downloads.Add(1)
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(p.body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

func mirroredSource(t *testing.T, srv *httptest.Server, maxAge time.Duration) *lwinSource {
	s := &lwinSource{logf: t.Logf, minRecords: 1}
	s.mirror = s.newMirror(srv.URL, filepath.Join(t.TempDir(), "lwin", "export.csv"), maxAge, srv.Client())
	return s
}

func waitLoaded(t *testing.T, s *lwinSource) {
	t.Helper()
	select {
	case <-s.loaded:
	case <-time.After(10 * time.Second):
		t.Fatal("background LWIN load did not finish")
	}
}

// Every wine consumer shares ONE resolver and ONE loaded dictionary: the real
// dictionary is ~55 MiB, and wineDepsFrom runs per wine source plus once for
// the surface.
func TestLWINMirrorIsLoadedOnceAndShared(t *testing.T) {
	pub, srv := newLWINPublisher(t, lwinCSV3)
	s := mirroredSource(t, srv, time.Hour)
	o := categoryOpts{lwin: s}
	a, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), o)
	if err != nil {
		t.Fatalf("wineDepsFrom: %v", err)
	}
	b, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), o)
	if err != nil {
		t.Fatal(err)
	}
	if a.LWIN == nil || a.LWIN != b.LWIN {
		t.Fatalf("consumers got different resolvers (%p, %p): the dictionary would load per consumer", a.LWIN, b.LWIN)
	}
	if a.LWINStamp {
		t.Fatal("stamping must default OFF (shadow) until false matches are measured")
	}
	waitLoaded(t, s)
	if a.LWIN.Len() != 3 || pub.downloads.Load() != 1 {
		t.Fatalf("records=%d downloads=%d, want 3 and 1", a.LWIN.Len(), pub.downloads.Load())
	}
}

// A publisher outage at first start must not fail startup: nagus runs without
// LWIN and says so, and hdd surfacing is unaffected.
func TestLWINMirrorOutageDoesNotFailStartup(t *testing.T) {
	pub, srv := newLWINPublisher(t, lwinCSV3)
	pub.down.Store(true)
	s := mirroredSource(t, srv, time.Hour)
	deps, err := wineDepsFrom(CategoryConfig{}, store.NewMemoryStore(), categoryOpts{lwin: s})
	if err != nil {
		t.Fatalf("an unreachable publisher failed startup: %v", err)
	}
	waitLoaded(t, s)
	if deps.LWIN.Len() != 0 || s.refreshFailures.Load() == 0 {
		t.Fatalf("records=%d failures=%d, want 0 and >0", deps.LWIN.Len(), s.refreshFailures.Load())
	}
	var buf bytes.Buffer
	writeLWINMetrics(&buf, s)
	if !strings.Contains(buf.String(), "nagus_lwin_records 0\n") {
		t.Fatalf("metrics must report no dictionary:\n%s", buf.String())
	}

	// The publisher recovers; the next refresh loads it.
	pub.down.Store(false)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh after recovery: %v", err)
	}
	if deps.LWIN.Len() != 3 {
		t.Fatalf("after recovery records=%d, want 3", deps.LWIN.Len())
	}
}

// A new export at the source is swapped in while serving, through the same
// resolver every consumer already holds.
func TestLWINRefreshSwapsANewExportIn(t *testing.T) {
	pub, srv := newLWINPublisher(t, lwinCSV3)
	s := mirroredSource(t, srv, 0) // MaxAge 0: consult the source on every refresh
	r, _ := s.get(context.Background())
	waitLoaded(t, s)

	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pub.downloads.Load() != 1 {
		t.Fatalf("an unchanged export was downloaded again (%d downloads)", pub.downloads.Load())
	}

	pub.body.Store(lwinCSV3 + "1111111,Live,Leonetti Cellar,Merlot,United States,Walla Walla,Red,Wine\n")
	pub.etag.Store(`"v2"`)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 4 {
		t.Fatalf("resolver holds %d records after the export changed, want 4", r.Len())
	}
}

// A download that parses too small never replaces a good mirror.
func TestLWINMirrorRejectsATruncatedExport(t *testing.T) {
	pub, srv := newLWINPublisher(t, lwinCSV3)
	s := mirroredSource(t, srv, 0)
	r, _ := s.get(context.Background())
	waitLoaded(t, s)

	s.minRecords = 3
	pub.body.Store("LWIN,PRODUCER_NAME\n1101245,Leonetti Cellar\n")
	pub.etag.Store(`"truncated"`)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("a rejected download with a good mirror should keep serving, got %v", err)
	}
	if r.Len() != 3 {
		t.Fatalf("records=%d: a truncated export replaced the good dictionary", r.Len())
	}
}
