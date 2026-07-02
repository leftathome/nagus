// Package geo enriches US land listing coordinates with free, no-key
// US-government geospatial signals: flood zone (FEMA), elevation (USGS),
// dominant soil series (USDA), and wetland classification (USFWS).
//
// This runs only on hard-filter survivors (see docs/design section 9), so
// throughput is not a concern, but reliability of the pipeline as a whole is:
// any single upstream being down, rate-limited, or returning "no data" for a
// point must degrade to a partial Result with that field left nil and the
// failure reason recorded in Result.Errors -- it must never fail the other
// sources or the caller's pipeline. Enrich only returns a non-nil error for
// programmer errors (invalid input), never for upstream trouble.
//
// All five endpoints wired here are free, public, and require no API key.
// Government GIS services are known to drift (layer indices, hostnames,
// response envelopes change without notice); each fetch function documents
// the endpoint it targets so a maintainer can re-verify the shape against
// the live service. See the per-source doc comments below for which ones
// are marked "verify against live service" in the implementation report.
package geo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/item"
)

// Default upstream endpoints. All are overridable on Enricher so tests can
// point at an httptest.Server instead of the real network.
const (
	// DefaultCensusGeocoderURL is the Census Bureau's one-line address
	// geocoder. Docs: https://geocoding.geo.census.gov/geocoder/Geocoding_Services_API.pdf
	DefaultCensusGeocoderURL = "https://geocoding.geo.census.gov/geocoder/locations/onelineaddress"

	// DefaultFEMANFHLURL is the FEMA National Flood Hazard Layer ArcGIS
	// MapServer "Flood Hazard Zones" layer (layer id 28 in the public NFHL
	// service as of this writing). Queried via the ArcGIS REST "query"
	// operation (point-in-polygon identify). Layer id and hostname have
	// changed before -- re-verify against
	// https://hazards.fema.gov/gis/nfhl/rest/services/public/NFHL/MapServer
	// before relying on this in production.
	DefaultFEMANFHLURL = "https://hazards.fema.gov/gis/nfhl/rest/services/public/NFHL/MapServer/28/query"

	// DefaultUSGSElevationURL is the USGS 3DEP Elevation Point Query
	// Service (EPQS) v1 JSON endpoint. Docs:
	// https://apps.nationalmap.gov/epqs/
	DefaultUSGSElevationURL = "https://epqs.nationalmap.gov/v1/json"

	// DefaultSSURGOURL is the USDA Soil Data Access (SDA) tabular REST
	// endpoint, queried with a SQL statement over the SSURGO snapshot to
	// find the dominant map unit component at a point. Docs:
	// https://sdmdataaccess.sc.egov.usda.gov/Documentation/WebServiceHelp.htm
	DefaultSSURGOURL = "https://sdmdataaccess.sc.egov.usda.gov/Tabular/post.rest"

	// DefaultNWIURL is the US Fish & Wildlife Service National Wetlands
	// Inventory (NWI) ArcGIS MapServer wetlands layer (layer 0), queried
	// via the ArcGIS REST "query" operation. The NWI service hostname has
	// moved more than once historically -- re-verify against
	// https://www.fws.gov/program/national-wetlands-inventory/data-download
	// before relying on this in production.
	DefaultNWIURL = "https://www.fws.gov/wetlandsmapservice/rest/services/Wetlands/MapServer/0/query"
)

// defaultTimeout bounds a single upstream call so one slow/hung gov service
// cannot stall the whole Enrich call indefinitely; callers should still pass
// a context with their own deadline.
const defaultTimeout = 15 * time.Second

// FloodInfo is the FEMA NFHL flood zone designation at a point.
type FloodInfo struct {
	// Zone is the FEMA flood zone code, e.g. "AE", "X", "VE".
	Zone string
	// ZoneSubtype is the NFHL zone subtype qualifier (may be empty), e.g.
	// "0.2 PCT ANNUAL CHANCE FLOOD HAZARD".
	ZoneSubtype string
}

