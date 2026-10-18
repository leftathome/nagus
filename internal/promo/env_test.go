package promo

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// world is ONE TLS test server that plays every internet host the tests
// need (image CDNs, tracked-link redirectors, product sites), dispatching on
// the Host header. client() returns a client that reaches it for ANY host
// name, so fixtures keep their real host names.
type world struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // host -> handler
	hits   map[string]*atomic.Int64    // host -> requests
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, routes: map[string]http.HandlerFunc{}, hits: map[string]*atomic.Int64{}}
	w.srv = httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		w.mu.Lock()
		h, ok := w.routes[host]
		c := w.hits[host]
		if c == nil {
			c = &atomic.Int64{}
			w.hits[host] = c
		}
		w.mu.Unlock()
		c.Add(1)
		if !ok {
			http.NotFound(rw, r)
			return
		}
		h(rw, r)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *world) handle(host string, h http.HandlerFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.routes[host] = h
}

func (w *world) count(host string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if c := w.hits[host]; c != nil {
		return c.Load()
	}
	return 0
}

// client reaches the world for every host, verifying the test certificate
// (issued for example.com), no redirects followed, no cookie jar.
func (w *world) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(w.srv.Certificate())
	addr := w.srv.Listener.Addr().String()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12},
	}
	return &http.Client{Transport: tr, CheckRedirect: noRedirects}
}

// testImage is a deterministic noisy PNG whose top-left pixel's red value is
// id, so the fake vision server knows which image it was sent.
func testImage(id uint8, size int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	r := rand.New(rand.NewSource(int64(id) + 1))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255})
		}
	}
	img.Set(0, 0, color.RGBA{id, 0, 0, 255})
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

func imageID(dataURL string) (uint8, bool) {
	_, b64, ok := strings.Cut(dataURL, ";base64,")
	if !ok {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return 0, false
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return 0, false
	}
	r, _, _, _ := img.At(0, 0).RGBA()
	return uint8(r >> 8), true
}

// answers is what the fake vision model says for one image: A for PromptA,
// B for PromptB (B empty = same as A).
type answers struct{ A, B string }

// fakeVision is an OpenAI-compatible chat-completions server.
type fakeVision struct {
	srv   *httptest.Server
	mu    sync.Mutex
	by    map[uint8]answers
	calls atomic.Int64
	keys  []string // Authorization headers seen
	fail  atomic.Int64
	// status, when non-zero, is the HTTP status every call answers.
	status atomic.Int64
	// statusB, when non-zero, is the HTTP status of every PromptB call.
	statusB atomic.Int64
	// failID (image id + 1) and failIDLeft: the next failIDLeft calls for
	// that one image answer 503 (see failOn).
	failID     atomic.Int64
	failIDLeft atomic.Int64
}

func newFakeVision(t *testing.T) *fakeVision {
	t.Helper()
	f := &fakeVision{by: map[uint8]answers{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.mu.Lock()
		f.keys = append(f.keys, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(rw, r)
			return
		}
		if code := f.status.Load(); code != 0 {
			http.Error(rw, "refused", int(code))
			return
		}
		if f.fail.Load() > 0 {
			f.fail.Add(-1)
			http.Error(rw, "busy", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model       string  `json:"model"`
			Temperature float64 `json:"temperature"`
			Messages    []struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		prompt, url := req.Messages[0].Content[0].Text, req.Messages[0].Content[1].ImageURL.URL
		if code := f.statusB.Load(); code != 0 && prompt == PromptB {
			http.Error(rw, "refused", int(code))
			return
		}
		id, ok := imageID(url)
		if ok && f.failID.Load() == int64(id)+1 && f.failIDLeft.Load() > 0 {
			f.failIDLeft.Add(-1)
			http.Error(rw, "busy", http.StatusServiceUnavailable)
			return
		}
		content := `{"products":[]}`
		if ok {
			f.mu.Lock()
			a, known := f.by[id]
			f.mu.Unlock()
			if known {
				content = a.A
				if prompt == PromptB && a.B != "" {
					content = a.B
				}
			}
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// failOn makes the next n calls for image id answer 503: a transient
// failure aimed at one image, whatever order the images are read in.
func (f *fakeVision) failOn(id uint8, n int64) {
	f.failID.Store(int64(id) + 1)
	f.failIDLeft.Store(n)
}

func (f *fakeVision) set(id uint8, a answers) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[id] = a
}

func (f *fakeVision) vision() *Vision {
	return &Vision{BaseURL: f.srv.URL + "/v1", APIKey: "test-key-not-real", Model: "test-vision", Client: f.srv.Client()}
}

// product renders one-product answer JSON.
func product(title, price, was string) string {
	p := map[string]any{"title": title, "price": price, "was_price": nil, "brand": nil, "currency": "USD"}
	if was != "" {
		p["was_price"] = was
	}
	b, _ := json.Marshal(map[string]any{"products": []any{p}})
	return string(b)
}
