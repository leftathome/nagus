package wine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// extractErr runs one title through the extractor.
func extractErr(title string) error {
	_, err := New().Extract(context.Background(), sanitized(title, ""))
	return err
}

// nagus-tmr: Old World wines named by place, with no year, grape or colour
// word, were all dropped as not-wine at 61beb01.
func TestExtract_AppellationIsWineEvidence(t *testing.T) {
	for _, title := range []string{
		"Castello di Ama Chianti Classico",
		"Barolo Vietti Castiglione",
		"Rioja Reserva Muga",
		"Chateauneuf-du-Pape Beaucastel",
		"Sancerre Vacheron",
		"Chablis Premier Cru",
		"Meursault Coche-Dury",
		"Pomerol Petrus",
		"Etna Rosso Benanti",
		"Franciacorta Brut",
		"Gaja Barbaresco",
		"Soave",
		"Vouvray",
		"Beaujolais",
		"Otello Ceci Lambrusco", // live corpus (internetwines.com)
		"Menard-Gaborit Muscadet Sevre et Maine 2-Pack",
		// Accent-folded, case-free, whole words; "St." is "Saint".
		"Ch\u00e2teauneuf du Pape",
		"CHATEAUNEUF DU PAPE",
		"St. Emilion Grand Cru",
	} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

// The hand-kept supplement: names LWIN files only under a wider or longer
// name, each pinned so removing it fails.
func TestExtract_AppellationSupplement(t *testing.T) {
	for _, title := range []string{
		"Benanti Etna", "Brunello Riserva", "Amarone", "Ripasso", "Recioto", "Vino Nobile", "Morellino",
		"Chateauneuf", "Sassicaia", "Lambrusco", "Tokaji", "Cremant", "Txakoli", "Txakolina",
		"Priorato", "Sudtirol", "Sud Tirol", "Asti Spumante", "Muscadet", "Brachetto d'Acqui",
		"Carmignano", "Ghemme", "Frascati",
		// Supplement broad names, with the cue they need.
		"Montalcino Rosso", "Napa Red", "Sonoma Rouge",
	} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
	for _, title := range []string{"Montalcino", "Napa", "Sonoma", "Brunello"} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine (broad alone)", title, err)
		}
	}
	// LWIN names that the task names, which the generated table must hold.
	for _, name := range []string{"vino nobile di montepulciano", "cotes du rhone", "beaujolais villages", "chianti classico"} {
		if known, broad := appellationTier(name); !known || broad {
			t.Errorf("%q: known=%v broad=%v, want an appellation", name, known, broad)
		}
	}
}

// Broad names are evidence only beside a classification or a bare colour.
func TestExtract_BroadRegionNeedsASupportingCue(t *testing.T) {
	for _, title := range []string{"Burgundy", "Tuscany", "Bordeaux", "Napa Valley", "Piedmont", "Rhone", "Loire"} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine", title, err)
		}
	}
	for _, tc := range []struct{ title, colour string }{
		{"Toscana IGT", ""},
		{"Bordeaux AOC", ""},
		{"Burgundy Grand Cru", ""},
		{"Rhone Reserva", ""}, // any classification word
		{"Bourgogne Rouge", "red"},
		{"Napa Valley Red", "red"},
		{"Napa Valley Red Collection", "red"}, // live corpus (Mondavi)
		{"Red Mountain Pioneer Red IV", "red"},
		{"Bordeaux Blanc", "white"},
	} {
		it, err := New().Extract(context.Background(), sanitized(tc.title, ""))
		if err != nil {
			t.Errorf("%q: %v, want wine", tc.title, err)
			continue
		}
		if it.Attributes["colour"] != tc.colour {
			t.Errorf("%q: colour %q, want %q", tc.title, it.Attributes["colour"], tc.colour)
		}
	}
	for _, title := range []string{
		// A colour inside the broad name is the place, not support.
		"Estate Red Mountain Big Kiona",
		// "Burgundy Red" and "Bordeaux White" are colour names.
		"Burgundy Red",
		"Bordeaux White",
		// Lower-case "do" is a word, not the Spanish DO.
		"What to do in Napa Valley",
	} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine", title, err)
		}
	}
}

// Merchandise, events and spirits with a place in the title stay out.
func TestExtract_AppellationGuards(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  error
	}{
		{"Tuscany Candle", ErrMerchandise},
		{"Bordeaux Wine Glass", ErrMerchandise},
		{"Burgundy Leather Wallet", ErrMerchandise},
		{"Napa Valley Tote", ErrMerchandise},
		{"Chianti Cooking Sauce", ErrCulinary},
		{"Barolo Sauce", ErrCulinary},
		{"Barolo Tasting Dinner", ErrNotWine},
		{"Chablis Harvest Tour", ErrNotWine},
		{"Bordeaux Map", ErrNotWine},
		{"Napa Valley Red Tour", ErrNotWine},
		{"Sauternes Cask Finish", ErrNotWine},
		{"Glenmorangie Sauternes Finish", ErrNotWine},
		{"Sauternes Barrel Aged", ErrNotWine},
		{"Rioja Reserva Rum", ErrNotWine},
		{"Barolo Oak", ErrNotWine},
	} {
		err := extractErr(tc.title)
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: %v, want %v", tc.title, err, tc.want)
		}
		if tc.want == ErrNotWine && (errors.Is(err, ErrCulinary) || errors.Is(err, ErrMerchandise)) {
			t.Errorf("%q: %v, want a bare ErrNotWine", tc.title, err)
		}
	}
	// Oak before an appellation is not a cask ("Oak Aged Rioja").
	if err := extractErr("Oak Aged Rioja"); err != nil {
		t.Errorf("Oak Aged Rioja: %v, want wine", err)
	}
	// A spirit word in a producer name before the place is the producer.
	if err := extractErr("Gin Lane Barolo"); err != nil {
		t.Errorf("Gin Lane Barolo: %v, want wine", err)
	}
}

