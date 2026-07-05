// Package ebay implements a nagus-direct listing.Connector over the eBay
// Browse API (item_summary/search), producing normalized-but-raw
// listing.Raw records for HDD/storage-adjacent queries. It is the
// common-denominator durables source: no scraping, no glovebox connector
// hop, just OAuth2 client-credentials + a REST search mapped straight into
// Raw.
//
// # Live-data caveat (researched 2026-07-01)
//
// eBay tightened its Developer Program / API ToS against AI-agent traffic in
// February 2026. Full production access to Browse search data is gated
// behind EPN (eBay Partner Network) partner approval plus an "Application
// Growth Check" -- a free/basic developer keyset may return throttled,
// limited, or empty itemSummaries until that approval lands. That approval
// is a separate, human-gated step outside this repo; this connector cannot
// and does not assert live completeness. It is proven here entirely offline:
// against an httptest.Server standing in for api.ebay.com, and via
// FixturePath, which reads a canned item_summary/search response from disk
// and never touches the network. Once/if an approved keyset exists, point
// Config.BaseURL/OAuthURL at the real eBay hosts (the defaults) and supply
// real credentials; no code change is required.
//
// A future option, if EPN approval proves too slow or too narrow, is a paid
// third-party scraping wrapper (e.g. Apify's eBay actors, Bright Data) sitting
// behind the same listing.Connector interface -- out of scope here.
//
// # Trust boundary
//
// Title, Body, and Aspects values on the emitted Raw are UNTRUSTED free text
// per internal/listing's contract; this connector does no sanitization, only
// field mapping. PriceCents, Currency, and the source condition code are
// structured scalars and pass through unchanged.
package ebay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the stable connector identity stamped onto every Raw this
// package emits.
const SourceID = "ebay"

// Default production endpoints. Both are overridable on Config so tests (and
// any future paid-wrapper swap) can point elsewhere without touching code.
const (
	// DefaultBaseURL is the eBay API host; Browse search is served under
	// /buy/browse/v1/item_summary/search off this base.
	DefaultBaseURL = "https://api.ebay.com"
	// DefaultOAuthURL is eBay's OAuth2 client-credentials token endpoint.
	DefaultOAuthURL = "https://api.ebay.com/identity/v1/oauth2/token"
	// DefaultSandboxBaseURL / DefaultSandboxOAuthURL are the eBay Sandbox hosts.
	// The Sandbox is a separate test environment (License 8.4): exercising it does
	// NOT consume the production daily call budget and is not circumvention.
	DefaultSandboxBaseURL  = "https://api.sandbox.ebay.com"
	DefaultSandboxOAuthURL = "https://api.sandbox.ebay.com/identity/v1/oauth2/token"
	// DefaultMarketplaceID is used when Config.MarketplaceID is empty.
	DefaultMarketplaceID = "EBAY_US"
	// DefaultLimit is used when Config.Limit is <= 0.
	DefaultLimit = 50
	// oauthScope is the Browse API's read scope for client-credentials auth.
	oauthScope = "https://api.ebay.com/oauth/api_scope"
	// tokenSafetyMargin is subtracted from the token's reported lifetime so
	// Fetch refreshes slightly before the real expiry rather than racing it.
	tokenSafetyMargin = 60 * time.Second
)

