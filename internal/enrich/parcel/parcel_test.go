package parcel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/item"
)

func TestParcelDataStructurePresent(t *testing.T) {
	tests := []struct {
		name                   string
		improvement            int64
		yearBuilt              int
		expectStructurePresent bool
	}{
		{
			name:                   "both positive",
			improvement:            50000_00, // $50k
			yearBuilt:              2000,
			expectStructurePresent: true,
		},
		{
			name:                   "improvement zero (unknown)",
			improvement:            0,
			yearBuilt:              2000,
			expectStructurePresent: false,
		},
		{
			name:                   "yearBuilt zero (unknown)",
			improvement:            50000_00,
			yearBuilt:              0,
			expectStructurePresent: false,
		},
		{
			name:                   "both zero",
			improvement:            0,
			yearBuilt:              0,
			expectStructurePresent: false,
		},
		{
			name:                   "improvement positive, yearBuilt 1",
			improvement:            50000_00,
			yearBuilt:              1,
			expectStructurePresent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pd := ParcelData{
				AssessedImprovementValueCents: tt.improvement,
				YearBuilt:                     tt.yearBuilt,
			}
			if got := pd.StructurePresent(); got != tt.expectStructurePresent {
				t.Errorf("StructurePresent() = %v, want %v", got, tt.expectStructurePresent)
			}
		})
	}
}

func TestParcelDataApplyTo(t *testing.T) {
	tests := []struct {
		name       string
		pd         ParcelData
		expectKeys map[string]bool // key -> must be set in Attributes
	}{
		{
			name: "all fields available",
			pd: ParcelData{
				AssessedImprovementValueCents: 100000_00,
				AssessedLandValueCents:        50000_00,
				YearBuilt:                     2005,
				AcreageAcres:                  2.5,
				Available: map[string]bool{
					"assessed_improvement": true,
					"assessed_land":        true,
					"year_built":           true,
					"acreage":              true,
					"structure_present":    true,
				},
			},
			expectKeys: map[string]bool{
				"assessed_improvement_usd": true,
				"assessed_land_usd":        true,
				"year_built":               true,
				"acreage":                  true,
				"structure_present":        true,
			},
		},
		{
			name: "only improvement and year_built available",
			pd: ParcelData{
				AssessedImprovementValueCents: 100000_00,
				AssessedLandValueCents:        50000_00, // not available
				YearBuilt:                     2005,
				AcreageAcres:                  2.5, // not available
				Available: map[string]bool{
					"assessed_improvement": true,
					"assessed_land":        false,
					"year_built":           true,
					"acreage":              false,
					"structure_present":    true,
				},
			},
			expectKeys: map[string]bool{
				"assessed_improvement_usd": true,
				"assessed_land_usd":        false,
				"year_built":               true,
				"acreage":                  false,
				"structure_present":        true,
			},
		},
		{
			name: "no fields available",
			pd: ParcelData{
				Available: map[string]bool{
					"assessed_improvement": false,
					"assessed_land":        false,
					"year_built":           false,
					"acreage":              false,
					"structure_present":    false,
				},
			},
			expectKeys: map[string]bool{
				"assessed_improvement_usd": false,
				"assessed_land_usd":        false,
				"year_built":               false,
				"acreage":                  false,
				"structure_present":        false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := &item.Item{
				ID:       "test:123",
				Category: "land",
			}
			tt.pd.ApplyTo(it)

			for key, shouldExist := range tt.expectKeys {
				_, exists := it.Attributes[key]
				if shouldExist && !exists {
					t.Errorf("expected key %q in Attributes, not found", key)
				}
				if !shouldExist && exists {
					t.Errorf("unexpected key %q in Attributes", key)
				}
			}
		})
	}
}

func TestParcelDataApplyToFormats(t *testing.T) {
	pd := ParcelData{
		AssessedImprovementValueCents: 123456_78,
		AssessedLandValueCents:        54321,
		YearBuilt:                     1995,
		AcreageAcres:                  3.14159,
		Available: map[string]bool{
			"assessed_improvement": true,
			"assessed_land":        true,
			"year_built":           true,
			"acreage":              true,
			"structure_present":    true,
		},
	}

	it := &item.Item{
		ID:       "test:123",
		Category: "land",
	}
	pd.ApplyTo(it)

	tests := []struct {
		key      string
		expected string
	}{
		{"assessed_improvement_usd", "123456.78"},
		{"assessed_land_usd", "543.21"},
		{"year_built", "1995"},
		{"acreage", "3.1416"},
		{"structure_present", "true"},
	}

	for _, tt := range tests {
		if got, ok := it.Attributes[tt.key]; !ok {
			t.Errorf("key %q missing from Attributes", tt.key)
		} else if got != tt.expected {
			t.Errorf("Attributes[%q] = %q, want %q", tt.key, got, tt.expected)
		}
	}
}

