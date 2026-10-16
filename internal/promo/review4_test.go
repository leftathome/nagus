package promo

import (
	"testing"

	"github.com/leftathome/nagus/internal/deal"
)

// Numbers other than years and sizes (a Bin number, a cuvee) must agree
// when both sides carry them.
func TestSlugOtherNumbers(t *testing.T) {
	const wc = "https://www.wine.com/product/"
	for _, tc := range []struct {
		slug, title string
		want        bool
	}{
		{"penfolds-bin-389-cabernet-shiraz-2020", "Penfolds Bin 707 Cabernet Shiraz 2020", false},
		{"penfolds-bin-389-cabernet-shiraz-2020", "Penfolds Bin 389 Cabernet Shiraz 2020", true},
		{"krug-grande-cuvee", "Krug Grande Cuvee 171eme Edition", true},
		{"caymus-cabernet-sauvignon-1-5-liter-2021", "Caymus Cabernet Sauvignon 1.5L 2021", true},
	} {
		if got := slugMatchesTitle(wc+tc.slug+"/1", tc.title, false); got != tc.want {
			t.Errorf("slugMatchesTitle(%s, %q) = %v, want %v", tc.slug, tc.title, got, tc.want)
		}
	}
}

// Where slugs carry the vintage (wine.com), a title year must be in the
// slug; Total Wine slugs carry none.
func TestSlugHasVintage(t *testing.T) {
	wc := mustProfile(t, "winecom")
	tw := mustProfile(t, "totalwine")
	if !wc.SlugHasVintage || tw.SlugHasVintage {
		t.Fatalf("profiles: winecom %v totalwine %v", wc.SlugHasVintage, tw.SlugHasVintage)
	}
	for _, tc := range []struct {
		u, title string
		flag     bool
		want     bool
	}{
		{"https://www.wine.com/product/caymus-cabernet-sauvignon/1", "Caymus Cabernet Sauvignon 2021", true, false},
		{"https://www.wine.com/product/caymus-cabernet-sauvignon-2021/1", "Caymus Cabernet Sauvignon 2021", true, true},
		{"https://www.wine.com/product/caymus-cabernet-sauvignon/1", "Caymus Cabernet Sauvignon", true, true},
		{"https://www.totalwine.com/wine/red-wine/caymus-cabernet-sauvignon/p/1", "Caymus Cabernet Sauvignon 2021", false, true},
	} {
		if got := slugMatchesTitle(tc.u, tc.title, tc.flag); got != tc.want {
			t.Errorf("slugMatchesTitle(%s, %q, %v) = %v, want %v", tc.u, tc.title, tc.flag, got, tc.want)
		}
	}
}

func TestCategoryFitRound4(t *testing.T) {
	for title, want := range map[string]bool{
		"Seagate IronWolf Pro 20TB NAS Hard Drive for 1-24 Bay NAS": true,
		"WD Red Plus 8TB NAS Hard Drive for 8-Bay NAS":              true,
		"Acer Veriton Desktop 2TB HDD":                              false,
		"MSI PRO DP21 Mini PC 1TB HDD":                              false,
		"Beelink Mini PC 2TB HDD Intel N100 16GB":                   false,
		"Dell Precision Tower 5860 2TB HDD Xeon":                    false,
		"Asustor Drivestor 2 Lite 8TB HDD":                          false,
		"HP Z2 Workstation 2TB Hard Drive":                          false,
		// real drives still pass
		"WD Blue 4TB Desktop Hard Disk Drive":                                        true,
		"Seagate BarraCuda 2TB Internal Hard Drive 7200 RPM for Computer Desktop PC": true,
		"Seagate Expansion Desktop 16TB External Hard Drive":                         true,
		"Toshiba X300 Performance 8TB Desktop & Gaming Hard Drive":                   true,
		"Seagate IronWolf 8TB NAS Hard Drive":                                        true,
		"WD My Book 18TB External Hard Drive":                                        true,
		"Seagate Exos X20 20TB SATA 7200 RPM":                                        true,
	} {
		if got := categoryFit(deal.CategoryHDD, title); got != want {
			t.Errorf("categoryFit(%q) = %v, want %v", title, got, want)
		}
	}
}