// A bare colour word is the colour only beside an appellation or an NV
// marker, and the last one outside a place name wins.
func TestExtract_BareColour(t *testing.T) {
	for _, tc := range []struct{ title, colour, nv string }{
		{"Corvee de Trousseau Arbois Red NV", "red", "true"}, // live corpus (mysa.wine)
		{"Melon a Queue Rouge Arbois Pupillin White 2022", "white", ""},
		{"Etna Rosso Benanti", "red", ""},
		{"Kekfrankos Rheinhessen Red 2020", "red", ""},
		{"Coup de Foug Blanc IGP Vin des Allobroges 2024", "white", ""},
		{"Bianco Abruzzo White 2023", "white", ""},

		{"Cuvee Rouge Garance Blanc NV", "white", "true"}, // the last colour
		// A colour in the appellation's own name beats one outside it.
		{"Etna Bianco Red Label", "white", ""},
		// An appellation is an NV cue on its own.
		{"Chablis NV", "", "true"},
	} {
		it, err := New().Extract(context.Background(), sanitized(tc.title, ""))
		if err != nil {
			t.Errorf("%q: %v", tc.title, err)
			continue
		}
		if it.Attributes["colour"] != tc.colour || it.Attributes["nv"] != tc.nv {
			t.Errorf("%q: colour=%q nv=%q, want %q %q", tc.title, it.Attributes["colour"], it.Attributes["nv"], tc.colour, tc.nv)
		}
	}
	for _, title := range []string{"Red", "Rosso", "Rouge", "Tinto", "Blanco", "Red Door Series", "Red, Reno NV"} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine", title, err)
		}
	}
	// A year alone never makes a bare colour the colour.
	it, err := New().Extract(context.Background(), sanitized("2019 Red Door", ""))
	if err != nil {
		t.Fatal(err)
	}
	if it.Attributes["colour"] != "" {
		t.Errorf("2019 Red Door: colour %q, want none", it.Attributes["colour"])
	}
}

// The generated table is normalized as the extractor normalizes titles,
// excludes the ordinary words and handled names, and tiers the broad
// regions.
func TestAppellationTableIsNormalized(t *testing.T) {
	if len(lwinAppellations) < 700 {
		t.Fatalf("lwinAppellations has %d names; regenerate it", len(lwinAppellations))
	}
	for _, m := range []map[string]bool{lwinAppellations, appellationSupplement} {
		for name := range m {
			if got := strings.Join(normalizeWords(name), " "); got != name {
				t.Errorf("%q normalizes to %q", name, got)
			}
		}
	}
	for _, name := range []string{"california", "washington", "oregon", "new york", "port", "porto", "champagne", "orange", "england"} {
		if known, _ := appellationTier(name); known {
			t.Errorf("%q is in the table; it is excluded", name)
		}
	}
	for _, name := range []string{"tuscany", "burgundy", "bordeaux", "piedmont", "rhone", "loire", "napa valley", "sonoma county", "willamette valley"} {
		if known, broad := appellationTier(name); !known || !broad {
			t.Errorf("%q: known=%v broad=%v, want broad", name, known, broad)
		}
	}
}

func TestFindAppellations_LongestMatch(t *testing.T) {
	got := findAppellations(normalizeWords("Chianti Classico Riserva"))
	if len(got) != 1 || got[0].start != 0 || got[0].end != 2 {
		t.Fatalf("got %+v, want chianti classico at [0,2)", got)
	}
	// Whole words only.
	if got := findAppellations(normalizeWords("Torontal Somontanos")); len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

// Classification tokens support a broad name but are never evidence alone.
func TestClassification(t *testing.T) {
	for _, title := range []string{"Toscana IGT", "Rioja DOCa", "Bordeaux AOC", "Rhone AOP", "Veneto D.O.C.", "Chablis 1er Cru", "Gran Reserva", "VdP"} {
		if !hasClassification(title) {
			t.Errorf("%q: no classification found", title)
		}
	}
	for _, title := range []string{"What to do", "doc martens", "Reserve Collection"} {
		if hasClassification(title) {
			t.Errorf("%q: classification found", title)
		}
	}
	for _, title := range []string{"Gran Reserva", "Premier Cru", "DOCG", "Riserva"} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine (supporting cue only)", title, err)
		}
	}
}

func TestNormalizeWords(t *testing.T) {
	for in, want := range map[string][]string{
		"Ch\u00e2teauneuf-du-Pape":   {"chateauneuf", "du", "pape"},
		"St. Emilion":                {"saint", "emilion"},
		"Mt. Veeder":                 {"mount", "veeder"},
		"Fig + Angelica & Jam":       {"fig", "and", "angelica", "and", "jam"},
		"Barbera d\u2019Alba (2021)": {"barbera", "d", "alba", "2021"},
	} {
		if got := normalizeWords(in); !slices.Equal(got, want) {
			t.Errorf("normalizeWords(%q) = %q, want %q", in, got, want)
		}
	}
}
