package wine

import (
	"context"
	"errors"
	"testing"
)

// nagus-d2u: leftovers from the !30 reviews. Each table names the rule it
// pins; wantWine nil means the title must stay wine.

func extractTyped(title, wineType string) error {
	s := sanitized(title, "")
	if wineType != "" {
		s.Aspects = map[string]string{"wine_type": wineType}
	}
	_, err := New().Extract(context.Background(), s)
	return err
}

func checkVerdicts(t *testing.T, cases []struct {
	title, wineType string
	want            error
}) {
	t.Helper()
	for _, tc := range cases {
		err := extractTyped(tc.title, tc.wineType)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%q (wine_type %q): %v, want wine", tc.title, tc.wineType, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%q (wine_type %q): %v, want %v", tc.title, tc.wineType, err, tc.want)
		case tc.want == ErrNotWine && (errors.Is(err, ErrCulinary) || errors.Is(err, ErrMerchandise)):
			t.Errorf("%q (wine_type %q): %v, want plain ErrNotWine", tc.title, tc.wineType, err)
		}
	}
}

// Soft food nouns count only as the title's last word (a trailing quantity
// aside); earlier they are a wine's name.
func TestLeftovers_SoftFoodNounsAtTheEnd(t *testing.T) {
	checkVerdicts(t, []struct {
		title, wineType string
		want            error
	}{
		{"Merlot Chocolate Sauce", "", ErrCulinary},
		{"Champagne Truffles", "", ErrCulinary},
		{"Calistoga Sparkling Water", "", ErrCulinary},
		{"Cabernet Chocolate Bar", "", ErrCulinary},
		{"Cabernet Chocolate Bars", "", ErrCulinary},
		{"Syrah Dark Chocolate", "", ErrCulinary},
		{"Pinot Noir Fudge", "", ErrCulinary},
		{"Pinot Noir Chocolate Sauce 8oz", "", ErrCulinary},
		{"Riesling Chocolate Truffles 12 pc", "", ErrCulinary},
		{"Merlot Truffle 8 oz", "", ErrCulinary},
		{"Chardonnay Mineral Water", "", ErrCulinary},
		{"Cabernet Chocolate Sauce", "Red", ErrCulinary},
		{"The Chocolate Block", "Red", nil},
		{"Boekenhoutskloof The Chocolate Block 2021", "", nil},
		{"Chocolate Shop Cabernet", "", nil},
		{"Truffle Hill Chardonnay", "", nil},
		{"Water Street Merlot 2020", "", nil},
		{"Cold Water Creek Chardonnay", "", nil},
		{"Sauce Boss Red Blend", "", nil},
		{"Sparkling Rose", "", nil},
		{"2023 WATER WITCH", "", nil},
		{"Champagne Truffle Collection", "", nil},
	})
}

// A fortified or appellation word with a dish after it is the dish, even
// when the source declares a wine_type; a food word BEFORE the wine word
// names the wine, unless a conjunction makes two products.
func TestLeftovers_CulinaryHeadAndDeclaredType(t *testing.T) {
	checkVerdicts(t, []struct {
		title, wineType string
		want            error
	}{
		{"Marsala Chicken Kit", "", ErrCulinary},
		{"Marsala Chicken Kit", "Red", ErrCulinary},
		{"Marsala Chicken Kit", "Dessert", ErrCulinary},
		{"Marsala Chicken", "", ErrCulinary},
		{"Port Braised Beef Kit", "", ErrCulinary},
		{"Port Glaze", "", ErrCulinary},
		{"Barolo Braised Beef", "", ErrCulinary},
		{"Chianti Cooking Sauce", "Red", ErrCulinary},
		{"Sherry Trifle Mix", "White", ErrCulinary},
		{"Salami and Chianti Pairing Box", "", ErrCulinary},
		{"Truffle Hunter Barolo", "", nil},
		{"Chocolate Port", "", nil},
		{"Chocolate Port", "Red", nil},
		{"Madeira Mix 3-Pack", "", nil},
		{"Holiday Port Mix Case", "", nil},
		{"Tawny Owl Reserve", "Red", nil},
		{"Marsala", "Dessert", nil},
		{"Florio Marsala Fine", "", nil},
	})
}

// "Wine" before an object noun makes the object the head.
func TestLeftovers_WineObject(t *testing.T) {
	checkVerdicts(t, []struct {
		title, wineType string
		want            error
	}{
		{"Wine Tool Chardonnay Edition", "", ErrMerchandise},
		{"Wine Tool Pinot Noir Edition", "", ErrMerchandise},
		{"Wine Tool Chardonnay Edition 2021", "", ErrMerchandise},
		{"Wine Charm Merlot", "", ErrMerchandise},
		{"Tool Shed Red 2019", "", nil},
		{"Tool Time Chardonnay 2021", "", nil},
		{"Toolbox Chardonnay", "", nil},
		{"Charm City Syrah 2020", "", nil},
	})
}

// Broad names: a famous cuvee is specific, an estate word supports the
// name like a classification, and lodging, venues and merchandise still
// withdraw it. A bare broad name stays not-wine (its recall cost).
func TestLeftovers_BroadNameCuveeAndEstate(t *testing.T) {
	checkVerdicts(t, []struct {
		title, wineType string
		want            error
	}{
		{"Hermitage La Chapelle", "", nil},
		{"Paul Jaboulet Aine Hermitage La Chapelle", "", nil},
		{"Domaine Jean-Louis Chave Hermitage", "", nil},
		{"Quinta do Crasto Douro", "", nil},
		{"Clos du Val Napa Valley", "", nil},
		{"Chateau Montelena Napa Valley", "", nil},
		{"Domaine Ste. Michelle Columbia Valley Brut", "", nil},
		{"Hermitage", "", ErrNotWine},
		{"Douro", "", ErrNotWine},
		{"La Chapelle", "", ErrNotWine},
		{"The Hermitage La Chapelle Inn", "", ErrNotWine},
		{"Hermitage Hotel", "", ErrNotWine},
		{"Napa Valley Chateau Wedding Venue", "", ErrNotWine},
		{"Tenuta Tuscany Villa Rental", "", ErrNotWine},
		{"Bodega Bay Sonoma", "", ErrNotWine},
		{"Quinta do Lago Algarve Resort", "", ErrNotWine},
		{"Chateau Napa Valley Throw Pillow", "", ErrNotWine},
		{"Domaine Burgundy Tee", "", ErrMerchandise},
		{"Chateau Burgundy Candle", "", ErrMerchandise},
		{"Chateau Bordeaux Map", "", ErrNotWine},
	})
}
