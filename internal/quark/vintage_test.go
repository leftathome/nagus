package quark

import (
	"encoding/json"
	"testing"
)

// quark returns a wine product's vintage_mode as a catalog fact inside its
// untrusted-data envelope (quark QUARK-04). Only a catalog-tier fact with a
// value in the closed set counts.
func TestResultVintageMode(t *testing.T) {
	decode := func(raw string) Result {
		t.Helper()
		var r Result
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	spec := func(key, value, tier string) string {
		return `{"key":{"untrusted":true,"value":"` + key + `"},"value":{"untrusted":true,"value":"` + value +
			`"},"tier":"` + tier + `","observed_at":"2026-09-25T00:00:00Z"}`
	}
	for name, tc := range map[string]struct{ specs, want string }{
		"non-vintage":         {spec("region", "Champagne", "catalog") + "," + spec("vintage_mode", "non_vintage", "catalog"), "non_vintage"},
		"vintage":             {spec("vintage_mode", "vintage", "catalog"), "vintage"},
		"unknown":             {spec("vintage_mode", "unknown", "catalog"), "unknown"},
		"none stated":         {spec("region", "Napa Valley", "catalog"), ""},
		"not catalog tier":    {spec("vintage_mode", "non_vintage", "asserted"), ""},
		"outside the enum":    {spec("vintage_mode", "sometimes", "catalog"), ""},
		"no specs (non-wine)": {"", ""},
	} {
		r := decode(`{"product_id":"p","route":"fuzzy","confidence":100,"specs":[` + tc.specs + `]}`)
		if got := r.VintageMode(); got != tc.want {
			t.Errorf("%s: VintageMode() = %q, want %q", name, got, tc.want)
		}
	}
}
