package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/leftathome/nagus/internal/identity/lwin"
	"github.com/leftathome/nagus/internal/refdata"
)

// lwinSource provides the ONE LWIN dictionary a nagus process uses. Every wine
// ingester and the wine surface share it: the loaded dictionary is ~55 MiB, and
// building it per consumer (wineDepsFrom runs once per wine source and again for
// the surface) would triple that inside a 256Mi container.
//
// Two ways to configure it:
//   - NAGUS_LWIN_URL (+ NAGUS_LWIN_CACHE): mirror the published file onto the
//     volume, re-checked against the source after NAGUS_LWIN_MAX_AGE (default
//     30 days) and swapped in while serving (refreshLoop).
//   - NAGUS_LWIN_CSV: a local file (.csv or .xlsx), loaded once, never
//     refreshed. Kept for tests and air-gapped runs.
type lwinSource struct {
	localPath string
	mirror    *refdata.Mirror
	stamp     bool
	logf      func(string, ...any)
	// minRecords is the size a download must parse to before it may replace
	// the mirror (minLWINRecords in production; tests lower it).
	minRecords int

	once     sync.Once
	dict     lwin.Dictionary
	resolver *lwin.Resolver
	err      error

	// refreshMu serializes refreshes (the startup load and the daily loop).
	refreshMu sync.Mutex
	// validated is the dictionary Validate just parsed from a download, handed
	// to refresh so an accepted download is parsed once, not twice.
	validated *lwin.DB
	// loaded is closed once the first mirror load attempt finishes (tests).
	loaded chan struct{}

	loadedAt        atomic.Int64 // unix seconds of the dictionary in use
	refreshFailures atomic.Int64
}

// lwinSourceFromEnv returns nil when LWIN is not configured.
func lwinSourceFromEnv(client *http.Client, logf func(string, ...any)) *lwinSource {
	local := envOr("NAGUS_LWIN_CSV", "")
	url := envOr("NAGUS_LWIN_URL", "")
	if local == "" && url == "" {
		return nil
	}
	s := &lwinSource{localPath: local, stamp: envBool("NAGUS_LWIN_STAMP"), logf: logf, minRecords: minLWINRecords}
	if local == "" {
		s.mirror = s.newMirror(url, envOr("NAGUS_LWIN_CACHE", "/data/lwin/LWINdatabase.xlsx"),
			envDuration("NAGUS_LWIN_MAX_AGE", 30*24*time.Hour), client)
	}
	return s
}

// newMirror describes the on-volume copy of the published export.
func (s *lwinSource) newMirror(url, path string, maxAge time.Duration, client *http.Client) *refdata.Mirror {
	return &refdata.Mirror{
		URL:      url,
		Path:     path,
		MaxAge:   maxAge,
		MaxBytes: 512 << 20,
		HTTP:     client,
		// A download replaces the mirror only if it parses into a
		// non-trivial dictionary: a truncated file, an HTML error page
		// served with 200, or a format change must never evict a good copy.
		Validate: func(p string) error {
			db, err := loadLWINFile(p)
			if err != nil {
				return err
			}
			if db.Len() < s.minRecords {
				return fmt.Errorf("only %d usable records (want >= %d)", db.Len(), s.minRecords)
			}
			s.validated = db
			return nil
		},
	}
}

// minLWINRecords guards a download against being truncated or wrong: the real
// export yields ~185,000 usable wine records.
const minLWINRecords = 10000

// get returns the shared resolver. The first call starts the dictionary load.
//
// A LOCAL file loads synchronously, and failing to load is an error: the
// operator named a file, and running without it silently would be the
// identity-less run they did not ask for.
//
// A MIRROR loads in the BACKGROUND and never fails startup. Downloading and
// parsing the export takes seconds to tens of seconds (26 MB, 185k records, on
// an arm64 node), which must not delay nagus's HTTP listener past its liveness
// probe, and a remote outage must not take hdd surfacing down with it. Until
// the load lands the resolver matches nothing (every listing routes "review");
// listings are re-extracted on their next ingest, so nothing is lost.
func (s *lwinSource) get(ctx context.Context) (*lwin.Resolver, error) {
	s.once.Do(func() {
		s.resolver = &lwin.Resolver{Dict: &s.dict}
		s.loaded = make(chan struct{})
		if s.localPath != "" {
			defer close(s.loaded)
			db, err := loadLWINFile(s.localPath)
			if err != nil {
				s.err = fmt.Errorf("wine: loading LWIN export %q: %w", s.localPath, err)
				return
			}
			s.install(db, "local file "+s.localPath)
			return
		}
		go func() {
			defer close(s.loaded)
			if err := s.refresh(context.WithoutCancel(ctx)); err != nil {
				s.logf("lwin: running WITHOUT the LWIN dictionary for now (the daily refresh retries): %v", err)
			}
		}()
	})
	return s.resolver, s.err
}

