// Package parcel provides a swappable interface for parcel data enrichment.
//
// A parcel database (Rentcast, Regrid, ATTOM, etc.) enriches listings with
// structured property metadata: assessed values, year built, acreage, and
// building-footprint data (Regrid only). The Provider interface allows swapping
// providers without changing callers.
//
// LIMITATION (v1 with Rentcast): Building-footprint (for manufactured-home
// detection and structure-present verification) is deferred. Rentcast returns
// assessed improvement value and year built, but not footprint. Regrid and ATTOM
// can add footprint later without Provider interface changes.
package parcel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/leftathome/nagus/internal/item"
)

// Provider is the interface for parcel data lookup. Implementations may target
// Rentcast, Regrid, ATTOM, or other parcel databases.
type Provider interface {
	// Lookup returns parcel data for the given address. If the address is not
	// found or the provider returns an error, Lookup returns a non-nil error.
	Lookup(ctx context.Context, address string) (ParcelData, error)
}

// ParcelData holds enrichment fields from a parcel database lookup.
// Fields are treated as optional; a zero value does not mean "none",
// it means "not provided by this provider". Use the Available map to
// distinguish "unknown" from "zero/none".
type ParcelData struct {
	AssessedImprovementValueCents int64           // assessed value of structure + improvements (cents)
	AssessedLandValueCents        int64           // assessed value of land only (cents)
	YearBuilt                     int             // year of construction; 0 = unknown
	AcreageAcres                  float64         // lot size in decimal acres; 0 = unknown
	Available                     map[string]bool // per-field "known" indicators
}

// StructurePresent returns true if the parcel data suggests a structure exists.
// It is true when BOTH the assessed improvement value is greater than zero AND
// year built is greater than zero. Both conditions must be satisfied to avoid
// false positives (zero values mean "unknown", not "none").
func (pd ParcelData) StructurePresent() bool {
	return pd.AssessedImprovementValueCents > 0 && pd.YearBuilt > 0
}

// ApplyTo folds parcel data into item.Attributes as strings.
// Keys follow the pattern: assessed_improvement_usd, assessed_land_usd, year_built,
// acreage, structure_present. Only fields marked Available are set.
func (pd ParcelData) ApplyTo(it *item.Item) {
	if it.Attributes == nil {
		it.Attributes = make(map[string]string)
	}
	if pd.Available["assessed_improvement"] {
		it.Attributes["assessed_improvement_usd"] = fmt.Sprintf("%.2f", float64(pd.AssessedImprovementValueCents)/100.0)
	}
	if pd.Available["assessed_land"] {
		it.Attributes["assessed_land_usd"] = fmt.Sprintf("%.2f", float64(pd.AssessedLandValueCents)/100.0)
	}
	if pd.Available["year_built"] {
		it.Attributes["year_built"] = strconv.Itoa(pd.YearBuilt)
	}
	if pd.Available["acreage"] {
		it.Attributes["acreage"] = strconv.FormatFloat(pd.AcreageAcres, 'f', 4, 64)
	}
	if pd.Available["structure_present"] {
		it.Attributes["structure_present"] = fmt.Sprintf("%v", pd.StructurePresent())
	}
}

// RentcastProvider implements Provider for the Rentcast property records API.
// Rentcast is free self-serve with ~50 requests/month (v1 default).
type RentcastProvider struct {
	client *http.Client
	apiKey string
}

// NewRentcastProvider creates a new Rentcast provider with the given http.Client
// and API key. The key is typically loaded from environment or vault at runtime,
// never hardcoded. For testing, inject a key and stub the client with httptest.
func NewRentcastProvider(client *http.Client, apiKey string) *RentcastProvider {
	return &RentcastProvider{
		client: client,
		apiKey: apiKey,
	}
}

// Lookup queries Rentcast for parcel data at the given address.
// It makes a request to https://api.rentcast.io/v1/properties with the X-Api-Key header.
func (rp *RentcastProvider) Lookup(ctx context.Context, address string) (ParcelData, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.rentcast.io/v1/properties", nil)
	if err != nil {
		return ParcelData{}, fmt.Errorf("parcel: create request: %w", err)
	}

	q := req.URL.Query()
	q.Set("address", address)
	req.URL.RawQuery = q.Encode()

	req.Header.Set("X-Api-Key", rp.apiKey)

	resp, err := rp.client.Do(req)
	if err != nil {
		return ParcelData{}, fmt.Errorf("parcel: fetch from rentcast: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ParcelData{}, fmt.Errorf("parcel: rentcast status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ParcelData{}, fmt.Errorf("parcel: read response: %w", err)
	}

	var props []struct {
		AssessedValue       int64   `json:"assessedValue"`
		AssessmentLandValue int64   `json:"assessmentLandValue"`
		YearBuilt           int     `json:"yearBuilt"`
		LotSize             float64 `json:"lotSize"` // in acres
	}
	if err := json.Unmarshal(body, &props); err != nil {
		return ParcelData{}, fmt.Errorf("parcel: parse response: %w", err)
	}

	if len(props) == 0 {
		return ParcelData{}, fmt.Errorf("parcel: no results for address")
	}

	// Use first result. Rentcast returns assessed values in dollars; convert to cents.
	prop := props[0]

	return ParcelData{
		AssessedImprovementValueCents: prop.AssessedValue * 100,
		AssessedLandValueCents:        prop.AssessmentLandValue * 100,
		YearBuilt:                     prop.YearBuilt,
		AcreageAcres:                  prop.LotSize,
		Available: map[string]bool{
			"assessed_improvement": prop.AssessedValue > 0,
			"assessed_land":        prop.AssessmentLandValue > 0,
			"year_built":           prop.YearBuilt > 0,
			"acreage":              prop.LotSize > 0,
			"structure_present":    true, // always compute, even if underlying data is partial
		},
	}, nil
}
