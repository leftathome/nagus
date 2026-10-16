package promo

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func fetchErr(t *testing.T, err error) *FetchError {
	t.Helper()
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a FetchError", err)
	}
	return fe
}

func TestFetchImage(t *testing.T) {
	w := newWorld(t)
	good := testImage(1, 100)
	w.handle("img.example.com", func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good.png":
			rw.Header().Set("Content-Type", "image/png")
			_, _ = rw.Write(good)
		case "/big.png":
			rw.Header().Set("Content-Type", "image/png")
			_, _ = rw.Write(make([]byte, 3<<20))
		case "/html.png":
			rw.Header().Set("Content-Type", "image/png")
			_, _ = rw.Write([]byte("<html><body>" + strings.Repeat("x", 4096) + "</body></html>"))
		case "/text":
			rw.Header().Set("Content-Type", "text/html")
			_, _ = rw.Write(good)
		case "/tiny.png":
			rw.Header().Set("Content-Type", "image/png")
			_, _ = rw.Write(testImage(2, 40))
		case "/redirect-ok":
			http.Redirect(rw, r, "https://img.example.com/good.png", http.StatusFound)
		case "/redirect-off":
			http.Redirect(rw, r, "https://evil.example.com/good.png", http.StatusFound)
		case "/redirect-http":
			http.Redirect(rw, r, "http://img.example.com/good.png", http.StatusFound)
		case "/flaky":
			http.Error(rw, "busy", http.StatusServiceUnavailable)
		default:
			http.NotFound(rw, r)
		}
	})
	f := &Fetcher{Client: w.client(), Retries: 2, Backoff: func(int) time.Duration { return 0 }}
	hosts := []string{"img.example.com"}
	ctx := context.Background()

	img, err := f.FetchImage(ctx, "http://img.example.com/good.png?t=1", hosts)
	if err != nil || img.ContentType != "image/png" || len(img.Bytes) != len(good) {
		t.Fatalf("good: %v %q %d", err, img.ContentType, len(img.Bytes))
	}
	if _, err := f.FetchImage(ctx, "https://img.example.com/redirect-ok", hosts); err != nil {
		t.Fatalf("redirect on host: %v", err)
	}
	for path, reason := range map[string]string{
		"/big.png":       DropTooLarge,
		"/html.png":      DropNotImage,
		"/text":          DropNotImage,
		"/tiny.png":      DropSmallImage,
		"/redirect-off":  DropImgRedirect,
		"/redirect-http": DropImgRedirect,
		"/missing":       DropImgHTTPError,
	} {
		_, err := f.FetchImage(ctx, "https://img.example.com"+path, hosts)
		if fe := fetchErr(t, err); fe.Reason != reason || fe.Temporary {
			t.Errorf("%s: %+v, want %s", path, fe, reason)
		}
	}
	if _, err := f.FetchImage(ctx, "https://other.example.com/good.png", hosts); fetchErr(t, err).Reason != SkipImageHost {
		t.Errorf("off-host fetch: %v", err)
	}
	if w.count("other.example.com") != 0 {
		t.Fatal("an off-allowlist host was contacted")
	}
	before := w.count("img.example.com")
	_, err = f.FetchImage(ctx, "https://img.example.com/flaky", hosts)
	if fe := fetchErr(t, err); fe.Reason != DropImgHTTPError || !fe.Temporary {
		t.Errorf("flaky: %+v", fe)
	}
	if n := w.count("img.example.com") - before; n != 3 {
		t.Errorf("flaky: %d requests, want 1 + 2 retries", n)
	}
}

func TestPublicOnlyDialer(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:443", "10.1.2.3:443", "192.168.1.1:443", "169.254.169.254:80", "[::1]:443", "100.64.0.1:443", "0.0.0.0:443", "[fd00::1]:443", "224.0.0.1:443"} {
		if err := publicOnly("tcp", addr, nil); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("%s allowed", addr)
		}
	}
	for _, addr := range []string{"93.184.216.34:443", "[2606:2800:220:1::1]:443"} {
		if err := publicOnly("tcp", addr, nil); err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
	}
	// The production client really refuses: a loopback listener is never reached.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cl := NewPublicClient(2 * time.Second)
	if _, err := cl.Get("http://" + ln.Addr().String() + "/"); err == nil || !strings.Contains(err.Error(), ErrPrivateAddress.Error()) {
		t.Fatalf("loopback get: %v", err)
	}
}
