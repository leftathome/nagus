// Package zillapi implements a nagus-direct listing.Connector over Zillapi's
// Zillow property-data API (https://api.zillapi.com, OpenAPI 3.1 spec at
// https://zillapi.com/openapi.json). It is the land acquisition source, replacing
// the Craigslist RSS connector Craigslist retired.
//
// It issues one POST /v1/search per Fetch with structured filters and maps the
// returned search rows straight into listing.Raw. See
// docs/design/2026-07-29-zillapi-land-connector.md for the full design.
//
// # Cost model -- READ THIS BEFORE CHANGING THE POLL CADENCE
//
// Zillapi bills ONE CREDIT PER RESULT RETURNED, not per call ("one credit equals
// one property record returned"). Failed calls are free. That inverts the usual
// polling instinct: re-seeing the same listing costs a credit EVERY time, so a
// frequent poll of a broad area is ruinously expensive (50 lots on a 30m poll is
// ~2,400 credits/day against a 1,000-credit/month plan).
//
// Three levers keep the bill down, and all three are cost controls rather than
// mere filtering:
//
//   - Config.DaysOnZillow asks upstream for only recently-listed lots, so each
//     poll bills for new inventory instead of the whole standing set.
//   - Config.MinLotAcres and Config.MaxPriceUSD push the acreage/price window
//     upstream, narrowing the result set BEFORE it is billed.
//   - Config.MaxItems caps the worst case per call.
//
// The caller (the spine's scheduler) owns cadence; this package does not sleep or
// schedule. Pair it with a DAILY interval, not the 30m used for eBay.
//
// # Trust boundary
//
// Title and Aspects values on the emitted Raw are UNTRUSTED per internal/listing's
// contract: they carry upstream strings (address, status text) and this connector
// does no sanitization, only field mapping -- that is the glovebox boundary's job.
// PriceCents, Currency and the acreage aspect are derived/structured scalars,
// trusted once computed here. Note the search shape carries no free-text
// description at all, so Body is left empty rather than synthesized.
package zillapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the stable connector identity stamped onto every Raw this package
// emits.
const SourceID = "zillapi"

// Defaults and upstream invariants.
const (
	// DefaultBaseURL is the Zillapi API root. Override in tests to point at an
	// httptest.Server.
	DefaultBaseURL = "https://api.zillapi.com"
	// searchPath is the structured-search endpoint (1 credit per result).
	searchPath = "/v1/search"
	// zillowRoot absolutizes the relative detailUrl rows carry.
	zillowRoot = "https://www.zillow.com"

	// SqftPerAcre converts an acreage floor into the square feet the upstream
	// lotSize filter expects. NOTE: the OpenAPI spec types lotSize.min as a bare
	// integer with NO unit stated; Zillow's own searchQueryState uses square
	// feet, which is the assumption encoded here. The first live capture
	// confirms it -- if lotSize turns out to be acres, this constant becomes 1.
	SqftPerAcre = 43560

	// MaxSyncItems is the largest maxItems that still returns a synchronous 200.
	// At 51+ upstream switches to a 202 + async job, which this connector does
	// not implement, so requests are clamped here instead.
	MaxSyncItems = 50

	// DefaultMaxItems is a deliberately small default: every returned row costs
	// a credit.
	DefaultMaxItems = 25
	// DefaultDaysOnZillow limits a poll to the last week of new listings.
	DefaultDaysOnZillow = "7"
	// DefaultCurrency -- Zillapi is US-only.
	DefaultCurrency = "USD"
)

