package wine

import (
	"context"
	"errors"
	"testing"
)

// The !30 review (424ef02) attack titles, each pinned.

// R-1: colour words and keywords are head cues for the culinary rule.
func TestReview_ColourIsAHeadCue(t *testing.T) {
	for _, title := range []string{
		"Jam Session Red Blend", "Honey Badger Red Blend", "Mustard Seed Cellars Red Wine",
		"Honey Bee White Wine", "Mustard Seed Red 2020", "Syrup Hill Red 2021",
		// A fortified style and the colour keywords are head cues too.
		"Honey Badger Tawny Port", "Honey Badger Sparkling", "Jelly Roll Rose", "Jam Jar Prosecco",
	} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

// R-2: a food word in a producer name after the wine, and the LAST food
// noun decides (not the first).
func TestReview_ProducerNameFoodWord(t *testing.T) {
	for _, title := range []string{"Butter Chardonnay by JaM", "2022 Butter Chardonnay by JaM Cellars"} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
	// The first food noun is followed by a varietal, the last is the head:
	// culinary. Choosing the first noun would call it wine.
	for _, title := range []string{"Honey Badger Chardonnay Cheese", "Jam Session Merlot Jelly"} {
		if err := extractErr(title); !errors.Is(err, ErrCulinary) {
			t.Errorf("%q: %v, want ErrCulinary (the last food noun is the head)", title, err)
		}
	}
	// "JaM Cellars Butter": the food word is the producer; with no wine cue
	// it is plain not-wine, not culinary.
	err := extractErr("JaM Cellars Butter")
	if !errors.Is(err, ErrNotWine) || errors.Is(err, ErrCulinary) {
		t.Errorf("JaM Cellars Butter: %v, want a bare ErrNotWine", err)
	}
}

// R-4: specific-tier false positives.
func TestReview_AppellationFalsePositives(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  error
	}{
		{"Sancerre Tee", ErrMerchandise},
		{"Barbaresco Socks", ErrMerchandise},
		{"Pomerol Sticker", ErrMerchandise},
		{"Sancerre Magnet", ErrMerchandise},
		{"Margaux Perfume", ErrMerchandise},
		{"Rioja Cookbook", ErrNotWine},
		{"Bouzy Babe Tee", ErrMerchandise},
		{"Ventoux Cycling Jersey", ErrMerchandise},
		{"Brunello Cucinelli Cashmere Sweater", ErrMerchandise},
		{"Barolo Coffee Mug", ErrMerchandise},
		{"Chianti Salami", ErrCulinary},
		{"Rioja Chorizo", ErrCulinary},
		{"Barolo Truffles", ErrCulinary},
		{"Chablis Oysters", ErrCulinary},
		{"Pantelleria Capers", ErrNotWine},
		{"Collioure Anchovies", ErrNotWine},
		{"Montrachet Goat Log", ErrNotWine},
		{"Soave Pasta", ErrCulinary},
		{"Chianti Marinara", ErrCulinary},
		{"Barolo Pizza", ErrCulinary},
		{"Vesuvio Pizza Oven", ErrNotWine},
		{"Douro River Cruise", ErrNotWine},
		{"Santorini Vacation Package", ErrNotWine},
		{"Hermitage Museum Visit", ErrNotWine},
		{"Etna Volcano Hike", ErrNotWine},
		{"Chablis Gift Voucher", ErrNotWine},
		{"Rioja Rioja Rioja Package", ErrNotWine},
		{"Rioja Food Tour", ErrNotWine},
		{"Rioja Wine Cruise", ErrNotWine},
		// Found running the attack sets.
		{"Barolo Gelato", ErrCulinary},
		{"Barolo Risotto Kit", ErrCulinary},
		{"Sauternes Foie Gras", ErrCulinary},
		{"Sancerre Pickleball Paddle", ErrMerchandise},
	} {
		err := extractErr(tc.title)
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: %v, want %v", tc.title, err, tc.want)
		}
	}
	// Moved to the broad tier: evidence only with a cue, and the full
	// Brunello name stays specific.
	for _, title := range []string{"Vesuvio", "Santorini", "Ischia", "Pantelleria", "Hermitage", "Douro", "Macon", "Dao", "Bouzy", "Ventoux", "Wagram", "Schlossberg", "Collioure", "Montrachet", "Brunello"} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine (broad alone)", title, err)
		}
	}
	for _, title := range []string{"Hermitage Rouge", "Douro Tinto", "Montrachet Grand Cru", "Brunello di Montalcino", "Brunello Riserva", "Santorini Assyrtiko DOC", "New Jersey Chardonnay"} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

