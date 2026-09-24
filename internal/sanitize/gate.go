package sanitize

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
//
// A REJECTED TOKEN TRIPS THE GATE (nagus-lg7). 401/403 is systemic, never
// per-listing: calling glovebox again for every remaining listing only trips
// glovebox's auth brute-force limiter (first live enable, 2026-09-23). So the
// first 401/403 trips the gate for Cooldown: listings are dropped WITHOUT a
// call, one line says why, and after the cooldown a single call probes again
// (re-reading the token first -- a rotated or late-synced token heals itself).
//
// THE TOKEN MAY BE A FILE (nagus-4g3). With TokenFile set the token is read
// from the mounted Secret whenever the gate has none, and again after every
// trip: a Secret that syncs AFTER the pod started (an ExternalSecret created
// in the same rollout) heals without a restart, and rotation needs none.
type Gate struct {
	Client *glovebox.ClientWithResponses
	// Token is nagus's bearer token (source-id "nagus" in glovebox's
	// registry). TokenFile, when set, takes precedence and is re-read.
	Token     string
	TokenFile string
	// Name is the boundary stamped on Sanitized; defaults to "glovebox.sanitize".
	Name string
	// Retries bounds the transient (429/503) retries; 0 defaults to 3.
	Retries int
	// Backoff is the first retry delay, doubled each time; 0 defaults to 1s.
	Backoff time.Duration
	// Cooldown is how long a rejected token trips the gate; 0 defaults to 5m.
	Cooldown time.Duration
	Sleep    func(context.Context, time.Duration) error
	Now      func() time.Time
	Logf     func(string, ...any)

	mu        sync.Mutex
	token     string    // current token (from Token or TokenFile)
	tripUntil time.Time // while now < tripUntil, drop without calling
	stats     gateStats
}

type gateStats struct {
	pass, quarantine, errored, unauthorized, tripped, noToken atomic.Int64
}

// Stats are the gate's outcome counters, exported as
// nagus_sanitize_total{outcome=...}.
type Stats struct {
	Pass, Quarantine, Error, Unauthorized, Tripped, NoToken int64
	// Misconfigured counts drops by a half-configured (Closed) gate.
	Misconfigured int64
}

var _ listing.Sanitizer = (*Gate)(nil)

// ErrQuarantined is returned when glovebox classifies a listing as an
// injection attempt; the item is dropped.
var ErrQuarantined = errors.New("sanitize: quarantined by glovebox")

// NewGate builds a gate against glovebox's sanitize listener, e.g.
// "http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9093". Exactly
// one of token and tokenFile is expected; an empty token FILE is allowed (it
// may not have synced yet) and is re-read until it has a value.
func NewGate(baseURL, token, tokenFile string, hc *http.Client, logf func(string, ...any)) (*Gate, error) {
	if baseURL == "" || (token == "" && tokenFile == "") {
		return nil, errors.New("sanitize: the glovebox gate needs a URL and a token (or token file)")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	c, err := glovebox.NewClientWithResponses(strings.TrimRight(baseURL, "/"), glovebox.WithHTTPClient(hc))
	if err != nil {
		return nil, fmt.Errorf("sanitize: glovebox client: %w", err)
	}
	return &Gate{Client: c, Token: token, TokenFile: tokenFile, Logf: logf}, nil
}

// Snapshot returns the outcome counters.
func (g *Gate) Snapshot() Stats {
	return Stats{
		Pass: g.stats.pass.Load(), Quarantine: g.stats.quarantine.Load(), Error: g.stats.errored.Load(),
		Unauthorized: g.stats.unauthorized.Load(), Tripped: g.stats.tripped.Load(), NoToken: g.stats.noToken.Load(),
	}
}

// Sanitize classifies the listing's untrusted text. See the type doc for the
// fail-closed mapping, the trip, and the token file.
func (g *Gate) Sanitize(ctx context.Context, r listing.Raw) (listing.Sanitized, error) {
	token, until := g.current()
	if !until.IsZero() {
		g.stats.tripped.Add(1)
		return listing.Sanitized{}, fmt.Errorf("sanitize: glovebox rejected nagus's token; gate tripped until %s, dropping without a call (fail closed)", until.Format(time.RFC3339))
	}
	if token == "" {
		g.stats.noToken.Add(1)
		return listing.Sanitized{}, fmt.Errorf("sanitize: no token yet (%s empty -- has its Secret synced?), dropping (fail closed)", g.TokenFile)
	}
	ctype := "text/plain"
	body := glovebox.SanitizeJSONRequestBody{Content: untrustedText(r), ContentType: &ctype}
	auth := func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
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
			g.stats.errored.Add(1)
			return listing.Sanitized{}, fmt.Errorf("sanitize: glovebox unreachable, dropping (fail closed): %w", err)
		}
		switch code := res.StatusCode(); {
		case code == http.StatusOK && res.JSON200 != nil:
			v := res.JSON200
			if v.Verdict != glovebox.Pass {
				g.stats.quarantine.Add(1)
				return listing.Sanitized{}, fmt.Errorf("%w: score %.2f, signals %s", ErrQuarantined, v.TotalScore, signalNames(v.Signals))
			}
			g.stats.pass.Add(1)
			// Keep the ORIGINAL bytes: glovebox classifies, it never rewrites.
			s, _ := Passthrough{Name: g.boundary()}.Sanitize(ctx, r)
			return s, nil
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			g.stats.unauthorized.Add(1)
			g.trip(code)
			return listing.Sanitized{}, fmt.Errorf("sanitize: glovebox HTTP %d (token rejected), dropping (fail closed)", code)
		case (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) && attempt < retries:
			if err := sleep(ctx, backoff<<attempt); err != nil {
				g.stats.errored.Add(1)
				return listing.Sanitized{}, fmt.Errorf("sanitize: %w (fail closed)", err)
			}
			continue
		default:
			g.stats.errored.Add(1)
			return listing.Sanitized{}, fmt.Errorf("sanitize: glovebox HTTP %d, dropping (fail closed)", code)
		}
	}
}

// current returns the token to send and, while the gate is tripped, when the
// trip ends (zero otherwise). The token file is re-read whenever there is no
// token yet and when a trip ends.
func (g *Gate) current() (string, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !g.tripUntil.IsZero() {
		if now.Before(g.tripUntil) {
			return "", g.tripUntil
		}
		g.tripUntil = time.Time{}
		g.token = "" // re-read: the token may have been fixed or rotated
		if g.Logf != nil {
			g.Logf("sanitize: trip cooldown over; probing glovebox again")
		}
	}
	if g.token == "" {
		g.token = g.loadToken()
	}
	return g.token, time.Time{}
}

func (g *Gate) loadToken() string {
	if g.TokenFile == "" {
		return g.Token
	}
	b, err := os.ReadFile(g.TokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// trip stops calls for the cooldown after a rejected token, logging once.
func (g *Gate) trip(code int) {
	cooldown := g.Cooldown
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.tripUntil.IsZero() {
		return
	}
	g.tripUntil = g.now().Add(cooldown)
	if g.Logf != nil {
		g.Logf("sanitize: glovebox REJECTED nagus's token (HTTP %d); dropping every listing WITHOUT calling glovebox for %s, then probing once. Check that secret/glovebox/ingest-tokens/nagus (field token) holds the same 64-hex value nagus sends.", code, cooldown)
	}
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
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