// Sentinel errors callers can branch on. The ingest loop treats every Fetch error
// the same (log + continue, per-source isolation), but these let tests and
// operators tell a transient throttle from a misconfiguration.
var (
	// ErrRateLimited is a 429 from upstream (per-minute cap, or concurrency).
	ErrRateLimited = errors.New("zillapi: rate limited")
	// ErrAsyncUnsupported is a 202: the request went async and results arrive via
	// a job. Surfaced explicitly so an async response is never mistaken for a
	// healthy empty poll.
	ErrAsyncUnsupported = errors.New("zillapi: upstream returned an async job; v1 requires a synchronous search")
	// ErrNoAPIKey is a configuration error, not a transient failure.
	ErrNoAPIKey = errors.New("zillapi: no api key configured")
)

// BBox is a bounding box in decimal degrees. Upstream REQUIRES one: a search is
// anchored by a box, never by a place name, and a free-text location alone is
// rejected with 400 invalid_filters before it ever reaches Zillow.
type BBox struct {
	West  float64
	South float64
	East  float64
	North float64
}

// Config configures a Connector.
type Config struct {
	// Name is the operator-chosen unique source name in a multi-source
	// deployment. When set, SourceID() returns "zillapi:<Name>" so several
	// Zillapi sources (e.g. different regions) have distinct identities
	// (freshness purge is scoped by SourceID). Empty -> the bare "zillapi"
	// identity.
	Name string

	// APIKey is the Zillapi key (zk_...), sent as a bearer token. Required for
	// live fetches; unused when FixturePath is set.
	APIKey string

	// BBox is the search area. Required for live fetches.
	BBox BBox

	// MinLotAcres is the acreage floor, pushed upstream as lotSize.min in square
	// feet. Zero omits the filter (and bills for every lot in the box).
	MinLotAcres float64
	// MaxPriceUSD is the price ceiling in whole dollars. Zero omits the filter.
	MaxPriceUSD int
	// DaysOnZillow restricts results to recently-listed lots. Valid upstream
	// values: 1, 7, 14, 30, 90, 6m, 12m, 24m, 36m. Defaults to
	// DefaultDaysOnZillow; set to "-" to omit the filter entirely (expensive).
	DaysOnZillow string
	// MaxItems caps results per call. Defaults to DefaultMaxItems and is clamped
	// to MaxSyncItems.
	MaxItems int
	// Location is an optional decorative place label passed through as
	// usersSearchTerm. It is NOT a substitute for BBox.
	Location string

	// HTTPClient performs requests. Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// BaseURL is the API root. Defaults to DefaultBaseURL.
	BaseURL string
	// Now returns the observation time stamped as SeenAt. Defaults to time.Now.
	Now func() time.Time

	// FixturePath, when non-empty, makes Fetch read a local JSON file instead of
	// making any network call -- the offline proving path. Costs no credits.
	FixturePath string
}

// Connector implements listing.Connector over Zillapi's structured search.
type Connector struct {
	cfg Config
}

// NewConnector builds a Connector from cfg, filling in defaults.
func NewConnector(cfg Config) *Connector {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxItems <= 0 {
		cfg.MaxItems = DefaultMaxItems
	}
	if cfg.MaxItems > MaxSyncItems {
		cfg.MaxItems = MaxSyncItems
	}
	if cfg.DaysOnZillow == "" {
		cfg.DaysOnZillow = DefaultDaysOnZillow
	}
	cfg.Name = strings.TrimSpace(cfg.Name)
	return &Connector{cfg: cfg}
}

// SourceID returns the connector identity stamped onto every Raw. With a
// configured Config.Name it is "zillapi:<Name>"; without one, the bare constant.
func (c *Connector) SourceID() string {
	if c.cfg.Name != "" {
		return SourceID + ":" + c.cfg.Name
	}
	return SourceID
}