// R-5: a broad name's colour must sit next to it, in a title with no
// object noun.
func TestReview_BroadColourAdjacency(t *testing.T) {
	for _, title := range []string{
		"Burgundy Rouge Sweater", "Burgundy Blanc Throw Pillow", "Bordeaux Blanc Paint Swatch",
		"Napa Valley Cap Red", "Napa Valley Gift Card Holder Red", "Napa Valley Photo Album Collection Red",
		"Red Barn and Farm Napa Valley", // a colour far from the name
	} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine", title, err)
		}
	}
	for _, title := range []string{"Napa Valley Red", "Red Mountain Pioneer Red IV", "Rouge Bordeaux", "Bordeaux Blanc"} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

// R-6: Angelica alone is a herb or a name; beside a year, a bottle size,
// "dessert wine" or a declared wine type it is the wine.
func TestReview_AngelicaNeedsAVoucher(t *testing.T) {
	for _, title := range []string{"Angelica Root Tea", "Angelica Pickles", "Angel Island Angelica", "Angelica"} {
		err := extractErr(title)
		if !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine", title, err)
		}
	}
	if err := extractErr("Angelica Houston Poster"); !errors.Is(err, ErrMerchandise) {
		t.Errorf("Angelica Houston Poster: %v, want ErrMerchandise", err)
	}
	for _, s := range []struct{ title, body, wineType string }{
		{"2020 Angelica", "", ""},
		{"Broc Angelica 500ml", "", ""},
		{"Broc Angelica", "a very special, very limited dessert wine", ""},
		{"Broc Angelica", "", "dessert"},
	} {
		in := sanitized(s.title, s.body)
		in.Aspects = map[string]string{}
		if s.wineType != "" {
			in.Aspects["wine_type"] = s.wineType
		}
		if _, err := New().Extract(context.Background(), in); err != nil {
			t.Errorf("%q (body %q, type %q): %v, want wine", s.title, s.body, s.wineType, err)
		}
	}
}

// R-7: an NV marker and a bare colour do not vouch for each other.
func TestReview_NVAndBareColourNeedAThirdCue(t *testing.T) {
	for _, title := range []string{"Red NV", "Rouge NV", "White NV Sticker", "Something Tinto NV", "Casa Blanco NV"} {
		if err := extractErr(title); !errors.Is(err, ErrNotWine) {
			t.Errorf("%q: %v, want ErrNotWine", title, err)
		}
	}
	it, err := New().Extract(context.Background(), sanitized("Cuvee Rouge NV", ""))
	if err != nil {
		t.Fatal(err)
	}
	if it.Attributes["colour"] != "red" || it.Attributes["nv"] != "true" {
		t.Fatalf("Cuvee Rouge NV: colour=%q nv=%q, want red true", it.Attributes["colour"], it.Attributes["nv"])
	}
}

// R-8: an object noun followed by a wine cue is a name, not the object.
func TestReview_ObjectNounHeadRule(t *testing.T) {
	for _, title := range []string{"Charm City Syrah 2020", "Soap Box Chardonnay 2021", "Tool Shed Red 2019", "Towel Merlot 2022"} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

// R-9: both declared -> the title decides; a food bundle is merchandise.
func TestReview_DeclaredBothAndBundles(t *testing.T) {
	both := map[string]string{"product_type": "Pantry", "tags": "holiday, merch, Valentine"}
	if got := declaredNonWine(both, "Marta's Prints"); got != ErrMerchandise {
		t.Errorf("Marta's Prints: %v, want ErrMerchandise", got)
	}
	if got := declaredNonWine(both, "June Taylor Mission Fig + Angelica Jam"); got != ErrCulinary {
		t.Errorf("fig jam: %v, want ErrCulinary", got)
	}
	for _, title := range []string{"Spritz Pack w/ June Taylor Seasonal Fruit Syrup", "Cheese and Port Gift Set", "Honey with Chardonnay", "Olive Oil Duo"} {
		err := extractErr(title)
		if !errors.Is(err, ErrMerchandise) || errors.Is(err, ErrCulinary) {
			t.Errorf("%q: %v, want ErrMerchandise (a bundle)", title, err)
		}
	}
}

// R-10: more accents fold; mead is plain not-wine.
func TestReview_AccentsAndMead(t *testing.T) {
	if err := extractErr("Oltrep\u00f2 Pavese Metodo Classico"); err != nil {
		t.Errorf("Oltrepo Pavese Metodo Classico: %v, want wine", err)
	}
	if got := normalizeWords("Oltrep\u00f2 L\u00ecvio \u00d9mbria"); len(got) != 3 || got[0] != "oltrepo" || got[1] != "livio" || got[2] != "umbria" {
		t.Errorf("normalizeWords = %q", got)
	}
	for _, title := range []string{"Honey Wine", "Honey Mead", "Orange Blossom Honey Mead"} {
		err := extractErr(title)
		if !errors.Is(err, ErrNotWine) || errors.Is(err, ErrCulinary) {
			t.Errorf("%q: %v, want a bare ErrNotWine (mead)", title, err)
		}
	}
}