// ElevationInfo is the USGS 3DEP point elevation.
type ElevationInfo struct {
	// Meters is the ground elevation at the point, in meters.
	Meters float64
}

// SoilInfo is the dominant SSURGO soil map unit component at a point.
type SoilInfo struct {
	// Series is the soil series / component name, e.g. "Chester".
	Series string
	// MapUnitName is the full SSURGO map unit name.
	MapUnitName string
	// ComponentPct is the dominant component's percent of the map unit
	// (0-100), or -1 if unknown.
	ComponentPct int
}

// WetlandsInfo is the USFWS NWI wetland classification at a point.
type WetlandsInfo struct {
	// Type is the human-readable NWI wetland type, e.g. "Freshwater
	// Forested/Shrub Wetland".
	Type string
	// Code is the NWI classification attribute code, e.g. "PFO1A".
	Code string
}

// Result is the outcome of enriching one point. Every source is a
// first-class "unavailable" state: a nil pointer means that source had no
// data (or was not queried); Errors records why a source failed, keyed by
// source name ("flood", "elevation", "soil", "wetlands"). A source with no
// entry in Errors and a nil field simply had no data for the point (e.g. no
// mapped flood zone), which is not a failure.
type Result struct {
	Flood     *FloodInfo
	Elevation *ElevationInfo
	Soil      *SoilInfo
	Wetlands  *WetlandsInfo
	// Errors maps source name -> failure reason, for sources that could not
	// be queried successfully (network error, non-2xx, unparsable body).
	Errors map[string]string
}

// Attribute keys ApplyTo writes into item.Item.Attributes.
const (
	AttrFloodZone   = "flood_zone"
	AttrElevationM  = "elevation_m"
	AttrSoilSeries  = "soil_series"
	AttrWetlandType = "wetland_type"
)

// ApplyTo folds the populated (non-nil) signals in Result into it.Attributes
// as strings, so geo enrichment integrates with the existing item model.
// Sources with no data are simply omitted -- ApplyTo never writes an
// "unavailable" sentinel string, since absence of the key already means
// "unavailable" to downstream readers of item.Item.Attributes.
func (r Result) ApplyTo(it *item.Item) {
	if it == nil {
		return
	}
	if it.Attributes == nil {
		it.Attributes = make(map[string]string)
	}
	if r.Flood != nil && r.Flood.Zone != "" {
		it.Attributes[AttrFloodZone] = r.Flood.Zone
	}
	if r.Elevation != nil {
		it.Attributes[AttrElevationM] = strconv.FormatFloat(r.Elevation.Meters, 'f', -1, 64)
	}
	if r.Soil != nil && r.Soil.Series != "" {
		it.Attributes[AttrSoilSeries] = r.Soil.Series
	}
	if r.Wetlands != nil && r.Wetlands.Type != "" {
		it.Attributes[AttrWetlandType] = r.Wetlands.Type
	}
}

// InvalidCoordinateError is returned by Enrich for out-of-range lat/lon --
// a programmer error, distinct from any upstream failure.
type InvalidCoordinateError struct {
	Lat, Lon float64
}

func (e *InvalidCoordinateError) Error() string {
	return fmt.Sprintf("geo: invalid coordinates lat=%v lon=%v", e.Lat, e.Lon)
}

// Enricher fetches free US-gov geospatial signals for a point. The zero
// value is not ready to use; construct with NewEnricher.
type Enricher struct {
	// Client performs HTTP requests. Injectable for tests; nil is invalid
	// on a value obtained any way other than NewEnricher.
	Client *http.Client

	CensusGeocoderURL string
	FEMANFHLURL       string
	USGSElevationURL  string
	SSURGOURL         string
	NWIURL            string
}

// NewEnricher builds an Enricher pointed at the real government endpoints.
// If client is nil, a default client with defaultTimeout is used.
func NewEnricher(client *http.Client) *Enricher {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Enricher{
		Client:            client,
		CensusGeocoderURL: DefaultCensusGeocoderURL,
		FEMANFHLURL:       DefaultFEMANFHLURL,
		USGSElevationURL:  DefaultUSGSElevationURL,
		SSURGOURL:         DefaultSSURGOURL,
		NWIURL:            DefaultNWIURL,
	}
}

