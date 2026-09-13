// Package quark is nagus's client for quark, the service that owns product
// identity.
//
// The contract is quark's OpenAPI document, NOT a shared Go package (quark
// design section 5, "Coupling (D4)"): quark is a private module on a different
// host, and importing it would drag GOPRIVATE and credential handling into this
// public repository for no benefit. So this is a deliberately thin, hand-written
// client for the one call nagus makes, POST /resolve.
//
// Two properties of the contract shape this client:
//
//   - quark's request schemas are CLOSED: an unknown member is a 422. This client
//     therefore never sends a field the deployed quark might not accept -- in
//     particular `retry` is omitted unless true, and nagus only retries after
//     quark has reported a catalog generation, which only a quark new enough to
//     accept `retry` can do.
//   - Refusal and quarantine are RESULTS, not errors. An error from Resolve
//     means the call failed; the caller leaves the offers unattempted and tries
//     again later. Confidence is NOT trust: nagus stores quark's product id but
//     must gate any trust decision on Standing.
package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxBatch is quark's schema cap on one resolve call. Sending more is a 422.
const MaxBatch = 500

// maxResponseBytes bounds how much of a response is read. A full 500-hint
// batch is well under 1 MiB; anything larger is not a valid resolve response.
const maxResponseBytes = 4 << 20

// ErrUnauthorized reports a 401: the bearer token is missing, wrong, or not
// issued to a known consumer. Distinct because it is a configuration problem an
// operator fixes, not a transient failure to retry blindly.
var ErrUnauthorized = errors.New("quark: unauthorized (check the nagus bearer token)")

// Routes quark returns.
const (
	RouteExact       = "exact"
	RouteMinted      = "minted"
	RouteRefused     = "refused"
	RouteQuarantined = "quarantined"
)

// Hint is quark's Hint wire shape. Values are untrusted listing text; quark's
// gates, not nagus, decide what identifies a product.
type Hint struct {
	Category string `json:"category"`
	Brand    string `json:"brand,omitempty"`
	MPN      string `json:"mpn,omitempty"`
	GTIN     string `json:"gtin,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Result is one resolution, positionally aligned with the request.
type Result struct {
	ProductID  string `json:"product_id,omitempty"`
	Route      string `json:"route"`
	Confidence int    `json:"confidence"`
	Standing   string `json:"standing,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Response is a resolve response. CatalogGeneration is 0 when quark does not
// report one (quark before the catalog-generation contract addition).
type Response struct {
	Results           []Result `json:"results"`
	CatalogGeneration int64    `json:"catalog_generation"`
}

// Client calls quark.
type Client struct {
	// BaseURL is quark's API root, e.g. http://quark.quark.svc.cluster.local:8080.
	BaseURL string
	// Token is this consumer's bearer token.
	Token string
	// HTTP is the transport; nil uses a client with a 30s timeout.
	HTTP *http.Client
}

// Resolve sends one batch. retry marks the batch as re-offered refusals, so
// quark can label them in its metrics and keep its refused-ratio alert honest.
func (c *Client) Resolve(ctx context.Context, hints []Hint, retry bool) (Response, error) {
	if len(hints) == 0 {
		return Response{}, nil
	}
	if len(hints) > MaxBatch {
		return Response{}, fmt.Errorf("quark: batch of %d exceeds the %d cap", len(hints), MaxBatch)
	}
	body := struct {
		Hints []Hint `json:"hints"`
		Retry bool   `json:"retry,omitempty"`
	}{Hints: hints, Retry: retry}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("quark: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/resolve", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("quark: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("quark: resolve: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Response{}, fmt.Errorf("quark: read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return Response{}, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		// The problem detail is quark's own prose and safe to log; it never
		// echoes the token. Truncated so a misbehaving proxy cannot flood logs.
		return Response{}, fmt.Errorf("quark: resolve returned %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}

	var out Response
	if err := json.Unmarshal(payload, &out); err != nil {
		return Response{}, fmt.Errorf("quark: decode response: %w", err)
	}
	// Positional alignment is the contract. A short or long result list cannot
	// be mapped back to offers safely, so it is an error, not a partial success.
	if len(out.Results) != len(hints) {
		return Response{}, fmt.Errorf("quark: %d results for %d hints violates positional alignment", len(out.Results), len(hints))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
