// Package refdata keeps a local mirror of a public reference file -- a
// dictionary or catalog published at a stable URL -- for a service that must
// not depend on that URL being up whenever it starts.
//
// The pattern (download at startup if absent or old, keep the last good copy
// otherwise) was chosen for LWIN, whose publisher refreshes one object at a
// fixed path, and is meant to be reused: quark will need the same thing for
// catalog data. Nothing here knows what the file contains; the caller's
// Validate decides whether a download is good enough to replace the mirror.
//
// Guarantees:
//   - The mirror file is only ever replaced by a download that Validate
//     accepted, via rename, so a reader never sees a partial or rejected file.
//   - A failed refresh leaves the previous mirror in place and says so
//     (Result.Stale) rather than failing: yesterday's dictionary beats none.
//   - Downloads are conditional (If-None-Match on the stored ETag), so an
//     unchanged file costs one small request, not the whole object.
//   - Downloads are size-capped (MaxBytes).
package refdata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Mirror describes one mirrored file.
type Mirror struct {
	// URL is the public source of truth.
	URL string
	// Path is where the mirror lives. The directory is created if missing;
	// Path+".etag" records the source ETag and the file's modification time
	// records when the copy was last confirmed current.
	Path string
	// MaxAge is how long a confirmed copy is used before asking the source
	// again. <= 0 asks every time.
	MaxAge time.Duration
	// MaxBytes caps a download; a larger response is rejected. <= 0 = 1 GiB.
	MaxBytes int64
	// Validate inspects a downloaded candidate (at the given path) and returns
	// an error to reject it. Nil accepts anything.
	Validate func(path string) error
	// HTTP is the client; nil uses one with a 10 minute timeout.
	HTTP *http.Client
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Result says what Ensure did.
type Result struct {
	// Path is the mirror to read, "" when no usable copy exists.
	Path string
	// Downloaded is true when a new copy replaced (or created) the mirror.
	Downloaded bool
	// Stale is true when the source could not be checked and an older copy
	// is being used; the error returned alongside says why.
	Stale bool
}

// Ensure makes sure a usable mirror exists and is no older than MaxAge,
// consulting the source only when it is missing or old.
//
// The error is non-nil whenever the source was needed and could not deliver.
// Result.Path may still be set (Stale) -- callers should use it and report the
// error, not treat every error as fatal.
func (m *Mirror) Ensure(ctx context.Context) (Result, error) {
	now := m.now()
	info, statErr := os.Stat(m.Path)
	have := statErr == nil && info.Mode().IsRegular() && info.Size() > 0
	if have && m.MaxAge > 0 && now.Sub(info.ModTime()) < m.MaxAge {
		return Result{Path: m.Path}, nil
	}
	downloaded, err := m.refresh(ctx, have)
	switch {
	case err == nil:
		return Result{Path: m.Path, Downloaded: downloaded}, nil
	case have:
		return Result{Path: m.Path, Stale: true}, err
	default:
		return Result{}, err
	}
}

func (m *Mirror) refresh(ctx context.Context, have bool) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o750); err != nil {
		return false, fmt.Errorf("refdata: mirror directory: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return false, fmt.Errorf("refdata: request: %w", err)
	}
	if have {
		if etag, err := os.ReadFile(m.Path + ".etag"); err == nil && len(etag) > 0 {
			req.Header.Set("If-None-Match", strings.TrimSpace(string(etag)))
		}
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return false, fmt.Errorf("refdata: fetching %s: %w", m.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified && have:
		// Unchanged at the source: the copy is confirmed current as of now.
		now := m.now()
		if err := os.Chtimes(m.Path, now, now); err != nil {
			return false, fmt.Errorf("refdata: marking mirror current: %w", err)
		}
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("refdata: fetching %s: HTTP %d", m.URL, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(m.Path), filepath.Base(m.Path)+".download-*")
	if err != nil {
		return false, fmt.Errorf("refdata: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	limit := m.maxBytes()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, limit+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return false, fmt.Errorf("refdata: downloading %s: %w", m.URL, err)
	case n > limit:
		return false, fmt.Errorf("refdata: %s exceeds the %d byte cap", m.URL, limit)
	case n == 0:
		return false, fmt.Errorf("refdata: %s returned an empty body", m.URL)
	}
	if m.Validate != nil {
		if err := m.Validate(tmpName); err != nil {
			return false, fmt.Errorf("refdata: rejected download of %s: %w", m.URL, err)
		}
	}
	if err := os.Rename(tmpName, m.Path); err != nil {
		return false, fmt.Errorf("refdata: installing mirror: %w", err)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		_ = os.Remove(m.Path + ".etag")
	} else if err := os.WriteFile(m.Path+".etag", []byte(etag), 0o600); err != nil {
		return true, fmt.Errorf("refdata: recording etag: %w", err)
	}
	return true, nil
}

func (m *Mirror) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Mirror) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (m *Mirror) maxBytes() int64 {
	if m.MaxBytes > 0 {
		return m.MaxBytes
	}
	return 1 << 30
}

// ErrNoMirror is returned by callers that require a mirror and have none.
var ErrNoMirror = errors.New("refdata: no mirror available")