// sourceResult is the outcome of fetching one source: either a populated
// value, or (nil, nil) for "no data at this point", or (nil, err) for a
// fetch failure.
type sourceJob struct {
	name string
	run  func() (any, error)
}

// Enrich fetches all four signals for a point concurrently and folds their
// outcomes into a Result. It returns a non-nil error only for invalid input
// (see InvalidCoordinateError); any number of upstream sources failing or
// having no data is reflected in the returned Result, never as an error.
func (e *Enricher) Enrich(ctx context.Context, lat, lon float64) (Result, error) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return Result{}, &InvalidCoordinateError{Lat: lat, Lon: lon}
	}

	jobs := []sourceJob{
		{name: "flood", run: func() (any, error) { return e.fetchFlood(ctx, lat, lon) }},
		{name: "elevation", run: func() (any, error) { return e.fetchElevation(ctx, lat, lon) }},
		{name: "soil", run: func() (any, error) { return e.fetchSoil(ctx, lat, lon) }},
		{name: "wetlands", run: func() (any, error) { return e.fetchWetlands(ctx, lat, lon) }},
	}

	type outcome struct {
		name  string
		value any
		err   error
	}
	outcomes := make(chan outcome, len(jobs))
	for _, j := range jobs {
		j := j
		go func() {
			v, err := j.run()
			outcomes <- outcome{name: j.name, value: v, err: err}
		}()
	}

	res := Result{Errors: make(map[string]string)}
	for range jobs {
		o := <-outcomes
		if o.err != nil {
			res.Errors[o.name] = o.err.Error()
			continue
		}
		switch o.name {
		case "flood":
			if v, ok := o.value.(*FloodInfo); ok {
				res.Flood = v
			}
		case "elevation":
			if v, ok := o.value.(*ElevationInfo); ok {
				res.Elevation = v
			}
		case "soil":
			if v, ok := o.value.(*SoilInfo); ok {
				res.Soil = v
			}
		case "wetlands":
			if v, ok := o.value.(*WetlandsInfo); ok {
				res.Wetlands = v
			}
		}
	}
	if len(res.Errors) == 0 {
		res.Errors = nil
	}
	return res, nil
}

// --- Census geocoder -------------------------------------------------------

type censusGeocodeResponse struct {
	Result struct {
		AddressMatches []struct {
			Coordinates struct {
				X float64 `json:"x"` // longitude
				Y float64 `json:"y"` // latitude
			} `json:"coordinates"`
		} `json:"addressMatches"`
	} `json:"result"`
}

// Geocode resolves a free-form US address to (lat, lon) using the Census
// Bureau's one-line geocoder:
//
//	GET https://geocoding.geo.census.gov/geocoder/locations/onelineaddress
//	    ?address=<addr>&benchmark=Public_AR_Current&format=json
//
// Unlike Enrich, Geocode returns a real error on failure or no match: a
// caller cannot proceed to point enrichment without coordinates, so there is
// no meaningful "partial" result here.
func (e *Enricher) Geocode(ctx context.Context, address string) (lat, lon float64, err error) {
	if strings.TrimSpace(address) == "" {
		return 0, 0, fmt.Errorf("geo: empty address")
	}
	q := url.Values{}
	q.Set("address", address)
	q.Set("benchmark", "Public_AR_Current")
	q.Set("format", "json")
	reqURL := e.CensusGeocoderURL + "?" + q.Encode()

	var parsed censusGeocodeResponse
	if err := e.getJSON(ctx, reqURL, &parsed); err != nil {
		return 0, 0, fmt.Errorf("geo: census geocode: %w", err)
	}
	if len(parsed.Result.AddressMatches) == 0 {
		return 0, 0, fmt.Errorf("geo: census geocode: no address match for %q", address)
	}
	m := parsed.Result.AddressMatches[0]
	return m.Coordinates.Y, m.Coordinates.X, nil
}

// --- FEMA NFHL flood zone ---------------------------------------------------