// waitLoaded blocks until the first dictionary load attempt has finished, max
// has passed, or ctx ends. It reports whether the load attempt finished.
func (s *lwinSource) waitLoaded(ctx context.Context, max time.Duration) bool {
	if s.loaded == nil {
		return true // get never ran: nothing is loading
	}
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-s.loaded:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// lwinStartWait bounds how long a wine source's first ingest waits for the
// dictionary. The real load takes ~2 minutes on an arm64 node at the 500m CPU
// limit; past this, ingest proceeds without it rather than stalling a source.
const lwinStartWait = 10 * time.Minute

// wineIngestGates returns, aligned with sources, a gate for every WINE source
// that holds its first ingest until the LWIN dictionary has loaded (nagus-0k0).
//
// Without it the startup ingest ran at 17:37 and the dictionary landed at
// 17:39 (2026-09-21): every wine item was extracted against an empty
// dictionary, and wine sources ingest every 12h, so each restart cost 12h of
// identity data. Other categories never wait on a wine dictionary.
func wineIngestGates(sources []SourceConfig, s *lwinSource, max time.Duration, logf func(string, ...any)) []func(context.Context) {
	if s == nil {
		return nil
	}
	gates := make([]func(context.Context), len(sources))
	for i, src := range sources {
		if src.Category != "wine" {
			continue
		}
		name := src.Name
		gates[i] = func(ctx context.Context) {
			if !s.waitLoaded(ctx, max) && ctx.Err() == nil {
				logf("lwin: %s starting ingest without the dictionary (not loaded within %s)", name, max)
			}
		}
	}
	return gates
}

// refresh makes the mirror current and, when the file changed or nothing is
// loaded yet, loads it and swaps it in.
func (s *lwinSource) refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.validated = nil
	res, err := s.mirror.Ensure(ctx)
	if err != nil {
		s.refreshFailures.Add(1)
		if res.Path == "" {
			return err
		}
		s.logf("lwin: source check failed, keeping the mirrored copy: %v", err)
	}
	if !res.Downloaded && s.dict.Load() != nil {
		return nil // unchanged and already loaded
	}
	db := s.validated
	s.validated = nil
	if !res.Downloaded || db == nil {
		var lerr error
		if db, lerr = loadLWINFile(res.Path); lerr != nil {
			s.refreshFailures.Add(1)
			return fmt.Errorf("loading mirrored LWIN %s: %w", res.Path, lerr)
		}
	}
	how := "mirror " + res.Path
	if res.Downloaded {
		how = "fresh download from " + s.mirror.URL
	}
	s.install(db, how)
	return nil
}

func (s *lwinSource) install(db *lwin.DB, how string) {
	s.dict.Store(db)
	s.loadedAt.Store(time.Now().Unix())
	mode := "SHADOW (routes recorded, canonical ids NOT stamped; set NAGUS_LWIN_STAMP=true after measuring false matches)"
	if s.stamp {
		mode = "STAMPING canonical ids on auto-route matches"
	}
	s.logf("lwin: %d wine records loaded from %s; %s", db.Len(), how, mode)
}

// refreshLoop re-checks the mirror daily until ctx ends. Only a MIRROR
// refreshes; the check is local (file age) until MaxAge has passed, so this
// costs nothing most days.
func (s *lwinSource) refreshLoop(ctx context.Context) {
	if s.mirror == nil {
		return
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.refresh(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logf("lwin: refresh failed, keeping the dictionary in use: %v", err)
			}
		}
	}
}

// loadLWINFile loads a .xlsx (the published format) or anything else as CSV.
func loadLWINFile(path string) (*lwin.DB, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if strings.HasSuffix(strings.ToLower(path), ".xlsx") || isZip(f) {
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		return lwin.LoadXLSX(f, st.Size())
	}
	return lwin.LoadCSV(f)
}

// isZip sniffs the zip magic, so a mirror path without an .xlsx suffix still
// loads correctly. It rewinds the file.
func isZip(f *os.File) bool {
	var magic [4]byte
	n, _ := io.ReadFull(f, magic[:])
	_, _ = f.Seek(0, io.SeekStart)
	return n == 4 && string(magic[:]) == "PK\x03\x04"
}

// writeLWINMetrics renders the dictionary gauges in Prometheus text.
func writeLWINMetrics(w io.Writer, s *lwinSource) {
	records := 0
	if db := s.dict.Load(); db != nil {
		records = db.Len()
	}
	stamping := 0
	if s.stamp {
		stamping = 1
	}
	fmt.Fprintf(w, "# HELP nagus_lwin_records Wine records in the LWIN dictionary in use (0 = none loaded).\n")
	fmt.Fprintf(w, "# TYPE nagus_lwin_records gauge\n")
	fmt.Fprintf(w, "nagus_lwin_records %d\n", records)
	fmt.Fprintf(w, "# HELP nagus_lwin_loaded_timestamp_seconds When the dictionary in use was loaded (0 = never).\n")
	fmt.Fprintf(w, "# TYPE nagus_lwin_loaded_timestamp_seconds gauge\n")
	fmt.Fprintf(w, "nagus_lwin_loaded_timestamp_seconds %d\n", s.loadedAt.Load())
	fmt.Fprintf(w, "# HELP nagus_lwin_refresh_failures_total Failed checks or loads of the LWIN mirror.\n")
	fmt.Fprintf(w, "# TYPE nagus_lwin_refresh_failures_total counter\n")
	fmt.Fprintf(w, "nagus_lwin_refresh_failures_total %d\n", s.refreshFailures.Load())
	fmt.Fprintf(w, "# HELP nagus_lwin_stamping 1 when auto-route matches stamp canonical ids, 0 in shadow mode.\n")
	fmt.Fprintf(w, "# TYPE nagus_lwin_stamping gauge\n")
	fmt.Fprintf(w, "nagus_lwin_stamping %d\n", stamping)
}