// Fetch runs one search and returns the rows as listing.Raw. When
// Config.FixturePath is set it reads that file and makes no network call.
//
// Cost: one credit per row returned (zero when reading a fixture).
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	var data []byte
	if c.cfg.FixturePath != "" {
		d, err := os.ReadFile(c.cfg.FixturePath)
		if err != nil {
			return nil, fmt.Errorf("zillapi: read fixture %s: %w", c.cfg.FixturePath, err)
		}
		data = d
	} else {
		d, err := c.search(ctx)
		if err != nil {
			return nil, err
		}
		data = d
	}

	var resp searchResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("zillapi: decode search response: %w", err)
	}

	now := c.cfg.Now()
	raws := make([]listing.Raw, 0, len(resp.Data))
	for _, row := range resp.Data {
		r, ok := c.mapRow(row, now)
		if !ok {
			// No zpid: no source-native key, so no provenance and no valid Raw.
			// Skip rather than emit a broken record.
			continue
		}
		raws = append(raws, r)
	}
	return raws, nil
}

// search issues the POST and returns the raw response body on a 200.
func (c *Connector) search(ctx context.Context) ([]byte, error) {
	if strings.TrimSpace(c.cfg.APIKey) == "" {
		return nil, ErrNoAPIKey
	}

	payload, err := json.Marshal(c.buildRequest())
	if err != nil {
		return nil, fmt.Errorf("zillapi: encode request: %w", err)
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + searchPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("zillapi: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zillapi: request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("zillapi: read response body: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case resp.StatusCode == http.StatusAccepted:
		// Async job. v1 keeps maxItems <= MaxSyncItems precisely to avoid this,
		// so reaching here means an upstream behavior change worth surfacing.
		return nil, fmt.Errorf("%w (status 202)", ErrAsyncUnsupported)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: %s", ErrRateLimited, describeError(body))
	default:
		return nil, fmt.Errorf("zillapi: search returned status %d: %s", resp.StatusCode, describeError(body))
	}
}

// buildRequest assembles the search payload. Every filter here except status and
// bbox is optional AND a cost control -- see the package comment.
func (c *Connector) buildRequest() searchRequest {
	f := searchFilters{
		Status: "for_sale",
		BBox: bboxPayload{
			West:  c.cfg.BBox.West,
			South: c.cfg.BBox.South,
			East:  c.cfg.BBox.East,
			North: c.cfg.BBox.North,
		},
		// "lot" is Zillow's bare-land home type -- the whole point of this source.
		HomeTypes: []string{"lot"},
	}
	if c.cfg.MinLotAcres > 0 {
		// Acres -> square feet; see the SqftPerAcre note about the unconfirmed
		// upstream unit.
		f.LotSize = &rangeFilter{Min: int64(c.cfg.MinLotAcres * SqftPerAcre)}
	}
	if c.cfg.MaxPriceUSD > 0 {
		f.Price = &rangeFilter{Max: int64(c.cfg.MaxPriceUSD)}
	}
	if c.cfg.DaysOnZillow != "" && c.cfg.DaysOnZillow != "-" {
		f.DaysOnZillow = c.cfg.DaysOnZillow
	}
	if c.cfg.Location != "" {
		f.Location = c.cfg.Location
	}
	return searchRequest{
		Filters: f,
		// PAGINATION is the default upstream extraction mode.
		ExtractionMethod: "PAGINATION",
		MaxItems:         c.cfg.MaxItems,
		Async:            false,
	}
}

// mapRow converts one search row into a listing.Raw. ok=false means the row has
// no zpid and therefore no source-native key, so it cannot become a valid Raw.
func (c *Connector) mapRow(row searchResultRow, now time.Time) (listing.Raw, bool) {
	zpid := strings.TrimSpace(row.ZPID)
	if zpid == "" {
		return listing.Raw{}, false
	}

	aspects := map[string]string{}
	// Acreage as a TYPED aspect, normalized to acres. Absent (not zero) when
	// upstream omits lot area: unknown acreage is not zero acreage, and the
	// hard-filter must be able to tell them apart.
	if acres, ok := row.acres(); ok {
		aspects["acreage"] = formatAcres(acres)
	}
	// Full formatted street address, kept distinct from the coarse city label
	// below: parcel lookups need the street address, geo needs coordinates, and
	// "location" is only a display/region hint.
	if a := strings.TrimSpace(row.Address); a != "" {
		aspects["street_address"] = a
	}
	if city := strings.TrimSpace(row.AddressCity); city != "" {
		loc := city
		if st := strings.TrimSpace(row.AddressState); st != "" {
			loc += ", " + st
		}
		aspects["location"] = loc
	}
	if zip := strings.TrimSpace(row.AddressZipcode); zip != "" {
		aspects["zip"] = zip
	}
	// Assessed value: the structure/land-value signal land scoring otherwise pays
	// Rentcast for. Present on only some rows, so strictly optional.
	if row.TaxAssessedValue != nil && *row.TaxAssessedValue > 0 {
		aspects["assessed_value_cents"] = strconv.FormatInt(int64(*row.TaxAssessedValue*100+0.5), 10)
	}
	if row.DaysOnZillow != nil && *row.DaysOnZillow >= 0 {
		aspects["days_on_market"] = strconv.Itoa(*row.DaysOnZillow)
	}
	if row.LatLong.Latitude != 0 || row.LatLong.Longitude != 0 {
		aspects["lat"] = strconv.FormatFloat(row.LatLong.Latitude, 'f', -1, 64)
		aspects["lon"] = strconv.FormatFloat(row.LatLong.Longitude, 'f', -1, 64)
	}

	return listing.Raw{
		SourceID:  c.SourceID(),
		SourceKey: zpid,
		SourceURL: absoluteURL(row.DetailURL),
		Title:     strings.TrimSpace(row.Address), // UNTRUSTED upstream string
		// The search shape carries no description; leave Body empty rather than
		// composing prose from structured fields.
		Body:         "",
		PriceCents:   priceCents(row.UnformattedPrice),
		Currency:     DefaultCurrency,
		ConditionRaw: "", // not meaningful for land
		Aspects:      aspects,
		SeenAt:       now,
	}, true
}

// --- upstream wire types ------------------------------------------------------

type searchRequest struct {
	Filters          searchFilters `json:"filters"`
	ExtractionMethod string        `json:"extractionMethod,omitempty"`
	MaxItems         int           `json:"maxItems,omitempty"`
	Async            bool          `json:"async"`
}

type searchFilters struct {
	Status       string       `json:"status"`
	BBox         bboxPayload  `json:"bbox"`
	HomeTypes    []string     `json:"homeTypes,omitempty"`
	LotSize      *rangeFilter `json:"lotSize,omitempty"`
	Price        *rangeFilter `json:"price,omitempty"`
	DaysOnZillow string       `json:"daysOnZillow,omitempty"`
	Location     string       `json:"location,omitempty"`
}

type bboxPayload struct {
	West  float64 `json:"west"`
	South float64 `json:"south"`
	East  float64 `json:"east"`
	North float64 `json:"north"`
}

// rangeFilter is a min/max pair. Zero fields are omitted so a min-only or
// max-only window does not accidentally pin the other end at 0.
type rangeFilter struct {
	Min int64 `json:"min,omitempty"`
	Max int64 `json:"max,omitempty"`
}

type searchResponse struct {
	Data []searchResultRow `json:"data"`
	Meta struct {
		Count int `json:"count"`
	} `json:"meta"`
	RequestID string `json:"request_id"`
}

// searchResultRow is the SEARCH shape -- lighter than the property detail record
// and with different field names. Only the fields nagus maps are declared.
type searchResultRow struct {
	ZPID             string  `json:"zpid"`
	ID               string  `json:"id"`
	Address          string  `json:"address"`
	AddressStreet    string  `json:"addressStreet"`
	AddressCity      string  `json:"addressCity"`
	AddressState     string  `json:"addressState"`
	AddressZipcode   string  `json:"addressZipcode"`
	Price            string  `json:"price"`
	UnformattedPrice float64 `json:"unformattedPrice"`
	StatusType       string  `json:"statusType"`
	StatusText       string  `json:"statusText"`
	HomeType         string  `json:"homeType"`
	DetailURL        string  `json:"detailUrl"`
	ImgSrc           string  `json:"imgSrc"`
	LatLong          struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"latLong"`
	// LotArea is the top-level lot-area object. It is NOT in the published OpenAPI
	// schema but IS present on every row of a real response (confirmed by live
	// capture 2026-07-30), and it is the more canonical of the two places lot area
	// appears, so it is preferred over hdpData.homeInfo below.
	LotArea *struct {
		Value     float64 `json:"value"`
		Unit      string  `json:"unit"`
		Formatted string  `json:"formatted"`
	} `json:"lotArea"`
	// TaxAssessedValue is likewise undeclared but present on some rows. It is the
	// assessed-value signal land scoring otherwise pays Rentcast for, so it is
	// carried through when available -- and frequently absent, so never required.
	TaxAssessedValue *float64 `json:"taxAssessedValue"`
	// DaysOnZillow at the top level is a freshness signal.
	DaysOnZillow *int `json:"daysOnZillow"`
	// HdpData is loosely typed upstream (additionalProperties: true). Lot area is
	// NOT in the declared search-row schema; it appears here as well, and is used
	// as the fallback when the top-level LotArea is missing.
	HdpData struct {
		HomeInfo struct {
			HomeStatus   string  `json:"homeStatus"`
			DaysOnZillow int     `json:"daysOnZillow"`
			LotAreaValue float64 `json:"lotAreaValue"`
			LotAreaUnit  string  `json:"lotAreaUnit"`
		} `json:"homeInfo"`
	} `json:"hdpData"`
}

// acres returns the row's lot area in acres, converting from whatever unit
// upstream reported. It prefers the top-level lotArea object and falls back to
// hdpData.homeInfo; ok=false when neither carries a usable value. Neither field is
// in the published schema, so absence is normal and never an error.
func (r searchResultRow) acres() (float64, bool) {
	if r.LotArea != nil && r.LotArea.Value > 0 {
		if a, ok := toAcres(r.LotArea.Value, r.LotArea.Unit); ok {
			return a, true
		}
	}
	return toAcres(r.HdpData.HomeInfo.LotAreaValue, r.HdpData.HomeInfo.LotAreaUnit)
}

// toAcres normalizes a (value, unit) lot area to acres.
func toAcres(v float64, unit string) (float64, bool) {
	if v <= 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "acres", "acre", "ac":
		return v, true
	case "sqft", "square feet", "sq ft", "sf", "":
		// Empty unit: Zillow's default for lot area is square feet.
		return v / SqftPerAcre, true
	default:
		// Unknown unit -- do not guess a conversion and publish a wrong acreage.
		return 0, false
	}
}

// --- helpers ------------------------------------------------------------------

// priceCents converts a dollar amount to integer cents. A missing or nonpositive
// price yields 0, which the item contract defines as "unknown/unpriced" --
// unpriced land is common and must still be emitted.
func priceCents(dollars float64) int64 {
	if dollars <= 0 {
		return 0
	}
	return int64(dollars*100 + 0.5)
}

// formatAcres renders an acreage without trailing zeros, in the plain decimal
// form the land extractor and hard-filter parse.
func formatAcres(a float64) string {
	return strconv.FormatFloat(a, 'f', -1, 64)
}

// absoluteURL turns the relative detailUrl rows often carry into a full link,
// leaving already-absolute URLs alone.
func absoluteURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return zillowRoot + u
}

// describeError renders an upstream error body for a Go error message. Zillapi's
// envelope is {error:{code,message,request_id}}; anything else is truncated raw
// so an unexpected body still tells the operator something.
func describeError(body []byte) string {
	var env struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		return fmt.Sprintf("%s: %s (request_id %s)", env.Error.Code, env.Error.Message, env.Error.RequestID)
	}
	return truncate(body, 200)
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
