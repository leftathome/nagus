package wine

import "testing"

// explicitNV on its own, so the rule is pinned apart from the merchandise
// list that also stops most of the titles in TestExtract_NVAloneIsNotWine.
func TestExplicitNV(t *testing.T) {
	for _, tc := range []struct {
		title    string
		otherCue bool
		want     bool
	}{
		{"Bollinger Special Cuvee Brut NV", false, true},
		{"Krug Grande Cuvee N.V.", false, true},
		{"Something Red NV", true, true},          // a colour the extractor found
		{"Mystery Box NV", false, false},          // NV with no wine cue
		{"Heineken N.V. Beer", false, false},      // a company suffix
		{"Heineken N.V. Brewing Co", true, false}, // even beside another cue
		{"Pickup Fee Reno, NV", false, false},     // a state after a city
		{"Brut Pickup Reno, NV", false, false},    // a state, whatever else
		{"Nevada Brut", false, false},             // NV must be a token
		{"Reno NV Pickup Brut", false, false},     // a Nevada city, no comma
		{"Las Vegas NV Brut", false, false},
	} {
		if got := explicitNV(tc.title, tc.otherCue); got != tc.want {
			t.Errorf("explicitNV(%q, %v) = %v, want %v", tc.title, tc.otherCue, got, tc.want)
		}
	}
}