// Config configures a Connector. ClientID and ClientSecret are credentials:
// the caller reads them from its own secret store (Vault, per repo policy)
// and passes them in at construction -- this package never reads env vars or
// files for them itself, and they must never be hardcoded or committed.
type Config struct {
	// ClientID and ClientSecret are the eBay application's OAuth2
	// client-credentials pair.
	ClientID     string
	ClientSecret string

	// MarketplaceID selects the eBay marketplace to search, e.g. "EBAY_US".
	// Defaults to DefaultMarketplaceID if empty.
	MarketplaceID string

	// Query is the Browse search term(s), e.g. "internal hard drive".
	Query string
	// Filter is an optional raw eBay Browse "filter" query parameter value
	// (e.g. "price:[50..300],conditionIds:{1000|2500}"), passed through
	// unmodified when non-empty.
	Filter string
	// Limit is the max number of results requested per Fetch. Defaults to
	// DefaultLimit when <= 0.
	Limit int

	// HTTPClient performs HTTP requests. Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// BaseURL is the eBay API host for Browse search. Defaults to
	// DefaultBaseURL; override with an httptest.Server URL in tests.
	BaseURL string
	// OAuthURL is the OAuth2 client-credentials token endpoint. Defaults to
	// DefaultOAuthURL; override with an httptest.Server URL in tests.
	OAuthURL string
	// Now returns the current time; used for token-expiry bookkeeping and
	// stamped onto every emitted Raw.SeenAt. Defaults to time.Now.
	Now func() time.Time

	// DailyBudget caps the number of eBay API calls (OAuth + search) this
	// connector makes per UTC day. Defaults to DefaultDailyBudget. When the
	// budget (or an eBay-reported remaining count) is spent, Fetch returns
	// ErrBudgetExhausted and makes no further calls until the next window.
	DailyBudget int

	// FixturePath, when non-empty, makes Fetch read a local JSON file (the
	// same item_summary/search response shape eBay returns) instead of
	// making any network call -- the offline proving path required while
	// live production access is gated (see package doc).
	FixturePath string

	// Sandbox routes calls to eBay's Sandbox environment when BaseURL/OAuthURL
	// are not explicitly set. Use with sandbox Application Keys to validate
	// against the real eBay APIs without spending the production call budget.
	Sandbox bool
}