func TestRentcastProviderLookup(t *testing.T) {
	t.Run("successful lookup", func(t *testing.T) {
		// Stub Rentcast API response
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify request shape
			if r.Method != "GET" {
				t.Errorf("expected GET, got %s", r.Method)
			}
			if r.Header.Get("X-Api-Key") != "test-api-key" {
				t.Errorf("expected X-Api-Key header, got %q", r.Header.Get("X-Api-Key"))
			}
			if !strings.Contains(r.URL.RawQuery, "address=") {
				t.Errorf("expected address in query, got %q", r.URL.RawQuery)
			}

			// Return stub response
			resp := []map[string]interface{}{
				{
					"assessedValue":       500000, // dollars
					"assessmentLandValue": 200000, // dollars
					"yearBuilt":           2010,
					"lotSize":             2.5, // acres
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		rt := &testRoundTripper{server: server}
		client := &http.Client{Transport: rt}
		rp := NewRentcastProvider(client, "test-api-key")

		pd, err := rp.Lookup(context.Background(), "123 Main St")
		if err != nil {
			t.Fatalf("Lookup failed: %v", err)
		}

		if pd.AssessedImprovementValueCents != 500000_00 {
			t.Errorf("AssessedImprovementValueCents = %d, want %d", pd.AssessedImprovementValueCents, 500000_00)
		}
		if pd.AssessedLandValueCents != 200000_00 {
			t.Errorf("AssessedLandValueCents = %d, want %d", pd.AssessedLandValueCents, 200000_00)
		}
		if pd.YearBuilt != 2010 {
			t.Errorf("YearBuilt = %d, want 2010", pd.YearBuilt)
		}
		if pd.AcreageAcres != 2.5 {
			t.Errorf("AcreageAcres = %f, want 2.5", pd.AcreageAcres)
		}

		// Verify all fields are marked available
		expectedAvailable := map[string]bool{
			"assessed_improvement": true,
			"assessed_land":        true,
			"year_built":           true,
			"acreage":              true,
			"structure_present":    true,
		}
		for key, expected := range expectedAvailable {
			if got, ok := pd.Available[key]; !ok || got != expected {
				t.Errorf("Available[%q] = %v, want %v", key, got, expected)
			}
		}

		// Verify structure_present is true for this data
		if !pd.StructurePresent() {
			t.Error("StructurePresent() = false, want true")
		}
	})

	t.Run("partial data (some fields zero/unavailable)", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := []map[string]interface{}{
				{
					"assessedValue":       150000, // has improvement
					"assessmentLandValue": 0,      // no land value
					"yearBuilt":           0,      // unknown year
					"lotSize":             1.0,    // known acreage
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		// Create a custom client that points to the test server
		rt := &testRoundTripper{server: server}
		client := &http.Client{Transport: rt}
		rp := NewRentcastProvider(client, "test-key")

		pd, err := rp.Lookup(context.Background(), "456 Oak Ave")
		if err != nil {
			t.Fatalf("Lookup failed: %v", err)
		}

		// Verify correct availability markers
		if !pd.Available["assessed_improvement"] {
			t.Error("assessed_improvement should be available (> 0)")
		}
		if pd.Available["assessed_land"] {
			t.Error("assessed_land should not be available (= 0)")
		}
		if pd.Available["year_built"] {
			t.Error("year_built should not be available (= 0)")
		}
		if !pd.Available["acreage"] {
			t.Error("acreage should be available (> 0)")
		}

		// StructurePresent should be false because yearBuilt is 0
		if pd.StructurePresent() {
			t.Error("StructurePresent() = true, want false (yearBuilt is unknown)")
		}
	})

	t.Run("empty result", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]map[string]interface{}{})
		}))
		defer server.Close()

		rt := &testRoundTripper{server: server}
		client := &http.Client{Transport: rt}
		rp := NewRentcastProvider(client, "test-key")

		_, err := rp.Lookup(context.Background(), "nonexistent")
		if err == nil {
			t.Error("expected error for empty result, got nil")
		}
		if !strings.Contains(err.Error(), "no results") {
			t.Errorf("error message should mention no results, got: %v", err)
		}
	})

	t.Run("non-200 response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("invalid api key"))
		}))
		defer server.Close()

		rt := &testRoundTripper{server: server}
		client := &http.Client{Transport: rt}
		rp := NewRentcastProvider(client, "bad-key")

		_, err := rp.Lookup(context.Background(), "any address")
		if err == nil {
			t.Error("expected error for 401, got nil")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error should mention status code 401, got: %v", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("not valid json"))
		}))
		defer server.Close()

		rt := &testRoundTripper{server: server}
		client := &http.Client{Transport: rt}
		rp := NewRentcastProvider(client, "test-key")

		_, err := rp.Lookup(context.Background(), "any address")
		if err == nil {
			t.Error("expected error for malformed JSON, got nil")
		}
		if !strings.Contains(err.Error(), "parse response") {
			t.Errorf("error should mention parse failure, got: %v", err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Simulate slow server
			<-r.Context().Done()
		}))
		defer server.Close()

		rt := &testRoundTripper{server: server}
		client := &http.Client{Transport: rt}
		rp := NewRentcastProvider(client, "test-key")

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := rp.Lookup(ctx, "any address")
		if err == nil {
			t.Error("expected error for cancelled context, got nil")
		}
	})
}

// testRoundTripper redirects requests to a test server.
// This allows us to stub the Rentcast API without hardcoding a URL.
type testRoundTripper struct {
	server *httptest.Server
}

func (rt *testRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Replace the host with the test server host
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(rt.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(r)
}

func TestNewRentcastProvider(t *testing.T) {
	client := &http.Client{}
	key := "my-api-key"
	rp := NewRentcastProvider(client, key)

	if rp.client != client {
		t.Error("client not set correctly")
	}
	if rp.apiKey != key {
		t.Error("apiKey not set correctly")
	}
}
