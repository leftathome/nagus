package promo

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustProfile(t *testing.T, name string) *Profile {
	t.Helper()
	p, err := Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The scrubbed fixtures select exactly the product images.
func TestSelectFixtures(t *testing.T) {
	for _, tc := range []struct {
		profile, file string
		want          int
		check         func(t *testing.T, c Candidate)
	}{
		{"newegg", "newegg.html", 20, func(t *testing.T, c Candidate) {
			if !strings.HasPrefix(c.Src, "https://c2.neweggimages.com/") || c.Item == "" {
				t.Errorf("newegg candidate %+v", c)
			}
		}},
		{"winecom", "winecom.html", 4, func(t *testing.T, c Candidate) {
			// The RichRelevance images are http:// in the mail: upgraded.
			if !strings.HasPrefix(c.Src, "https://image.richrelevance.com/") || !strings.HasPrefix(c.Href, "https://links.e.wine.com/") {
				t.Errorf("winecom candidate %+v", c)
			}
		}},
		{"totalwine", "totalwine.html", 3, func(t *testing.T, c Candidate) {
			if !strings.HasPrefix(c.Src, "https://mi.totalwine.com/") || !strings.HasPrefix(c.Href, "https://click.email-totalwine.com/") {
				t.Errorf("totalwine candidate %+v", c)
			}
		}},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			sel := SelectImages(fixture(t, tc.file), mustProfile(t, tc.profile))
			if len(sel.Candidates) != tc.want {
				t.Fatalf("selected %d, want %d (seen %d, skipped %v)", len(sel.Candidates), tc.want, sel.Seen, sel.Skipped)
			}
			for _, c := range sel.Candidates {
				tc.check(t, c)
			}
			total := len(sel.Candidates)
			for _, n := range sel.Skipped {
				total += n
			}
			if total != sel.Seen {
				t.Errorf("selected+skipped %d != seen %d (%v)", total, sel.Seen, sel.Skipped)
			}
		})
	}
}

func TestSelectHeuristics(t *testing.T) {
	wc := mustProfile(t, "winecom")
	link := `https://links.e.wine.com/ls/click?t=X`
	img := func(attrs string) string {
		return `<html><body><a href="` + link + `"><img ` + attrs + `></a></body></html>`
	}
	for _, tc := range []struct {
		name, doc, reason string
	}{
		{"ok", img(`src="http://image.richrelevance.com/rrmail/image/recs?p=1" alt="Wine.com" width="280"`), ""},
		{"other host", img(`src="https://braze-images.com/x.png" alt="banner" width="600"`), SkipImageHost},
		{"tracking pixel", img(`src="https://image.richrelevance.com/p.gif" width="1" height="1"`), SkipTiny},
		{"zero height", img(`src="https://image.richrelevance.com/p.gif" width="1" height="0"`), SkipTiny},
		{"style tiny", img(`src="https://image.richrelevance.com/a.png" style="width:24px;height:24px"`), SkipTiny},
		{"hidden", img(`src="https://image.richrelevance.com/a.png" style="display:none"`), SkipHidden},
		{"logo", img(`src="https://image.richrelevance.com/a.png" alt="Company Logo" width="200"`), SkipNav},
		{"social", img(`src="https://image.richrelevance.com/a.png" alt="Instagram" width="200"`), SkipNav},
		{"x", img(`src="https://image.richrelevance.com/a.png" alt="X" width="200"`), SkipNav},
		{"no src", img(`alt="Wine.com"`), SkipNoImageSrc},
		{"data url", img(`src="data:image/png;base64,AAAA" width="200"`), SkipBadImgURL},
		{"odd port", img(`src="https://image.richrelevance.com:8443/a.png" width="200"`), SkipBadImgURL},
		{"no link", `<img src="https://image.richrelevance.com/a.png" width="280">`, SkipNoLink},
		{"link elsewhere", `<a href="https://evil.example.com/x"><img src="https://image.richrelevance.com/a.png" width="280"></a>`, SkipNoLink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := SelectImages(tc.doc, wc)
			if tc.reason == "" {
				if len(sel.Candidates) != 1 || sel.Candidates[0].Href != link {
					t.Fatalf("got %+v skipped %v", sel.Candidates, sel.Skipped)
				}
				return
			}
			if len(sel.Candidates) != 0 || sel.Skipped[tc.reason] != 1 {
				t.Fatalf("want skip %s, got %+v %v", tc.reason, sel.Candidates, sel.Skipped)
			}
		})
	}
}

func TestSelectNeweggAltAndCap(t *testing.T) {
	ne := mustProfile(t, "newegg")
	var b strings.Builder
	b.WriteString(`<img src="https://c2.neweggimages.com/banner.jpg" alt="1015Item" width="650">`)
	b.WriteString(`<img src="https://c2.neweggimages.com/a.jpg" alt="item_34-156-677" width="214">`)
	b.WriteString(`<img src="https://c2.neweggimages.com/a.jpg" alt="item_34-156-677" width="214">`)
	for i := 0; i < 30; i++ {
		b.WriteString(`<img src="https://c2.neweggimages.com/t` + string(rune('a'+i)) + `.jpg" alt="item_9SIC11UKGE3529">`)
	}
	sel := SelectImages(b.String(), ne)
	if len(sel.Candidates) != ne.MaxImages || sel.Skipped[SkipNoItem] != 1 || sel.Skipped[SkipDuplicate] != 1 || sel.Skipped[SkipOverCap] != 31-ne.MaxImages {
		t.Fatalf("candidates %d skipped %v", len(sel.Candidates), sel.Skipped)
	}
	if sel.Candidates[0].Item != "34-156-677" || sel.Candidates[1].Item != "9SIC11UKGE3529" {
		t.Fatalf("items %q %q", sel.Candidates[0].Item, sel.Candidates[1].Item)
	}
}
