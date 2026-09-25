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
		// Re-review F3: NV beside a sparkling cue, on merchandise.
		"Brut Force Tool NV",
		"Blanc de Blancs Tea Towel NV",
		"Brut Magnum Display Box NV",
		"Reno NV Pickup Brut",
		"Champagne Flute",
		"Champagne Bucket",
		"Prosecco Ice Bucket",
		"Champagne Saber",
		"Brut Bottle Charm",
		"Sparkling Wine Chiller Sleeve",
		"Rose Soap",
		// Re-review F5: real titles from the live corpus.
		"2025 Fox Hill Olive Oil",
		"2024 Turley Estate Olive Oil",
		// Each keyword alone against another wine cue, so removing it fails.
		"Holiday Gift Box Brut NV",
		"Champagne Key Chain",
		"Champagne Foil Cutter",
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
		"50th Anniversary 2019 Cabernet":                  2019, // "<ordinal> anniversary" is not a skip
		"Anniversary 1966 Commemorative Cabernet":         0,
		"Founded 1972 Estate Cabernet Sauvignon":          0,
		"Bottled in 2020 Tawny Port":                      0,
		"Disgorged on 2019 Brut":                          0,
		"Grande Annee 2014":                               2014,
	} {
		got, ok := extractVintage(title)
		if (want == 0 && ok) || got != want {
			t.Errorf("extractVintage(%q) = %d, %v; want %d", title, got, ok, want)
		}
	}
}

// Real wine sold in packaging is still wine (re-review F1): the live
// commerce7:mark-ryan listing, and a pack of a varietal.
func TestExtract_PackagedWineIsWine(t *testing.T) {
	for _, title := range []string{
		"6PK Gift Box Wood MRW, 2021 Mix",
		"3 Pack Gift Box Pinot Noir",
		"Cabernet Sauvignon Gift Box",
		"6PK Gift Box Brut NV", // the pack count alone keeps it wine
	} {
		if _, err := New().Extract(context.Background(), sanitized(title, "")); err != nil {
			t.Errorf("%q: %v", title, err)
		}
	}
}

// A disgorgement date is wine evidence (only sparkling wine is disgorged)
// but never the vintage (re-review F2) -- with no product type and no body,
// as a Shopify listing arrives.
func TestExtract_DisgorgedIsWineEvidenceNotAVintage(t *testing.T) {
	it, err := New().Extract(context.Background(), sanitized("Bollinger Special Cuvee (disgorged 2019)", ""))
	if err != nil {
		t.Fatalf("a disgorged listing must be wine: %v", err)
	}
	if it.Attributes["vintage"] != "" {
		t.Errorf("vintage = %q: a disgorgement year is not the vintage", it.Attributes["vintage"])
	}
	// "cuvee", "bottled", "est." and "since" are NOT evidence on their own.
	for _, title := range []string{"Special Cuvee", "Bottled Fresh", "Est. Winery Club", "Since Forever"} {
		if _, err := New().Extract(context.Background(), sanitized(title, "")); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q counted as wine evidence (err %v)", title, err)
		}
	}
}

// NV beside a fortified, aromatised or petillant cue is wine (re-review F6),
// with no product type.
func TestExtract_NVWithFortifiedOrPetillantCues(t *testing.T) {
	for _, title := range []string{
		"Graham's Six Grapes Reserve Port NV",
		"Mondavi Moscato d'Oro NV",
		"Tawny NV",
		"Ruby Reserve NV",
		"Pet Nat NV",
		"Pet-Nat Sparkler NV",
		"Petillant Naturel NV",
		"Sweet Vermouth NV",
	} {
		it, err := New().Extract(context.Background(), sanitized(title, ""))
		if err != nil || it.Attributes["nv"] != "true" {
			t.Errorf("%q: nv=%q err=%v", title, it.Attributes["nv"], err)
		}
	}
}
