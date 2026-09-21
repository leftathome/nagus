package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/pipeline"
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

// nagus-0k0: a wine source's first ingest waits for the dictionary, so a
// restart does not extract every wine item against an empty one (and then
// wait 12h to redo it). Other categories never wait.
func TestWineIngestGateHoldsTheFirstPassUntilLoaded(t *testing.T) {
	s := &lwinSource{logf: t.Logf, loaded: make(chan struct{})}
	gates := wineIngestGates([]SourceConfig{
		{Name: "spd", Category: "hdd"},
		{Name: "harbinger", Category: "wine"},
	}, s, time.Minute, t.Logf)
	if gates[0] != nil || gates[1] == nil {
		t.Fatalf("gates = %v; only the wine source should wait", gates)
	}

	conn := &loopFakeConnector{id: "harbinger"}
	ing := &pipeline.Ingester{Connector: conn}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSourceIngestLoop(ctx, ing, time.Hour, gates[1])

	time.Sleep(50 * time.Millisecond)
	if conn.calls.Load() != 0 {
		t.Fatal("the wine source ingested before the dictionary loaded")
	}
	close(s.loaded)
	loopWaitForCount(t, 2*time.Second, 1, conn.calls.Load)
}

// The wait is bounded: a dictionary that never loads delays the first pass by
// at most max, never forever.
func TestWineIngestGateGivesUpAtItsLimit(t *testing.T) {
	s := &lwinSource{logf: t.Logf, loaded: make(chan struct{})} // never closed
	gate := wineIngestGates([]SourceConfig{{Name: "w", Category: "wine"}}, s, 20*time.Millisecond, t.Logf)[0]
	start := time.Now()
	gate(context.Background())
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("gate waited %s past a 20ms limit", waited)
	}
	if wineIngestGates([]SourceConfig{{Name: "w", Category: "wine"}}, nil, time.Minute, t.Logf) != nil {
		t.Fatal("with LWIN off there must be no gates")
	}
}

// nagus-a8t: a source stamps only when it has opted in, even with stamping on
// globally, so a newly added source starts in shadow until reviewed.
func TestWineSourceStampsOnlyWhenOptedIn(t *testing.T) {
	s := &lwinSource{localPath: filepath.Join(t.TempDir(), "lwin.csv"), stamp: true, logf: t.Logf}
	if err := os.WriteFile(s.localPath, []byte(lwinCSV3), 0o600); err != nil {
		t.Fatal(err)
	}
	o := categoryOpts{lwin: s}
	for _, tc := range []struct {
		optIn bool
		want  bool
	}{{false, false}, {true, true}} {
		src := SourceConfig{Name: "w", Category: "wine", Type: "shopify", BaseURL: "https://example.test",
			WineChannel: "producer", Origin: "US-CA", WineProducer: "Leonetti Cellar", LWINStamp: tc.optIn}
		ing, err := buildIngester(src, CategoryConfig{}, store.NewMemoryStore(), o)
		if err != nil {
			t.Fatalf("buildIngester: %v", err)
		}
		ex, ok := ing.Extractor.(interface{ StampEnabled() bool })
		if !ok {
			t.Fatalf("extractor %T does not report its stamp setting", ing.Extractor)
		}
		if got := ex.StampEnabled(); got != tc.want {
			t.Errorf("lwinStamp=%v: stamping %v, want %v", tc.optIn, got, tc.want)
		}
	}
}
