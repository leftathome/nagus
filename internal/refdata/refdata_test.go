package refdata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// source is a fake publisher: serves body with an ETag and honours
// If-None-Match, counting full downloads.
type source struct {
	body      atomic.Value // string
	etag      atomic.Value // string
	status    atomic.Int32 // 0 = normal
	downloads atomic.Int32
	requests  atomic.Int32
}

func newSource(body, etag string) (*source, *httptest.Server) {
	s := &source{}
	s.body.Store(body)
	s.etag.Store(etag)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if st := s.status.Load(); st != 0 {
			w.WriteHeader(int(st))
			return
		}
		etag := s.etag.Load().(string)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s.downloads.Add(1)
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(s.body.Load().(string)))
	}))
	return s, srv
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEnsureDownloadsWhenAbsentThenUsesTheCopy(t *testing.T) {
	src, srv := newSource("v1", `"e1"`)
	defer srv.Close()
	now := time.Now()
	m := &Mirror{URL: srv.URL, Path: filepath.Join(t.TempDir(), "d", "dict.xlsx"), MaxAge: time.Hour,
		Now: func() time.Time { return now }}

	res, err := m.Ensure(context.Background())
	if err != nil || !res.Downloaded || read(t, res.Path) != "v1" {
		t.Fatalf("first Ensure: %+v %v", res, err)
	}
	res, err = m.Ensure(context.Background())
	if err != nil || res.Downloaded || src.requests.Load() != 1 {
		t.Fatalf("a fresh copy must not touch the source: %+v %v requests=%d", res, err, src.requests.Load())
	}
}

// An old copy asks the source conditionally: unchanged costs no download and
// marks the copy current; changed replaces it.
func TestEnsureRevalidatesAnOldCopy(t *testing.T) {
	src, srv := newSource("v1", `"e1"`)
	defer srv.Close()
	now := time.Now()
	m := &Mirror{URL: srv.URL, Path: filepath.Join(t.TempDir(), "dict"), MaxAge: time.Hour,
		Now: func() time.Time { return now }}
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour) // copy is old; source unchanged
	res, err := m.Ensure(context.Background())
	if err != nil || res.Downloaded || src.downloads.Load() != 1 {
		t.Fatalf("unchanged source must answer 304 with no download: %+v %v downloads=%d", res, err, src.downloads.Load())
	}
	requests := src.requests.Load()
	if _, err := m.Ensure(context.Background()); err != nil || src.requests.Load() != requests {
		t.Fatal("a 304 must mark the copy current, so the next Ensure stays local")
	}

	now = now.Add(2 * time.Hour)
	src.body.Store("v2")
	src.etag.Store(`"e2"`)
	res, err = m.Ensure(context.Background())
	if err != nil || !res.Downloaded || read(t, m.Path) != "v2" {
		t.Fatalf("changed source must replace the copy: %+v %v", res, err)
	}
}

// A download Validate rejects never replaces the mirror.
func TestEnsureKeepsTheOldCopyWhenADownloadIsRejected(t *testing.T) {
	src, srv := newSource("good", `"e1"`)
	defer srv.Close()
	now := time.Now()
	m := &Mirror{URL: srv.URL, Path: filepath.Join(t.TempDir(), "dict"), MaxAge: time.Hour,
		Now: func() time.Time { return now },
		Validate: func(p string) error {
			if read(t, p) == "corrupt" {
				return errors.New("not a dictionary")
			}
			return nil
		}}
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	src.body.Store("corrupt")
	src.etag.Store(`"e2"`)
	res, err := m.Ensure(context.Background())
	if err == nil || !res.Stale || read(t, res.Path) != "good" {
		t.Fatalf("rejected download: res=%+v err=%v, want the stale good copy and an error", res, err)
	}
	if left, _ := filepath.Glob(m.Path + ".download-*"); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestEnsureFallsBackToAStaleCopyWhenTheSourceIsDown(t *testing.T) {
	src, srv := newSource("v1", `"e1"`)
	defer srv.Close()
	now := time.Now()
	m := &Mirror{URL: srv.URL, Path: filepath.Join(t.TempDir(), "dict"), MaxAge: time.Hour,
		Now: func() time.Time { return now }}
	if _, err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	src.status.Store(http.StatusServiceUnavailable)
	res, err := m.Ensure(context.Background())
	if err == nil || !res.Stale || read(t, res.Path) != "v1" {
		t.Fatalf("source down: res=%+v err=%v, want stale v1 plus an error", res, err)
	}
}

func TestEnsureWithNoCopyAndNoSourceIsAnError(t *testing.T) {
	src, srv := newSource("v1", `"e1"`)
	defer srv.Close()
	src.status.Store(http.StatusNotFound)
	m := &Mirror{URL: srv.URL, Path: filepath.Join(t.TempDir(), "dict")}
	res, err := m.Ensure(context.Background())
	if err == nil || res.Path != "" {
		t.Fatalf("res=%+v err=%v, want no path and an error", res, err)
	}
}

func TestEnsureRejectsAnOversizedDownload(t *testing.T) {
	_, srv := newSource("0123456789", `"e1"`)
	defer srv.Close()
	m := &Mirror{URL: srv.URL, Path: filepath.Join(t.TempDir(), "dict"), MaxBytes: 5}
	if _, err := m.Ensure(context.Background()); err == nil {
		t.Fatal("a response over MaxBytes must be rejected")
	}
	if _, err := os.Stat(m.Path); !os.IsNotExist(err) {
		t.Fatal("an oversized download must not become the mirror")
	}
}
