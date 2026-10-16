package promo

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/listing"
)

// A producer's other wine shares most words with the one shown; generic
// slugs name a category page.
func TestSlugContentTokens(t *testing.T) {
	const wc = "https://www.wine.com/product/"
	const tw = "https://www.totalwine.com/wine/red-wine/cabernet-sauvignon/"
	for _, tc := range []struct {
		u, title string
		want     bool
	}{
		{wc + "caymus-vineyards-cabernet-sauvignon-2021/1", "Caymus Cabernet Sauvignon 2021", true},
		{wc + "caymus-special-selection-cabernet-sauvignon-2021/1", "Caymus Cabernet Sauvignon 2021", false},
		{wc + "the-prisoner-cabernet-sauvignon-2022/1", "The Prisoner Red Blend 2022", false},
		{wc + "the-prisoner-red-blend-2022/1", "The Prisoner Red Blend 2022", true},
		{wc + "tolaini-valdisanti-toscana-2022/1", "Tolaini Al Passo Toscana 2022", false},
		{wc + "tolaini-al-passo-toscana-2022/1", "Tolaini Al Passo Toscana 2022", true},
		{tw + "caymus-cabernet/p/1", "Caymus Special Selection Cabernet", false},
		{tw + "red-wine/p/1", "Red Wine", false},
		{wc + "red-blend/1", "Red Blend", false},
		{wc + "a/1", "A", false},
		{wc + "caymus-vineyards-cabernet-sauvignon-2021/1", "Caymus Cabernet Sauvignon 2020", false},
	} {
		if got := slugMatchesTitle(tc.u, tc.title, false); got != tc.want {
			t.Errorf("slugMatchesTitle(%s, %q) = %v, want %v", tc.u, tc.title, got, tc.want)
		}
	}
}

func TestCategoryFitPrebuilts(t *testing.T) {
	for title, want := range map[string]bool{
		"HP Desktop PC 1TB HDD 16GB RAM":                                             false,
		"Dell OptiPlex Tower Desktop 2TB Hard Drive":                                 false,
		"Lenovo ThinkCentre Desktop Computer 1TB HDD":                                false,
		"Synology DS224+ 2-Bay NAS 8TB HDD":                                          false,
		"QNAP 4-Bay NAS with 4x 4TB HDD":                                             false,
		"Acer Aspire Intel Core i5 1TB HDD":                                          false,
		"Gaming PC AMD Ryzen 7 2TB HDD Windows 11":                                   false,
		"WD Blue 4TB Desktop Hard Disk Drive":                                        true,
		"Seagate Expansion Desktop 16TB External Hard Drive":                         true,
		"Toshiba X300 Performance 8TB Desktop & Gaming Hard Drive":                   true,
		"Seagate BarraCuda 2TB Internal Hard Drive 7200 RPM for Computer Desktop PC": true,
		"WD Red Plus 12TB NAS Internal Hard Drive":                                   true,
		"Seagate IronWolf 8TB NAS Hard Drive":                                        true,
	} {
		if got := categoryFit(deal.CategoryHDD, title); got != want {
			t.Errorf("categoryFit(%q) = %v, want %v", title, got, want)
		}
	}
}

// Every number in the STORED title (read A's) must be confirmed by read B;
// B may add numbers A lacks.
func TestNumbersConfirmedDirection(t *testing.T) {
	for _, tc := range []struct {
		a, b, drop string
	}{
		{"WD Red Plus 12TB NAS Hard Drive", "WD Red Plus 12TB NAS Hard Drive 7200 RPM", ""},
		{"Caymus Cabernet Sauvignon", "Caymus Cabernet Sauvignon 750ml", ""},
		{"WD Red Plus 12TB NAS Hard Drive 7200 RPM", "WD Red Plus 12TB NAS Hard Drive", DropTitleDisagree},
		{"Seagate IronWolf 8TB NAS Hard Drive", "Seagate IronWolf 18TB NAS Hard Drive", DropTitleDisagree},
		{"2022 Caymus Cabernet Sauvignon", "2021 Caymus Cabernet Sauvignon", DropTitleDisagree},
	} {
		if _, drop := Agree(read(t, product(tc.a, "10.00", "")), read(t, product(tc.b, "10.00", ""))); drop != tc.drop {
			t.Errorf("A %q / B %q: drop %q, want %q", tc.a, tc.b, drop, tc.drop)
		}
	}
}

func TestTitleClippedToDealLimit(t *testing.T) {
	long := "WD Red Plus 12TB NAS Hard Drive " + strings.Repeat("extra ", 80)
	p, drop := Agree(read(t, product(long, "10.00", "")), read(t, product(long, "10.00", "")))
	if drop != "" || utf8.RuneCountInString(p.Title) > deal.MaxTitleLen {
		t.Fatalf("drop %q, title %d runes", drop, utf8.RuneCountInString(p.Title))
	}
}

// A refused key stops the poll: no attempt counted, calls refunded, the
// poll incomplete; once the key works, everything is read.
func TestParserVisionAuthStopsPoll(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 1000, perDay: 1000}, "newegg.html")
	r.fv.set(1, answers{A: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "")})
	r.fv.status.Store(http.StatusUnauthorized)
	for poll := 0; poll < 5; poll++ {
		before := r.fv.calls.Load()
		if raws := r.parse(t, neweggMsg(t)); len(raws) != 0 {
			t.Fatal("offers while the key is refused")
		}
		if !r.p.incomplete() {
			t.Fatal("an auth refusal left the poll complete")
		}
		if n := r.fv.calls.Load() - before; n > int64(DefaultConcurrency) {
			t.Fatalf("poll %d made %d calls after the key was refused", poll, n)
		}
	}
	if st := r.budget.Stats(); st.UsedToday != 0 {
		t.Fatalf("refused calls not refunded: used %d", st.UsedToday)
	}
	if r.m.Drops(r.src, DropVisionFailed) != 0 {
		t.Fatal("an auth refusal finalized images")
	}
	r.fv.status.Store(0)
	var raws []listing.Raw
	for poll := 0; poll < 3 && (poll == 0 || r.p.incomplete()); poll++ {
		raws = r.parse(t, neweggMsg(t))
	}
	if len(raws) != 1 || r.p.incomplete() {
		t.Fatalf("after the key works: %d raws, incomplete %v", len(raws), r.p.incomplete())
	}
}
