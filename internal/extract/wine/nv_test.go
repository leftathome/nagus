package wine

import (
	"context"
	"errors"
	"testing"

	"github.com/leftathome/nagus/internal/listing"
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
		"MUZ Vermut 1L NV", // mysa; the Spanish spelling
	} {
		it, err := New().Extract(context.Background(), sanitized(title, ""))
		if err != nil || it.Attributes["nv"] != "true" {
			t.Errorf("%q: nv=%q err=%v", title, it.Attributes["nv"], err)
		}
	}
}

// A fortified-wine style is wine evidence with no NV and no product type
// (operator ruling 2026-09-25, "Port is wine"). A year beside it is still
// the vintage unless a skip word says otherwise.
func TestExtract_FortifiedIsWine(t *testing.T) {
	for title, vintage := range map[string]string{
		"Bottled in 2020 Tawny Port":       "", // wine, but 2020 is a bottling year
		"Taylor's 20 Year Old Tawny Port":  "",
		"Graham's 2017 Vintage Port":       "2017",
		"Fonseca Bin 27 Port":              "",
		"Tio Pepe Fino Sherry":             "",
		"Blandy's 10 Year Malmsey Madeira": "",
		"Florio Marsala Superiore":         "",
		"Warre's LBV 2016":                 "2016",
		"Quinta do Noval Colheita":         "",
		"Lustau Manzanilla Papirusa":       "",
		"Gonzalez Byass Oloroso":           "",
		"Pedro Ximenez Dessert Sherry":     "",
		"Late Bottled Vintage Porto Style": "",
		"Hidalgo Amontillado Napoleon":     "",
		"Ruby Port Reserve":                "",
		"Harveys Bristol Cream Sherry":     "",
		"Taylor Fladgate LBV":              "",
	} {
		it, err := New().Extract(context.Background(), sanitized(title, ""))
		if err != nil {
			t.Errorf("%q: %v, want wine", title, err)
			continue
		}
		if it.Attributes["vintage"] != vintage {
			t.Errorf("%q: vintage = %q, want %q", title, it.Attributes["vintage"], vintage)
		}
	}
}

// "port" the English word, "ruby"/"tawny" the colours, and fortified-wine
// merchandise are not wine.
func TestExtract_PortAsEnglishWordIsNotWine(t *testing.T) {
	for _, title := range []string{
		// Merchandise: glass and decanter are on the strong list, so no pack
		// count, year or varietal rescues them; sipper, leather and carrier
		// are on the packaging list.
		"Port Glass Set of 2",
		"6 Pack Port Glasses",
		"Sherry Glass",
		"Port Sipper",
		"Port Decanter",
		"Tawny Leather Wine Carrier",
		"Tawny Leather Tote",
		"Port Travel Carrier",
		"Port Leather Case",
		// Places: whole words, and "Port <place>".
		"Newport Wine Tote",
		"Portland Oregon Wine Tour Gift Card",
		"Portsmouth Tasting Room Pass",
		"Port Townsend Tasting Room Pass",
		"Port Angeles Tasting Room Pass",
		// A connector.
		"USB Port Adapter",
		// Colours and gemstones.
		"Ruby Red Grapefruit Soda",
		"Tawny Owl Print",
	} {
		if _, err := New().Extract(context.Background(), sanitized(title, "")); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: err = %v, want ErrNotWine", title, err)
		}
	}
}

// A source's declared wine_type is wine evidence (a colour), so it overrides
// the no-evidence rejection -- intentional, and the production behaviour --
// but never the merchandise lists, which run first on the title alone.
func TestExtract_DeclaredWineTypeIsEvidence(t *testing.T) {
	withType := func(title, wineType string) listing.Sanitized {
		s := sanitized(title, "")
		s.Aspects = map[string]string{"wine_type": wineType}
		return s
	}
	if _, err := New().Extract(context.Background(), withType("Tawny Owl Reserve", "Red")); err != nil {
		t.Errorf("declared wine_type Red with no title evidence: %v, want wine", err)
	}
	for _, title := range []string{"Tawny Leather Wine Carrier", "Port Glass Set of 2", "Port Sipper"} {
		if _, err := New().Extract(context.Background(), withType(title, "Red")); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q with wine_type Red: err = %v, want ErrNotWine (merchandise wins)", title, err)
		}
	}
}

