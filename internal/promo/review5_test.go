package promo

import (
	"net/url"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/deal"
)

// Newegg's marketplace item-identity URL names no product: it is slug-less,
// so the profile's allowSluglessLinks rule keeps it (unverified) instead of
// reading "Product.aspx" as a slug that never matches.
func TestNeweggProductAspxIsSlugless(t *testing.T) {
	u := "https://www.newegg.com/Product/Product.aspx?Item=9SIA12345678"
	if s := slugOf(u); s != "" {
		t.Fatalf("slugOf(%s) = %q, want none", u, s)
	}
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD}, "newegg.html")
	verified, why := r.p.linkCheck(true, u, "Seagate IronWolf Pro 20TB NAS Hard Drive")
	if why != "" || verified {
		t.Fatalf("linkCheck = (%v, %q), want kept unverified", verified, why)
	}
}

// A wine.com slug segment must name something: a digits-only segment before
// the id cannot pose as a slug-less link.
func TestWinecomDigitsOnlySlugRefused(t *testing.T) {
	wc := mustProfile(t, "winecom")
	for in, want := range map[string]bool{
		"https://www.wine.com/product/1945/123":                           false,
		"https://www.wine.com/product/123":                                true,
		"https://www.wine.com/product/tolaini-al-passo-2022/3721589":      true,
		"https://www.wine.com/product/2019-opus-one/3721589":              true,
		"https://www.wine.com/product/" + strings.Repeat("a", 130) + "/1": false,
	} {
		u, _ := url.Parse(in)
		if _, got := canonical(wc, u); got != want {
			t.Errorf("canonical(%s) = %v, want %v", in, got, want)
		}
	}
}

// Tiles with their own item number fill the cap before images found only
// through a tracked link, whatever their order in the mail.
func TestSelectItemTilesBeforeLinkFallback(t *testing.T) {
	p := *mustProfile(t, "newegg")
	p.MaxImages = 2
	img := func(src, alt string) string {
		return `<a href="https://l.promo.newegg.com/rts/go2.aspx?h=x"><img src="https://c2.neweggimages.com/` + src + `.jpg" alt="` + alt + `" width="214" height="520"></a>`
	}
	doc := "<html><body>" + img("banner1", "") + img("banner2", "") + img("t1", "item_22-234-441") + img("t2", "item_22-184-772") + "</body></html>"
	sel := SelectImages(doc, &p)
	if len(sel.Candidates) != 2 || sel.Candidates[0].Item == "" || sel.Candidates[1].Item == "" {
		t.Fatalf("candidates %+v, want the two item tiles", sel.Candidates)
	}
	if sel.Skipped[SkipOverCap] != 2 {
		t.Fatalf("over-cap skips %d, want the 2 banners", sel.Skipped[SkipOverCap])
	}
}
