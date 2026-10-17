package promo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // DecodeConfig for GIF
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Fetch limits.
const (
	DefaultMaxImageBytes = 2 << 20
	MinImageBytes        = 2 << 10
	DefaultFetchTimeout  = 20 * time.Second
	DefaultFetchRetries  = 2
	maxImageRedirects    = 3
)

// imageTypes are the content types an image may have.
var imageTypes = []string{"image/jpeg", "image/png", "image/gif", "image/webp"}

// normImageType lower-cases a declared Content-Type, drops its parameters,
// and maps the non-standard JPEG spellings some CDNs send (image.richrelevance.com
// answers "image/jpg") onto image/jpeg. The sniffed type is still required
// to be an image.
func normImageType(v string) string {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
	switch ct {
	case "image/jpg", "image/pjpeg", "image/jpe":
		return "image/jpeg"
	}
	return ct
}

// ErrPrivateAddress is returned by the public dialer for an address that is
// not a public unicast one.
var ErrPrivateAddress = errors.New("promo: refusing to connect to a non-public address")

// publicOnly is a net.Dialer Control that refuses loopback, private,
// link-local, multicast and unspecified addresses AFTER resolution, so an
// allowlisted name that resolves inward cannot reach the cluster.
func publicOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return ErrPrivateAddress
	}
	a := ap.Addr().Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return ErrPrivateAddress
	}
	for _, p := range specialPurpose {
		if p.Contains(a) {
			return ErrPrivateAddress
		}
	}
	return nil
}

// specialPurpose are address blocks that are never a public web server
// (IANA special-purpose registries), refused explicitly beyond what netip's
// predicates cover. The NAT64 and 6to4 blocks embed IPv4 addresses that
// could be private ones.
var specialPurpose = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
		"fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// NewPublicClient is the production HTTP client for images and links: no
// cookie jar, redirects NOT followed (callers decide each hop), public
// addresses only, TLS verified.
func NewPublicClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	d := &net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: noRedirects}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Fetcher fetches product images.
type Fetcher struct {
	Client   *http.Client // redirects must NOT be followed by the client
	MaxBytes int64
	Retries  int
	// Backoff is the wait before retry n (1-based); nil = 500ms * n.
	Backoff func(n int) time.Duration
	// UserAgent sent with every request.
	UserAgent string
}

// FetchError is a refused or failed fetch. Temporary means a later poll may
// succeed (a transport error or a 5xx).
type FetchError struct {
	Reason    string
	Temporary bool
}

func (e *FetchError) Error() string { return "promo: image fetch: " + e.Reason }

// Drop reasons at fetch.
const (
	DropFetchFailed  = "fetch_failed"
	DropTooLarge     = "image_too_large"
	DropNotImage     = "not_an_image"
	DropSmallImage   = "image_too_small"
	DropImgRedirect  = "image_redirect_refused"
	DropImgHTTPError = "image_http_error"
)

// Image is a fetched image.
type Image struct {
	Bytes       []byte
	ContentType string
}

// FetchImage fetches one image from an allowed host, over https.
func (f *Fetcher) FetchImage(ctx context.Context, src string, hosts []string) (Image, error) {
	u, ok := upgradeURL(src)
	if !ok || !slices.Contains(hosts, u.Host) {
		return Image{}, &FetchError{Reason: SkipImageHost}
	}
	retries := f.Retries
	if retries < 0 {
		retries = 0
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * 500 * time.Millisecond
			if f.Backoff != nil {
				wait = f.Backoff(attempt)
			}
			if wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					t.Stop()
					return Image{}, &FetchError{Reason: DropFetchFailed, Temporary: true}
				case <-t.C:
				}
			}
		}
		img, err := f.fetchOnce(ctx, u, hosts)
		if err == nil {
			return img, nil
		}
		last = err
		var fe *FetchError
		if errors.As(err, &fe) && !fe.Temporary {
			return Image{}, err
		}
	}
	return Image{}, last
}

func (f *Fetcher) fetchOnce(ctx context.Context, u *url.URL, hosts []string) (Image, error) {
	cur := u
	for hop := 0; ; hop++ {
		resp, err := f.get(ctx, cur.String())
		if err != nil {
			return Image{}, &FetchError{Reason: DropFetchFailed, Temporary: true}
		}
		if isRedirect(resp.StatusCode) {
			loc := resp.Header.Get("Location")
			drain(resp)
			next, err := cur.Parse(loc)
			if err != nil || hop >= maxImageRedirects || next.Scheme != "https" || !slices.Contains(hosts, strings.ToLower(next.Hostname())) || next.User != nil ||
				(next.Port() != "" && next.Port() != "443") {
				return Image{}, &FetchError{Reason: DropImgRedirect}
			}
			cur = next
			continue
		}
		defer drain(resp)
		switch {
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			return Image{}, &FetchError{Reason: DropImgHTTPError, Temporary: true}
		case resp.StatusCode != http.StatusOK:
			return Image{}, &FetchError{Reason: DropImgHTTPError}
		}
		ct := normImageType(resp.Header.Get("Content-Type"))
		if !slices.Contains(imageTypes, ct) {
			return Image{}, &FetchError{Reason: DropNotImage}
		}
		max := f.MaxBytes
		if max <= 0 {
			max = DefaultMaxImageBytes
		}
		if resp.ContentLength > max {
			return Image{}, &FetchError{Reason: DropTooLarge}
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return Image{}, &FetchError{Reason: DropFetchFailed, Temporary: true}
		}
		if int64(len(b)) > max {
			return Image{}, &FetchError{Reason: DropTooLarge}
		}
		if sniffed := http.DetectContentType(b); !slices.Contains(imageTypes, sniffed) {
			return Image{}, &FetchError{Reason: DropNotImage}
		}
		if len(b) < MinImageBytes {
			return Image{}, &FetchError{Reason: DropSmallImage}
		}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil && (cfg.Width < 80 || cfg.Height < 80) {
			return Image{}, &FetchError{Reason: DropSmallImage}
		}
		return Image{Bytes: b, ContentType: http.DetectContentType(b)}, nil
	}
}

func (f *Fetcher) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	req.Header.Set("Accept", "image/*")
	return f.Client.Do(req)
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// drain discards (a bounded amount of) a body and closes it.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// String is for logs: never the URL.
func (c Candidate) String() string { return fmt.Sprintf("candidate(alt=%q)", c.Alt) }
