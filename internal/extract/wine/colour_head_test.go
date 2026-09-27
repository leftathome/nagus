package wine

import (
	"context"
	"testing"
)

// nagus-dwq: colour and style words are wine evidence only where they name
// the wine -- the title's head, or right before another wine cue -- not
// merchandise in a colour. Each table pins one rule; want nil means wine.
// The merchandise nouns here ("lanyard", "beanie") are on no list on
// purpose: the rules must reject them by position alone.

type verdictCase = struct {
	title, wineType string
	want            error
}

// A title colour word followed by some other word is that word's colour.
func TestColourHead_MidTitleColourIsNotEvidence(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Red Wine Journal", "", ErrNotWine},
		{"Chateau Napa Valley Sparkling Lanyard", "", ErrNotWine},
		{"Rose Gold Lanyard", "", ErrNotWine},
		{"Red Wine Journal", "Red", nil}, // a declared colour is evidence
		{"Witches Brew Red Wine", "", nil},
		{"Honey Badger Red Blend", "", nil},
		{"Rose", "", nil},
		{"Red Wine", "", nil},
	})
}

// A description colour still vouches for a title whose colour words fail.
func TestColourHead_BodyColourStillCounts(t *testing.T) {
	s := sanitized("Chateau Napa Valley Lanyard Rose", "A dry rose from the estate.")
	if _, err := New().Extract(context.Background(), s); err != nil {
		t.Fatalf("body colour: %v, want wine", err)
	}
}

// A run of plain-English colour words ending the title is the wine's only
// when nothing but style words stands between it and any place name.
func TestColourHead_EnglishRunAfterPlaceAndObject(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Napa Valley Lanyard Rose", "", ErrNotWine},
		{"Chateau Napa Valley Beanie Sparkling", "", ErrNotWine},
		{"La Vie en Rose", "", nil},
		{"Chateau d'Esclans Whispering Angel Rose", "", nil},
		{"Avia Sweet Rose", "", nil},
		{"Columbia Valley Rose", "", nil},
		{"Pinot Noir Rose", "", nil},
	})
}

// A title whose head is a wine-only style is a wine: its colour words name
// it wherever they stand.
func TestColourHead_WineOnlyHeadVouches(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Sparkling Water Cellars Brut", "", nil},
		{"Sparkling Lanyard", "", ErrNotWine},
	})
}

// A colour right before another wine cue names that wine.
func TestColourHead_ColourBeforeWineCue(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Rose Wine Napa Valley Selection", "", nil},
		{"Casa Nuestra Red Wine Napa Valley - ARCHIVE", "", nil},
		{"Chateau Diana Sparkling Moscato", "", nil},
		{"Gruet Sparkling Frizzante", "", nil},
	})
}

// A "by <producer>" credit is not the head.
func TestColourHead_ProducerCredit(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Butter Red Blend by JaM", "", nil},
		{"Sparkling Rose by Gruet", "", nil},
	})
}

// A can, keg, bottle or collection of wine is still the wine.
func TestColourHead_ContainersAreNotTheHead(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Love Rose Can Pack", "", nil},
		{"Love Sparkling Cans", "", nil},
		{"Analemma White Blend - 5.16G Keg", "", nil},
		{"Napa Valley Red Collection", "", nil},
		{"Columbia Valley Rose 3 Bottles", "", nil},
	})
}

// With an estate word, a trailing run of plain-English style words is the
// head only right after the place name; a wine-only word is the head
// anywhere. (The colour is out of colourBeside's reach in these titles.)
func TestColourHead_EstateStyleRun(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Chateau Napa Valley Lanyard Strap Red", "", ErrNotWine},
		{"Chateau Napa Valley Lanyard Strap Reserve", "", ErrNotWine},
		{"Domaine Carneros Napa Valley Le Reve Brut", "", nil},
		{"Chateau Napa Valley Cuvee Alexandre Rouge", "", nil},
		{"Chateau Montelena Napa Valley Estate", "", nil},
		{"Clos du Val Napa Valley Estate Reserve", "", nil},
		{"Quinta do Crasto Douro Superior", "", nil},
		{"Chateau Napa Valley Estate Lanyard", "", ErrNotWine},
	})
}

