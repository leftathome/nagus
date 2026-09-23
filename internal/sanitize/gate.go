package sanitize

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/glovebox"
	"github.com/leftathome/nagus/internal/listing"
)

// Gate is the real trust boundary: every listing's untrusted text is
// classified by glovebox's out-of-process sanitize gate (POST /v1/sanitize)
// before nagus acts on it (nagus-9ib).
//
// Passthrough was honest only while no listing text reached an LLM. That no
// longer holds: watch rows, titles included, are delivered to an agent, and
// the deals mailbox is an open channel anyone can write to. So the gate
// applies to every source.
//
// FAIL CLOSED. glovebox classifies and never rewrites. Only verdict=pass keeps
// the item, with its ORIGINAL bytes; quarantine, any error status, and any
// transport failure drop it. 429 (rate limited) and 503 (scanner not ready)
// are documented as transient and are retried with backoff first -- dropping
// on them would silently lose items during a burst or a glovebox restart --
// but a retry that never reaches a verdict still drops. Nothing is ever
// passed without one.
type Gate struct {
	Client *glovebox.ClientWithResponses
	// Token is nagus's bearer token (source-id "nagus" in glovebox's registry).
	Token string
	// Name is the boundary stamped on Sanitized; defaults to "glovebox.sanitize".
	Name string
	// Retries bounds the transient (429/503) retries; 0 defaults to 3.
	Retries int
	// Backoff is the first retry delay, doubled each time; 0 defaults to 1s.
	Backoff time.Duration
	Sleep   func(context.Context, time.Duration) error
	Logf    func(string, ...any)
}

var _ listing.Sanitizer = (*Gate)(nil)

// ErrQuarantined is returned when glovebox classifies a listing as an
// injection attempt; the item is dropped.
var ErrQuarantined = errors.New("sanitize: quarantined by glovebox")

// NewGate builds a gate against glovebox's sanitize listener, e.g.
// "http://glovebox.glovebox-ingest.svc.cluster.local:9093".
func NewGate(baseURL, token string, hc *http.Client, logf func(string, ...any)) (*Gate, error) {
	if baseURL == "" || token == "" {
		return nil, errors.New("sanitize: the glovebox gate needs both a URL and a token")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	c, err := glovebox.NewClientWithResponses(strings.TrimRight(baseURL, "/"), glovebox.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("sanitize: glovebox client: %w", err)
	}
	return &Gate{Client: c, Token: token, Logf: logf}, nil
}

// Sanitize classifies the listing's untrusted text. See the type doc for the
// fail-closed mapping.
func (g *Gate) Sanitize(ctx context.Context, r listing.Raw) (listing.Sanitized, error) {
	ctype := "text/plain"
	body := glovebox.SanitizeJSONRequestBody{Content: untrustedText(r), ContentType: &ctype}
	auth := func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+g.Token)
		return nil
	}
	retries, backoff := g.Retries, g.Backoff
	if retries <= 0 {
		retries = 3
	}
	if backoff <= 0 {
		backoff = time.Second
	}
	sleep := g.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for attempt := 0; ; attempt++ {
		res, err := g.Client.SanitizeWithResponse(ctx, body, auth)
		if err != nil {
			return listing.Sanitized{}, fmt.Errorf("sanitize: glovebox unreachable, dropping (fail closed): %w", err)
		}
		switch code := res.StatusCode(); {
		case code == http.StatusOK && res.JSON200 != nil:
			v := res.JSON200
			if v.Verdict != glovebox.Pass {
				return listing.Sanitized{}, fmt.Errorf("%w: score %.2f, signals %s", ErrQuarantined, v.TotalScore, signalNames(v.Signals))
			}
			// Keep the ORIGINAL bytes: glovebox classifies, it never rewrites.
			s, _ := Passthrough{Name: g.boundary()}.Sanitize(ctx, r)
			return s, nil
		case (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) && attempt < retries:
			if err := sleep(ctx, backoff<<attempt); err != nil {
				return listing.Sanitized{}, fmt.Errorf("sanitize: %w (fail closed)", err)
			}
			continue
		default:
			return listing.Sanitized{}, fmt.Errorf("sanitize: glovebox HTTP %d, dropping (fail closed)", code)
		}
	}
}

func (g *Gate) boundary() string {
	if g.Name == "" {
		return "glovebox.sanitize"
	}
	return g.Name
}

// untrustedText is every free-text field a listing carries: the title, the
// body, and the aspect values (sorted, so a request is deterministic). The
// aspects include seller-authored strings (vendor, product type), so they are
// classified too.
func untrustedText(r listing.Raw) string {
	var b strings.Builder
	b.WriteString(r.Title)
	b.WriteString("\n\n")
	b.WriteString(r.Body)
	keys := make([]string, 0, len(r.Aspects))
	for k := range r.Aspects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("\n")
		b.WriteString(r.Aspects[k])
	}
	return b.String()
}

func signalNames(ss []glovebox.Signal) string {
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		names = append(names, s.Name)
	}
	return "[" + strings.Join(names, ", ") + "]"
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
