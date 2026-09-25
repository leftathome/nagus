package offer

import "testing"

func TestComparisonKey(t *testing.T) {
	for _, tc := range []struct {
		name, mode, vintage string
		nv                  bool
		wantKey, wantStatus string
	}{
		{"non-vintage ignores a title year", "non_vintage", "2019", false, "p", VintageStatusNonVintage},
		{"non-vintage without a year", "non_vintage", "", true, "p", VintageStatusNonVintage},
		{"vintage with a year", "vintage", "2014", false, "p@2014", VintageStatusVintage},
		{"vintage without a year", "vintage", "", false, "", VintageStatusUnknown},
		{"vintage: NV does not override quark", "vintage", "", true, "", VintageStatusUnknown},
		{"unknown with a year", "unknown", "2019", false, "p@2019", VintageStatusVintage},
		{"unknown without a year", "unknown", "", false, "", VintageStatusUnknown},
		{"unknown with an explicit NV", "unknown", "", true, "p", VintageStatusNonVintage},
		{"no mode: a non-wine product", "", "", false, "p", ""},
	} {
		key, status := ComparisonKey("p", tc.mode, tc.vintage, tc.nv)
		if key != tc.wantKey || status != tc.wantStatus {
			t.Errorf("%s: (%q, %q), want (%q, %q)", tc.name, key, status, tc.wantKey, tc.wantStatus)
		}
	}
	if k, s := ComparisonKey("", "vintage", "2019", false); k != "" || s != "" {
		t.Errorf("no product id: (%q, %q)", k, s)
	}
}