// Connector implements listing.Connector over the eBay Browse API.
type Connector struct {
	cfg    Config
	budget *callBudget

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// NewConnector builds a Connector from cfg, filling in defaults for any
// unset seam. It does not validate credentials -- an OAuth or search
// failure surfaces from Fetch.
func NewConnector(cfg Config) *Connector {
	if cfg.MarketplaceID == "" {
		cfg.MarketplaceID = DefaultMarketplaceID
	}
	if cfg.Limit <= 0 {
		cfg.Limit = DefaultLimit
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	// An explicit URL always wins; otherwise pick the production or sandbox host.
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
		if cfg.Sandbox {
			cfg.BaseURL = DefaultSandboxBaseURL
		}
	}
	if cfg.OAuthURL == "" {
		cfg.OAuthURL = DefaultOAuthURL
		if cfg.Sandbox {
			cfg.OAuthURL = DefaultSandboxOAuthURL
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg, budget: newCallBudget(cfg.DailyBudget, cfg.Now)}
}

// BudgetStats returns a snapshot of this connector's eBay API-call budget usage,
// for metrics.
func (c *Connector) BudgetStats() BudgetStats {
	return c.budget.stats()
}

// SourceID returns the stable connector identity stamped onto every Raw
// this Connector emits.
func (c *Connector) SourceID() string {
	return SourceID
}

// Fetch returns the current Browse search results as listing.Raw. When
// Config.FixturePath is set, Fetch reads that file instead of calling the
// network at all (neither OAuth nor search); this is the offline proving
// path documented at package level.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	var resp browseSearchResponse
	if c.cfg.FixturePath != "" {
		data, err := os.ReadFile(c.cfg.FixturePath)
		if err != nil {
			return nil, fmt.Errorf("ebay: read fixture %s: %w", c.cfg.FixturePath, err)
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("ebay: decode fixture %s: %w", c.cfg.FixturePath, err)
		}
	} else {
		token, err := c.token(ctx)
		if err != nil {
			return nil, fmt.Errorf("ebay: get token: %w", err)
		}
		resp, err = c.search(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("ebay: search: %w", err)
		}
	}

	now := c.cfg.Now()
	raws := make([]listing.Raw, 0, len(resp.ItemSummaries))
	for _, is := range resp.ItemSummaries {
		r, ok := mapItemSummary(is, now)
		if !ok {
			// No itemId: cannot form provenance (SourceKey), so this item
			// cannot become a valid Raw. Skip rather than emit a broken record.
			continue
		}
		raws = append(raws, r)
	}
	return raws, nil
}

// --- OAuth2 client-credentials -----------------------------------------------

type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// token returns a cached access token if it is still valid, otherwise
// fetches a fresh one via the OAuth2 client-credentials grant and caches it
// until near its reported expiry (using Config.Now, so tests are
// deterministic).
func (c *Connector) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.cfg.Now()
	if c.accessToken != "" && now.Before(c.tokenExpiry) {
		return c.accessToken, nil
	}

	body := url.Values{
		"grant_type": {"client_credentials"},
		"scope":      {oauthScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.OAuthURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	creds := base64.StdEncoding.EncodeToString([]byte(c.cfg.ClientID + ":" + c.cfg.ClientSecret))
	req.Header.Set("Authorization", "Basic "+creds)

	// Account for this call against the daily budget BEFORE making it; refuse
	// (rather than circumvent) once the budget or eBay-reported remaining is spent.
	if err := c.budget.reserve(); err != nil {
		return "", err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	c.budget.observeRateHeaders(resp.Header)

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("token endpoint returned status %d: %s", resp.StatusCode, truncate(data, 200))
	}

	var parsed oauthTokenResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", errors.New("token response had no access_token")
	}

	c.accessToken = parsed.AccessToken
	expiresIn := time.Duration(parsed.ExpiresIn) * time.Second
	c.tokenExpiry = now.Add(expiresIn - tokenSafetyMargin)
	return c.accessToken, nil
}

// --- Browse search ------------------------------------------------------------

type browseSearchResponse struct {
	Total         int           `json:"total"`
	ItemSummaries []itemSummary `json:"itemSummaries"`
}

type itemSummary struct {
	ItemID       string        `json:"itemId"`
	Title        string        `json:"title"`
	ItemWebURL   string        `json:"itemWebUrl"`
	Condition    string        `json:"condition"`
	ConditionID  string        `json:"conditionId"`
	Price        *itemPrice    `json:"price"`
	Seller       *itemSeller   `json:"seller"`
	ItemLocation *itemLocation `json:"itemLocation"`
}

type itemPrice struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// itemSeller carries only eBay's PUBLIC seller-quality signals. It intentionally
// does NOT decode the seller username: nagus never ingests eBay user PII, so we
// don't even parse it off the wire (data minimization).
type itemSeller struct {
	FeedbackPercentage string `json:"feedbackPercentage"`
	FeedbackScore      *int   `json:"feedbackScore"` // pointer: distinguish absent from a real 0 (new seller)
}

type itemLocation struct {
	Country string `json:"country"`
}

// search calls GET {BaseURL}/buy/browse/v1/item_summary/search?q=...&limit=...[&filter=...]
// with the given bearer token, and decodes the response.
func (c *Connector) search(ctx context.Context, token string) (browseSearchResponse, error) {
	q := url.Values{}
	q.Set("q", c.cfg.Query)
	q.Set("limit", strconv.Itoa(c.cfg.Limit))
	if c.cfg.Filter != "" {
		q.Set("filter", c.cfg.Filter)
	}
	reqURL := strings.TrimRight(c.cfg.BaseURL, "/") + "/buy/browse/v1/item_summary/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return browseSearchResponse{}, fmt.Errorf("build search request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-EBAY-C-MARKETPLACE-ID", c.cfg.MarketplaceID)
	req.Header.Set("Accept", "application/json")

	if err := c.budget.reserve(); err != nil {
		return browseSearchResponse{}, err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return browseSearchResponse{}, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()
	c.budget.observeRateHeaders(resp.Header)

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return browseSearchResponse{}, fmt.Errorf("read search response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return browseSearchResponse{}, fmt.Errorf("search endpoint returned status %d: %s", resp.StatusCode, truncate(data, 200))
	}

	var parsed browseSearchResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return browseSearchResponse{}, fmt.Errorf("decode search response: %w", err)
	}
	return parsed, nil
}

// --- mapping --------------------------------------------------------------

// mapItemSummary converts one Browse itemSummary into a listing.Raw. It
// returns ok=false when the item has no itemId: without it there is no
// source-native key to stamp as provenance (Raw.SourceKey), so the item
// cannot become a valid Raw and must be skipped rather than emitted broken.
//
// Body is left empty: item_summary/search does not return a description:
// that requires a follow-up call to Browse's getItem detail endpoint
// (GET /buy/browse/v1/item/{itemId}), which is a future enrichment step, not
// part of this connector's single search call.
func mapItemSummary(is itemSummary, now time.Time) (listing.Raw, bool) {
	if strings.TrimSpace(is.ItemID) == "" {
		return listing.Raw{}, false
	}

	var priceCents int64
	var currency string
	if is.Price != nil {
		var err error
		priceCents, err = parsePriceToCents(is.Price.Value)
		if err != nil {
			// A malformed price string is untrusted-source noise, not a
			// reason to drop otherwise-usable provenance; leave PriceCents
			// at 0 ("unknown"), per Raw's documented convention.
			priceCents = 0
		}
		currency = is.Price.Currency
	}

	conditionRaw := is.ConditionID
	if conditionRaw == "" {
		conditionRaw = is.Condition
	}

	aspects := map[string]string{}
	if is.Condition != "" {
		aspects["condition"] = is.Condition
	}
	if is.ConditionID != "" {
		aspects["conditionId"] = is.ConditionID
	}
	// Surface eBay's PUBLIC seller-quality fields for downstream bucketing. We
	// deliberately do NOT surface the seller username: it is eBay user PII, and
	// nagus stores none (Marketplace Account Deletion opt-out, see SECURITY.md).
	// The extract stage maps these raw values to coarse, non-identifying tiers;
	// the raw values themselves are never persisted.
	if is.Seller != nil {
		if is.Seller.FeedbackPercentage != "" {
			aspects["seller_feedback_pct"] = is.Seller.FeedbackPercentage
		}
		if is.Seller.FeedbackScore != nil {
			aspects["seller_feedback_score"] = strconv.Itoa(*is.Seller.FeedbackScore)
		}
	}
	if is.ItemLocation != nil && is.ItemLocation.Country != "" {
		aspects["item_location_country"] = is.ItemLocation.Country
	}

	return listing.Raw{
		SourceID:     SourceID,
		SourceKey:    is.ItemID,
		SourceURL:    is.ItemWebURL,
		Title:        is.Title,
		Body:         "",
		PriceCents:   priceCents,
		Currency:     currency,
		ConditionRaw: conditionRaw,
		Aspects:      aspects,
		SeenAt:       now,
	}, true
}

// parsePriceToCents converts an eBay Browse price.value decimal string in
// major units (e.g. "129.99", "89.5", "10") into integer minor units
// (cents), without float rounding loss. It parses the integer dollar part
// and the fractional cents part separately: a missing fraction means 0
// cents, a single fractional digit is treated as a tenths-of-a-dollar value
// (right-padded with a zero, e.g. "89.5" -> 8950), and any fractional digits
// beyond two are truncated (currency has no smaller unit here).
func parsePriceToCents(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty price value")
	}

	neg := false
	if strings.HasPrefix(value, "-") {
		neg = true
		value = value[1:]
	}

	whole, frac, hasFrac := strings.Cut(value, ".")
	if whole == "" {
		whole = "0"
	}
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse dollars from %q: %w", value, err)
	}

	var cents int64
	if hasFrac {
		switch {
		case len(frac) == 0:
			cents = 0
		case len(frac) == 1:
			frac += "0"
			fallthrough
		default:
			c, err := strconv.ParseInt(frac[:2], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse cents from %q: %w", value, err)
			}
			cents = c
		}
	}

	total := dollars*100 + cents
	if neg {
		total = -total
	}
	return total, nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