// A fortified-wine word on a cask-finished spirit or beer is not wine
// evidence (nagus !28 port review F-1); a hyphen-joined token is not the
// word (F-3). These are not culinary.
func TestExtract_CaskFinishedSpiritsAreNotWine(t *testing.T) {
	for _, title := range []string{
		"Port Cask Finish Single Malt Scotch",
		"Sherry Cask Bourbon",
		"Glenmorangie Quinta Ruban Port Cask",
		"Balvenie DoubleWood 12 Sherry Cask",
		"Angel's Envy Port Barrel Finished Bourbon",
		"Madeira Cask Finish Rum",
		"Oloroso Sherry Cask Whisky",
		"Sherry Oak 12 Year Macallan",
		"Port Dundas Grain Whisky",
		"Port Barrel Aged Stout",
		"Sherry Barrel Imperial Porter",
		// A cask word alone.
		"Tawny Port Wood Finish",
		// F-3: hyphen-joined.
		"Port-a-Potty",
		"port-a-john rental",
	} {
		_, err := New().Extract(context.Background(), sanitized(title, ""))
		if !errors.Is(err, ErrNotWine) || errors.Is(err, ErrCulinary) {
			t.Errorf("%q: err = %v, want ErrNotWine and not ErrCulinary", title, err)
		}
	}
	// The guards withdraw only the fortified cue: other evidence stands.
	for _, title := range []string{
		"Porter Creek Vineyards Pinot Noir 2021",
		"Port",
		"PX",
	} {
		if _, err := New().Extract(context.Background(), sanitized(title, "")); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

// Culinary products are not wine and not merchandise: they fail with
// ErrCulinary (reserved for a future grocery category; nagus !28 port
// review F-2), which is still ErrNotWine but never ErrMerchandise.
func TestExtract_CulinaryIsItsOwnReason(t *testing.T) {
	for _, title := range []string{
		// A fortified word on food.
		"Sherry Vinegar",
		"Cooking Sherry",
		"Marsala Cooking Wine",
		"Marsala Chicken Sauce",
		"Madeira Cake",
		"Madeira Wine Cake",
		"Sherry Trifle Mix",
		"Port Wine Cheese",
		"Port Wine Jelly",
		"Port Fig Jam",
		"Port Fudge",
		"Sherry Chocolates",
		// A food noun AFTER a colour, varietal or year is the product
		// (culinaryTitle), so they do not rescue it ("Camino Red Wine
		// Vinegar" is live on broc-cellars).
		"Camino Red Wine Vinegar",
		"Zinfandel Cooking Wine",
		"Chardonnay Cake",
		"Cabernet Sauvignon Cheese",
		"Merlot Wine Jelly",
		"2025 Fox Hill Olive Oil",
		"2024 Turley Estate Olive Oil",
	} {
		_, err := New().Extract(context.Background(), sanitized(title, ""))
		if !errors.Is(err, ErrCulinary) || !errors.Is(err, ErrNotWine) || errors.Is(err, ErrMerchandise) {
			t.Errorf("%q: err = %v, want ErrCulinary (and ErrNotWine, not ErrMerchandise)", title, err)
		}
	}
	// Merchandise keeps its own reason.
	for _, title := range []string{"Newport Wine Tote", "Port Glass Set of 2", "Port Sipper", "Champagne Flute"} {
		_, err := New().Extract(context.Background(), sanitized(title, ""))
		if !errors.Is(err, ErrMerchandise) || errors.Is(err, ErrCulinary) {
			t.Errorf("%q: err = %v, want ErrMerchandise, not ErrCulinary", title, err)
		}
	}
	// Words real wine names use are not culinary on their own.
	for _, title := range []string{"JaM Cellars Butter Chardonnay 2022", "The Chocolate Block 2021", "6PK Gift Box Wood MRW, 2021 Mix"} {
		if _, err := New().Extract(context.Background(), sanitized(title, "")); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}