type arcgisQueryResponse struct {
	Features []struct {
		Attributes map[string]any `json:"attributes"`
	} `json:"features"`
	// Some ArcGIS error responses come back 200 OK with an "error" object
	// instead of an HTTP error status.
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// fetchFlood queries the FEMA NFHL "Flood Hazard Zones" layer via the
// ArcGIS REST query (point-in-polygon identify) operation. See
// DefaultFEMANFHLURL for the endpoint and its caveats.
func (e *Enricher) fetchFlood(ctx context.Context, lat, lon float64) (*FloodInfo, error) {
	reqURL := arcgisPointQueryURL(e.FEMANFHLURL, lat, lon, "FLD_ZONE,ZONE_SUBTY")
	var parsed arcgisQueryResponse
	if err := e.getJSON(ctx, reqURL, &parsed); err != nil {
		return nil, fmt.Errorf("fema nfhl: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("fema nfhl: arcgis error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Features) == 0 {
		return nil, nil // no mapped flood zone at this point -- not a failure
	}
	attrs := parsed.Features[0].Attributes
	zone, _ := attrs["FLD_ZONE"].(string)
	if zone == "" {
		return nil, nil
	}
	subtype, _ := attrs["ZONE_SUBTY"].(string)
	return &FloodInfo{Zone: zone, ZoneSubtype: subtype}, nil
}

// --- USGS 3DEP elevation -----------------------------------------------------

type usgsElevationResponse struct {
	Value json.Number `json:"value"`
}

// fetchElevation queries the USGS 3DEP Elevation Point Query Service:
//
//	GET https://epqs.nationalmap.gov/v1/json?x=<lon>&y=<lat>&units=Meters&wkid=4326
//
// See DefaultUSGSElevationURL.
func (e *Enricher) fetchElevation(ctx context.Context, lat, lon float64) (*ElevationInfo, error) {
	q := url.Values{}
	q.Set("x", strconv.FormatFloat(lon, 'f', -1, 64))
	q.Set("y", strconv.FormatFloat(lat, 'f', -1, 64))
	q.Set("units", "Meters")
	q.Set("wkid", "4326")
	q.Set("includeDate", "false")
	reqURL := e.USGSElevationURL + "?" + q.Encode()

	var parsed usgsElevationResponse
	if err := e.getJSON(ctx, reqURL, &parsed); err != nil {
		return nil, fmt.Errorf("usgs 3dep: %w", err)
	}
	if parsed.Value == "" {
		return nil, nil
	}
	meters, err := parsed.Value.Float64()
	if err != nil {
		return nil, fmt.Errorf("usgs 3dep: unparsable elevation value %q: %w", parsed.Value, err)
	}
	// EPQS returns a large negative sentinel (historically -1000000) for
	// points outside coverage; treat that as "no data", not a value.
	if meters <= -999999 {
		return nil, nil
	}
	return &ElevationInfo{Meters: meters}, nil
}

// --- USDA SSURGO soil (Soil Data Access) -------------------------------------

type sdaRequest struct {
	Format string `json:"format"`
	Query  string `json:"query"`
}

type sdaResponse struct {
	// Table is [header-row, data-rows...]; each row is column values in
	// column order, all returned as strings by SDA's JSON serializer.
	Table [][]string `json:"Table"`
}

// fetchSoil queries USDA Soil Data Access with a SQL statement that finds
// the dominant SSURGO map unit component at a point:
//
//	POST https://sdmdataaccess.sc.egov.usda.gov/Tabular/post.rest
//	Content-Type: application/json
//	{"format":"JSON","query":"SELECT ... SDA_Get_Mukey_from_intersection_with_WktWgs84(...) ..."}
//
// See DefaultSSURGOURL. The exact stored-function name/signature for
// point-to-mukey lookup is worth re-verifying against
// https://sdmdataaccess.sc.egov.usda.gov/Documentation/WebServiceHelp.htm
// before relying on this in production.
func (e *Enricher) fetchSoil(ctx context.Context, lat, lon float64) (*SoilInfo, error) {
	point := fmt.Sprintf("point (%s %s)", strconv.FormatFloat(lon, 'f', -1, 64), strconv.FormatFloat(lat, 'f', -1, 64))
	query := fmt.Sprintf(`SELECT TOP 1 mu.muname, c.compname, c.comppct_r
FROM SDA_Get_Mukey_from_intersection_with_WktWgs84('%s') AS pt
INNER JOIN mapunit mu ON mu.mukey = pt.mukey
INNER JOIN component c ON c.mukey = mu.mukey AND c.majcompflag = 'Yes'
ORDER BY c.comppct_r DESC`, point)

	body, err := json.Marshal(sdaRequest{Format: "JSON", Query: query})
	if err != nil {
		return nil, fmt.Errorf("ssurgo: encode request: %w", err)
	}

	var parsed sdaResponse
	if err := e.postJSON(ctx, e.SSURGOURL, body, &parsed); err != nil {
		return nil, fmt.Errorf("ssurgo: %w", err)
	}
	if len(parsed.Table) < 2 {
		return nil, nil // no map unit / component at this point
	}
	row := parsed.Table[1]
	if len(row) < 3 || row[1] == "" {
		return nil, nil
	}
	pct := -1
	if p, err := strconv.Atoi(row[2]); err == nil {
		pct = p
	}
	return &SoilInfo{MapUnitName: row[0], Series: row[1], ComponentPct: pct}, nil
}

// --- USFWS NWI wetlands -------------------------------------------------------

// fetchWetlands queries the USFWS National Wetlands Inventory wetlands
// layer via the ArcGIS REST query (point-in-polygon identify) operation.
// See DefaultNWIURL.
func (e *Enricher) fetchWetlands(ctx context.Context, lat, lon float64) (*WetlandsInfo, error) {
	reqURL := arcgisPointQueryURL(e.NWIURL, lat, lon, "WETLAND_TYPE,ATTRIBUTE")
	var parsed arcgisQueryResponse
	if err := e.getJSON(ctx, reqURL, &parsed); err != nil {
		return nil, fmt.Errorf("nwi: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("nwi: arcgis error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Features) == 0 {
		return nil, nil // not mapped as wetland -- not a failure
	}
	attrs := parsed.Features[0].Attributes
	wtype, _ := attrs["WETLAND_TYPE"].(string)
	if wtype == "" {
		return nil, nil
	}
	code, _ := attrs["ATTRIBUTE"].(string)
	return &WetlandsInfo{Type: wtype, Code: code}, nil
}

// --- shared HTTP plumbing -----------------------------------------------------

// arcgisPointQueryURL builds a standard ArcGIS REST "query" operation URL
// for a point-in-polygon identify at (lat, lon), asking for outFields.
func arcgisPointQueryURL(base string, lat, lon float64, outFields string) string {
	geometry := fmt.Sprintf(`{"x":%s,"y":%s,"spatialReference":{"wkid":4326}}`,
		strconv.FormatFloat(lon, 'f', -1, 64), strconv.FormatFloat(lat, 'f', -1, 64))
	q := url.Values{}
	q.Set("geometry", geometry)
	q.Set("geometryType", "esriGeometryPoint")
	q.Set("inSR", "4326")
	q.Set("spatialRel", "esriSpatialRelIntersects")
	q.Set("outFields", outFields)
	q.Set("returnGeometry", "false")
	q.Set("f", "json")
	return base + "?" + q.Encode()
}

// getJSON performs a GET request and decodes a JSON body into out. Any
// non-2xx status or body-read/decode failure is returned as an error; the
// caller is responsible for treating that as a per-source failure.
func (e *Enricher) getJSON(ctx context.Context, reqURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	return e.doJSON(req, out)
}

// postJSON performs a POST request with a JSON body and decodes a JSON
// response into out.
func (e *Enricher) postJSON(ctx context.Context, reqURL string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return e.doJSON(req, out)
}

func (e *Enricher) doJSON(req *http.Request, out any) error {
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, truncate(data, 200))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("empty response body")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
