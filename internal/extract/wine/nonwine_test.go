package wine

import (
	"context"
	"errors"
	"testing"

	"github.com/leftathome/nagus/internal/connector/shopify"
	"github.com/leftathome/nagus/internal/sanitize"
)

// The production false positive of 2026-09-25 (item c565b9b58ede21ab),
// through the real Shopify connector and the passthrough boundary: Broc
// Cellars files "June Taylor Mission Fig + Angelica Jam" under product_type
// Pantry with tags merch and pantry, and its description names "our 2020
// Angelica dessert wine". It must be culinary. Beside it on the same store,
// "2020 Angelica" (a fortified dessert wine) and a Wine tagged holiday stay
// wine, and a decanter filed under Wine but tagged merch is merchandise.
func TestExtract_StoreDeclaredNonWine_BrocCellars(t *testing.T) {
	c := shopify.NewConnector(shopify.Config{Name: "broc-cellars", BaseURL: "https://broccellars.com", FixturePath: "testdata/broc_cellars_2026-09-25.json"})
	raws, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]error{
		"June Taylor Mission Fig + Angelica Jam": ErrCulinary,
		"2020 Angelica":                          nil,
		"2023 Michael Mara Chardonnay":           nil,
		"Decanter":                               ErrMerchandise,
	}
	seen := 0
	for _, r := range raws {
		w, ok := want[r.Title]
		if !ok {
			continue
		}
		seen++
		s, err := sanitize.Passthrough{}.Sanitize(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		it, err := New().Extract(context.Background(), s)
		switch {
		case w == nil && err != nil:
			t.Errorf("%q: %v, want wine", r.Title, err)
		case w != nil && !errors.Is(err, w):
			t.Errorf("%q: %v (vintage %q), want %v", r.Title, err, it.Attributes["vintage"], w)
		}
	}
	if seen != len(want) {
		t.Fatalf("fixture yielded %d of %d titles", seen, len(want))
	}
}

// The declaration rejects only; the words are exact, so "gifts" and
// "holiday" never do.
func TestDeclaredNonWine(t *testing.T) {
	for _, tc := range []struct {
		productType, tags string
		want              error
	}{
		{"Pantry", "", ErrCulinary},
		{"  FOOD ", "", ErrCulinary},
		{"Grocery", "", ErrCulinary},
		{"", "2021, merch, pantry", ErrCulinary},
		{"Wine", "food", ErrCulinary},
		{"Merch", "", ErrMerchandise},
		{"Merchandise", "", ErrMerchandise},
		{"Apparel", "", ErrMerchandise},
		{"Gift Card", "", ErrMerchandise},
		{"Gift Cards", "", ErrMerchandise},
		{"Accessories", "", ErrMerchandise},
		{"Wine Accessories", "", ErrMerchandise},
		{"Glassware", "", ErrMerchandise},
		{"Books", "", ErrMerchandise},
		{"Events", "", ErrMerchandise},
		{"Special Event", "", ErrMerchandise},
		{"Tickets", "", ErrMerchandise},
		{"Membership", "", ErrMerchandise},
		{"Wine", "gift, merch", ErrMerchandise},
		{"", "Merchandise", ErrMerchandise},
		{"", "apparel", ErrMerchandise},
		{"", "Gift Card", ErrMerchandise},
		{"Wine", "gifts, holiday, Gift, Valentine", nil},
		{"Wine Pack", "packs, wine", nil},
		{"", "", nil},
		{"Pantry Wine", "", nil},      // exact types only
		{"Wine", "online merch", nil}, // exact tags only
	} {
		got := declaredNonWine(map[string]string{"product_type": tc.productType, "tags": tc.tags})
		if got != tc.want {
			t.Errorf("type %q tags %q: %v, want %v", tc.productType, tc.tags, got, tc.want)
		}
	}
	// Through Extract: a declared type beats every other cue, and a
	// holiday-tagged Wine stays wine.
	s := sanitized("2021 Pinot Noir", "")
	s.Aspects = map[string]string{"product_type": "Merchandise"}
	if _, err := New().Extract(context.Background(), s); !errors.Is(err, ErrMerchandise) {
		t.Errorf("declared merchandise: %v", err)
	}
	s.Aspects = map[string]string{"product_type": "Wine", "tags": "gifts, holiday"}
	if _, err := New().Extract(context.Background(), s); err != nil {
		t.Errorf("holiday-tagged wine: %v", err)
	}
}

