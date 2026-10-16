package promo

import (
	"net/http"
	"testing"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
)

// Other products and other sizes of the same name are refused; a producer
// suffix on the slug is the only one-token escape.
func TestSlugVariantsAndSizes(t *testing.T) {
	const wc = "https://www.wine.com/product/"
	for _, tc := range []struct {
		slug, title string
		want        bool
	}{
		{"opus-one-2019", "Overture by Opus One 2019", false},
		{"dom-perignon-rose-2012", "Dom Perignon 2012", false},
		{"chianti-classico-riserva-2019", "Chianti Classico 2019", false},
		{"caymus-cabernet-sauvignon-1-5-liter-2021", "Caymus Cabernet Sauvignon 2021", false},
		{"caymus-cabernet-sauvignon-magnum-2021", "Caymus Cabernet Sauvignon 2021", false},
		{"caymus-cabernet-sauvignon-375ml-2021", "Caymus Cabernet Sauvignon 2021", false},
		{"caymus-cabernet-sauvignon-2021", "Caymus Cabernet Sauvignon 2021 3-Pack", false},
		{"veuve-clicquot-brut", "Veuve Clicquot Extra Brut", false},
		{"moet-chandon-imperial-brut", "Moet & Chandon Imperial Brut Rose", false},
		{"ridge-zinfandel", "Ridge Geyserville Zinfandel", false},
		{"caymus-cabernet-sauvignon-1-5-liter-2021", "Caymus Cabernet Sauvignon 1.5L 2021", true},
		{"caymus-cabernet-sauvignon-magnum-2021", "Caymus Cabernet Sauvignon 2021 1.5 Liter", true},
		{"caymus-cabernet-sauvignon-2021", "Caymus Cabernet Sauvignon 2021 750ml", true},
		{"caymus-vineyards-cabernet-sauvignon-2021", "Caymus Cabernet Sauvignon 2021", true},
		{"the-prisoner-red-blend-2022", "The Prisoner Red Blend 2022", true},
		{"ridge-geyserville-zinfandel-2021", "Ridge Geyserville Zinfandel 2021", true},
	} {
		if got := slugMatchesTitle(wc+tc.slug+"/1", tc.title, false); got != tc.want {
			t.Errorf("slugMatchesTitle(%s, %q) = %v, want %v", tc.slug, tc.title, got, tc.want)
		}
	}
}

// Accented titles match their unaccented slugs, and two reads that differ
// only in accents agree.
func TestAccentsFolded(t *testing.T) {
	const wc = "https://www.wine.com/product/"
	for slug, title := range map[string]string{
		"chateau-margaux-2015":        "Ch\u00e2teau Margaux 2015",
		"guigal-cotes-du-rhone-2020":  "Guigal C\u00f4tes du Rh\u00f4ne 2020",
		"hugl-gruner-veltliner-2022":  "H\u00fcgl Gr\u00fcner Veltliner 2022",
		"domaine-leflaive-macon-2021": "Domaine Leflaive M\u00e2con 2021",
	} {
		if !slugMatchesTitle(wc+slug+"/1", title, false) {
			t.Errorf("slugMatchesTitle(%s, %q) = false", slug, title)
		}
	}
	if _, drop := Agree(read(t, product("Ch\u00e2teau Margaux 2015", "599.99", "")), read(t, product("Chateau Margaux 2015", "599.99", ""))); drop != "" {
		t.Fatalf("accent-only difference: %q", drop)
	}
}

func TestCategoryFitFullTitleSpecs(t *testing.T) {
	for title, want := range map[string]bool{
		"Lenovo IdeaCentre 5 Desktop 1TB HDD for Gaming, Ryzen 7, 32GB DDR5":         false,
		"Dell Inspiron 3030 1TB Hard Drive":                                          false,
		"HP Pavilion 2TB HDD":                                                        false,
		"Apple iMac 1TB Hard Drive":                                                  false,
		"Apple Mac mini 1TB HDD":                                                     false,
		"Buffalo LinkStation 4TB HDD":                                                false,
		"WD My Cloud Home 8TB HDD":                                                   false,
		"Synology DiskStation 2x 8TB HDD":                                            false,
		"Seagate Personal Cloud 4TB Hard Drive":                                      false,
		"WD Blue 4TB Desktop Hard Disk Drive":                                        true,
		"Seagate BarraCuda 2TB Internal Hard Drive 7200 RPM for Computer Desktop PC": true,
		"Seagate Expansion Desktop 16TB External Hard Drive":                         true,
		"Toshiba X300 Performance 8TB Desktop & Gaming Hard Drive":                   true,
		"Seagate IronWolf 8TB NAS Hard Drive":                                        true,
		"WD My Book 18TB External Hard Drive":                                        true,
	} {
		if got := categoryFit(deal.CategoryHDD, title); got != want {
			t.Errorf("categoryFit(%q) = %v, want %v", title, got, want)
		}
	}
}

// A key refused at read B refunds only read B: read A was spent.
func TestAuthAtReadBRefundsOne(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "winecom", category: deal.CategoryWine, perDay: 100}, "winecom.html")
	r.fv.statusB.Store(http.StatusForbidden)
	doc := `<a href="https://links.e.wine.com/ls/click?t=1"><img src="` + r.cands[0].Src + `" alt="Wine.com" width="280"></a>`
	r.parse(t, imapmail.Message{ID: "wc-z@example.com", From: wineSender, HTML: doc})
	if st := r.budget.Stats(); st.UsedToday != 1 || !r.p.incomplete() || r.m.Drops(r.src, DropVisionFailed) != 0 {
		t.Fatalf("used %d (want 1: read A), incomplete %v, drops %d", st.UsedToday, r.p.incomplete(), r.m.Drops(r.src, DropVisionFailed))
	}
}
