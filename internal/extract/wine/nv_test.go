package wine

import (
	"context"
	"errors"
	"testing"
)

// "NV" is wine evidence only beside another wine cue (nagus !28 re-review
// N-a): each of these reached the Telegram deal path as a "wine" before.
func TestExtract_NVAloneIsNotWine(t *testing.T) {
	for _, title := range []string{
		"NV Key Chain",
		"Foil Cutter NV",
		"Wine Stopper - N.V.",
		"Gift Box NV",
		"Olive Oil NV",
		"Heineken N.V. Beer",
		"Pickup Fee Reno, NV",
	} {
		if _, err := New().Extract(context.Background(), sanitized(title, "")); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q extracted as wine (err %v)", title, err)
		}
	}
}

// ... and it IS the evidence for a house-style blend whose title carries no
// year, varietal or colour -- pinned so removing NV as evidence fails here.
func TestExtract_NVWithAWineCueIsWine(t *testing.T) {
	for _, title := range []string{"Bollinger Special Cuvee Brut NV", "Krug Grande Cuvee N.V.", "Cremant de Loire NV"} {
		it, err := New().Extract(context.Background(), sanitized(title, ""))
		if err != nil {
			t.Errorf("%q: %v", title, err)
			continue
		}
		if it.Attributes["nv"] != "true" || it.Attributes["vintage"] != "" {
			t.Errorf("%q: nv=%q vintage=%q", title, it.Attributes["nv"], it.Attributes["vintage"])
		}
	}
	// The same titles without the marker have no evidence at all.
	if _, err := New().Extract(context.Background(), sanitized("Krug Grande Cuvee", "")); !errors.Is(err, ErrNotWine) {
		t.Errorf("without NV, 'Krug Grande Cuvee' must have no wine evidence (err %v)", err)
	}
}

// A year right after disgorged / bottled / Est. / since / anniversary is not
// the vintage (N-b).
func TestExtractVintage_SkipsNonVintageYears(t *testing.T) {
	for title, want := range map[string]int{
		"Bollinger Special Cuvee Brut NV, disgorged 2019": 0,
		"Old Winery Est. 1970 Cabernet Sauvignon":         0,
		"Opus One 2019 (bottled 2021)":                    2019,
		"Bottled 2021: Opus One 2019":                     2019,
		"Dom Perignon 2012, degorgement 2021":             2012,
		"Chateau Montelena since 1882 Chardonnay 2021":    2021,
		"50th Anniversary 1966 Commemorative Cabernet":    0,
		"Grande Annee 2014":                               2014,
	} {
		got, ok := extractVintage(title)
		if (want == 0 && ok) || got != want {
			t.Errorf("extractVintage(%q) = %d, %v; want %d", title, got, ok, want)
		}
	}
}