// Title culinary nouns are the product unless a wine cue follows as the
// head of the title (nagus !28 final review nit 1; the fig jam).
func TestExtract_CulinaryTitleHeadRule(t *testing.T) {
	culinary := []string{
		"Sherry Vinegar", "Madeira Cake", "Chardonnay Jam", "Pinot Noir Jelly", "Camino Red Wine Vinegar",
		"Apricot Jam 2021",         // a year is not a head cue
		"Cheese and Port Gift Set", // a conjunction
		"Honey with Chardonnay",    // ditto
		"JaM Cellars Butter",       // no wine cue at all
		"Fig Preserves", "Orange Marmalade", "Mango Chutney", "Cherry Compote", "Fruit Syrup",
		"Wildflower Honey", "Dijon Mustard", "Fox Hill Olive Oil",
		"Zinfandel Cooking Wine", "Cooking Sherry",
	}
	for _, title := range culinary {
		if err := extractErr(title); !errors.Is(err, ErrCulinary) {
			t.Errorf("%q: %v, want ErrCulinary", title, err)
		}
	}
	// The body cannot rescue it (the fig jam's vintage came from its body).
	s := sanitized("June Taylor Mission Fig + Angelica Jam", "a fig preserve made with our 2020 Angelica dessert wine")
	if _, err := New().Extract(context.Background(), s); !errors.Is(err, ErrCulinary) {
		t.Errorf("fig jam with a body vintage: %v, want ErrCulinary", err)
	}
	for _, title := range []string{
		"Cake Bread Cellars Chardonnay", "Jelly Roll Zinfandel", "Vinegar Hill Syrah",
		"JaM Cellars Butter Chardonnay", "Honey Badger Sparkling",
		"Cake Walk Barolo", "Honeybee Rose",
	} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
	// A spirit with a food word is not culinary, just not wine.
	for _, title := range []string{"Revelton Honey Whiskey", "Bees Knees Honey Cask Bourbon"} {
		err := extractErr(title)
		if !errors.Is(err, ErrNotWine) || errors.Is(err, ErrCulinary) {
			t.Errorf("%q: %v, want a bare ErrNotWine", title, err)
		}
	}
}

// Object merchandise is not rescued by a varietal, a year or a pack count
// -- only by a bottle size or an explicit pack of wine (nit: branded
// merchandise with a grape name).
func TestExtract_ObjectMerchandise(t *testing.T) {
	for _, title := range []string{
		"Merlot Tea Towel", "Riesling Wine Charm", "Pinot Noir Wine Tool", "Chardonnay Soap Gift Box",
		"2-Pack Champagne Flutes", "2019 Wine Charm Set", "Cabernet Sauvignon Soap 2020", "Champagne Flute 2019",
	} {
		if err := extractErr(title); !errors.Is(err, ErrMerchandise) {
			t.Errorf("%q: %v, want ErrMerchandise", title, err)
		}
	}
	for _, title := range []string{
		"6PK Gift Box Wood MRW, 2021 Mix", // Mark Ryan: real wine in a gift box
		"Champagne Flute 750ml",
		"Pinot Noir Wine Set with Tool",
		"Chardonnay Magnum with Towel",
		"Riesling 6 Bottles with Charm",
		"2021 Merlot 1.5L Tool Kit",
	} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
}

func TestExtract_BareCabernet(t *testing.T) {
	for _, tc := range []struct{ title, varietal string }{
		{"Cabernet", "Cabernet"},
		{"Cabernet Exploration Collection", "Cabernet"},
		{"2020 K Powerline Cabernet -1.5L", "Cabernet"},
		{"Cabernet Merlot", "Merlot"}, // last in the list: any other grape wins
		{"Cabernet Sauvignon", "Cabernet Sauvignon"},
		{"Cabernet Franc", "Cabernet Franc"},
	} {
		it, err := New().Extract(context.Background(), sanitized(tc.title, ""))
		if err != nil {
			t.Errorf("%q: %v", tc.title, err)
			continue
		}
		if it.Attributes["varietal"] != tc.varietal || it.Attributes["colour"] != "red" {
			t.Errorf("%q: varietal=%q colour=%q, want %q red", tc.title, it.Attributes["varietal"], it.Attributes["colour"], tc.varietal)
		}
	}
}
