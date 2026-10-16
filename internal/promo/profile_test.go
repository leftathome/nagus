package promo

import (
	"strings"
	"testing"
)

func TestBuiltinProfiles(t *testing.T) {
	names := ProfileNames()
	if strings.Join(names, ",") != "newegg,totalwine,winecom" {
		t.Fatalf("profiles = %v", names)
	}
	for _, n := range names {
		p, err := Lookup(n)
		if err != nil {
			t.Fatal(err)
		}
		if p.Principal() != "retailer:"+n {
			t.Errorf("%s principal %q", n, p.Principal())
		}
	}
	if _, err := Lookup("nope"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestSenderAllowed(t *testing.T) {
	ne, _ := Lookup("newegg")
	tw, _ := Lookup("totalwine")
	for _, tc := range []struct {
		p    *Profile
		addr string
		want bool
	}{
		{ne, "deals@promo.newegg.com", true},
		{ne, "Deals@NEWEGG.COM", true},
		{ne, "deals@newegg.com.example.com", false},
		{ne, "deals@notnewegg.com", false},
		{ne, "no-at-sign", false},
		{tw, "news@email-totalwine.com", true},
		{tw, "news@e.totalwine.com", true},
		{tw, "news@totalwine.example.com", false},
	} {
		if got := tc.p.SenderAllowed(tc.addr); got != tc.want {
			t.Errorf("%s SenderAllowed(%q) = %v", tc.p.Name, tc.addr, got)
		}
	}
}

func TestNeweggURL(t *testing.T) {
	for _, tc := range []struct{ item, want string }{
		{"34-156-677", "https://www.newegg.com/p/N82E16834156677"},
		{"32-119-001", "https://www.newegg.com/p/N82E16832119001"},
		{"9SIC11UKGE3529", "https://www.newegg.com/Product/Product.aspx?Item=9SIC11UKGE3529"},
		{"9sib0gjm099877", "https://www.newegg.com/Product/Product.aspx?Item=9SIB0GJM099877"},
		{"1TS-000E-1DFS6", "https://www.newegg.com/p/1TS-000E-1DFS6"},
		{"0E6-003V-005T2", "https://www.newegg.com/p/0E6-003V-005T2"},
		{"", ""},
		{"34-156-67", ""},
		{"../../etc", ""},
		{"9SI", ""},
	} {
		got, ok := NeweggURL(tc.item)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("NeweggURL(%q) = %q,%v want %q", tc.item, got, ok, tc.want)
		}
	}
}

func TestLoadProfilesRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field":   `{"profiles":[{"name":"x","seller":"X","bogus":1}]}`,
		"no hosts":        `{"profiles":[{"name":"x","seller":"X","senderDomains":["x.com"],"canonical":"follow-link"}]}`,
		"bad canonical":   `{"profiles":[{"name":"x","seller":"X","senderDomains":["x.com"],"imageHosts":["i.x.com"],"productHosts":["www.x.com"],"canonical":"guess"}]}`,
		"item no group":   `{"profiles":[{"name":"x","seller":"X","senderDomains":["x.com"],"imageHosts":["i.x.com"],"productHosts":["www.x.com"],"canonical":"newegg-item","productAlt":"^item_"}]}`,
		"follow no path":  `{"profiles":[{"name":"x","seller":"X","senderDomains":["x.com"],"imageHosts":["i.x.com"],"productHosts":["www.x.com"],"linkHosts":["l.x.com"],"canonical":"follow-link"}]}`,
		"bad regex":       `{"profiles":[{"name":"x","seller":"X","senderDomains":["x.com"],"imageHosts":["i.x.com"],"productHosts":["www.x.com"],"linkHosts":["l.x.com"],"productPath":"(","canonical":"follow-link"}]}`,
		"duplicate names": `{"profiles":[{"name":"x","seller":"X","senderDomains":["x.com"],"imageHosts":["i.x.com"],"productHosts":["www.x.com"],"linkHosts":["l.x.com"],"productPath":"^/p$","canonical":"follow-link"},{"name":"x","seller":"X","senderDomains":["x.com"],"imageHosts":["i.x.com"],"productHosts":["www.x.com"],"linkHosts":["l.x.com"],"productPath":"^/p$","canonical":"follow-link"}]}`,
	} {
		if _, err := LoadProfiles([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