// Beside a broad name, an English colour must be the head: followed by
// another word it is that word's colour; after a one-word gap it must not
// end the title unless the gap is a style word.
func TestColourHead_ColourBesideBroadName(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Columbia Valley Red Wine Lanyard", "", ErrNotWine},
		{"Napa Valley White Lanyard", "", ErrNotWine},
		{"Napa Valley Lanyard Red", "", ErrNotWine},
		{"Napa Valley Beanie White 2 Pack", "", ErrNotWine},
		{"Red Mountain Pioneer Red IV", "", nil},
		{"Napa Valley Red Wine Edition II", "", nil},
		{"Napa Valley Proprietary Red", "", nil},
		{"Napa Valley Red", "", nil},
		{"Napa Valley Red 1.5L", "", nil},
		{"Hotel California Napa Valley Red", "", nil},
		// The wine-only colours keep the old rule.
		{"Languedoc Belleruche Rouge", "", nil},
		{"Bourgogne Rouge Belleruche", "", nil},
	})
}

// A colour word that fails with a food after it is culinary.
func TestColourHead_FailedColourBeforeFood(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Red Wine Salami", "", ErrCulinary},
		{"Rose Wine Gelato", "", ErrCulinary},
		{"Camino Red Wine Vinegar", "", ErrCulinary},
		{"Red Wine Lanyard", "", ErrNotWine},
	})
}

// A lodging word followed by a label word names a label, not a venue.
func TestColourHead_LodgingLabel(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Chianti Classico Riserva Wedding Edition", "", nil},
		{"Barolo Hotel Series", "", nil},
		{"Rioja Wedding Venue", "", ErrNotWine},
		{"Chianti Inn Stay", "", ErrNotWine},
		{"Napa Valley Chateau Wedding Venue", "", ErrNotWine},
	})
}

// A tumbler, sweatshirt or umbrella is merchandise whatever year it carries.
func TestColourHead_VintageObjectNouns(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Chateau Napa Valley Tumbler 2019", "", ErrMerchandise},
		{"Chateau Napa Valley Sweatshirt 2020", "", ErrMerchandise},
		{"Winery Umbrella 2019", "", ErrMerchandise},
		{"Tumbler Ridge Red 2019", "", nil},
	})
}

// The bead's own examples (nagus-dwq).
func TestColourHead_BeadExamples(t *testing.T) {
	checkVerdicts(t, []verdictCase{
		{"Chateau Ste Michelle Columbia Valley Red Wine Tumbler", "", ErrMerchandise},
		{"Chateau Napa Valley Sweatshirt Red", "", ErrNotWine},
		{"Chateau Napa Valley Tumbler Rose", "", ErrNotWine},
		{"Chateau Napa Valley Umbrella White", "", ErrNotWine},
		{"Chateau Napa Valley Tumbler 2019", "", ErrMerchandise},
	})
}

// The head-aware rules gate evidence only: a wine with other evidence keeps
// the colour its title names.
func TestColourHead_ColourAttributeUnchanged(t *testing.T) {
	for title, want := range map[string]string{
		"2021 Artist Series Red Wine - Sun":        "red",
		"2025 Sparkling Erbaluce":                  "sparkling",
		"2022 The Reserve Red Blend, To Kalon":     "red",
		"2025 Patelin de Tablas Rose 3L Box":       "rose",
		"Chateau Napa Valley Tumbler 2019 Rose NV": "rose",
	} {
		it, err := New().Extract(context.Background(), sanitized(title, ""))
		if err != nil {
			t.Errorf("%q: %v", title, err)
			continue
		}
		if got := it.Attributes["colour"]; got != want {
			t.Errorf("%q: colour %q, want %q", title, got, want)
		}
	}
}
