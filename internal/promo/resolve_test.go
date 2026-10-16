package promo

import (
	"context"
	"net/http"
	"testing"
)

// testWineProfile is winecom with an extra redirect hop host.
func testWineProfile(t *testing.T) *Profile {
	t.Helper()
	ps, err := LoadProfiles([]byte(`{"profiles":[{"name":"shop","seller":"Example Shop","category":"wine",
		"senderDomains":["example.com"],"imageHosts":["img.example.com"],"linkHosts":["links.example.com"],
		"redirectHosts":["hop.example.com"],"productHosts":["www.example.com"],
		"productPath":"^/product/[a-z0-9-]+/[0-9]+$","canonical":"follow-link"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return ps["shop"]
}

func TestResolve(t *testing.T) {
	w := newWorld(t)
	loc := map[string]string{
		"/direct":    "https://www.example.com/product/a-wine-2022/123?utm_source=x&uid=SECRET#frag",
		"/viahop":    "https://hop.example.com/r1",
		"/relative":  "/r2",
		"/offsite":   "https://evil.example.org/product/a-wine-2022/123",
		"/downgrade": "http://www.example.com/product/a-wine-2022/123",
		"/loop":      "https://links.example.com/loop",
		"/ip":        "https://192.0.2.1/product/a-wine-2022/123",
		"/port":      "https://www.example.com:8443/product/a-wine-2022/123",
		"/userinfo":  "https://user@www.example.com/product/a-wine-2022/123",
		"/landing":   "https://www.example.com/landing/holiday",
		"/trailing":  "https://www.example.com/product/a-wine-2022/123/",
		"/dotdot":    "https://www.example.com/x/../product/a-wine-2022/123",
		"/many":      "https://links.example.com/many1",
		"/many1":     "https://links.example.com/many2",
		"/many2":     "https://links.example.com/many3",
		"/many3":     "https://links.example.com/many4",
		"/many4":     "https://links.example.com/many5",
		"/many5":     "https://links.example.com/many6",
		"/many6":     "https://www.example.com/product/a-wine-2022/123",
		"/fivehops":  "https://links.example.com/many3",
	}
	redir := func(rw http.ResponseWriter, r *http.Request) {
		if l, ok := loc[r.URL.Path]; ok {
			http.Redirect(rw, r, l, http.StatusFound)
			return
		}
		switch r.URL.Path {
		case "/busy":
			http.Error(rw, "busy", http.StatusBadGateway)
		case "/page":
			_, _ = rw.Write([]byte("<html>a landing page</html>"))
		default:
			http.NotFound(rw, r)
		}
	}
	w.handle("links.example.com", redir)
	w.handle("hop.example.com", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "https://www.example.com/product/b-wine/456?ref=hop", http.StatusMovedPermanently)
	})
	w.handle("www.example.com", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landing/holiday" {
			_, _ = rw.Write([]byte("landing"))
			return
		}
		http.NotFound(rw, r)
	})
	r := &Resolver{Client: w.client()}
	p := testWineProfile(t)
	ctx := context.Background()
	for _, tc := range []struct {
		href, want, drop string
		temp             bool
	}{
		{href: "https://links.example.com/direct?t=TOKEN", want: "https://www.example.com/product/a-wine-2022/123"},
		{href: "http://links.example.com/direct", want: "https://www.example.com/product/a-wine-2022/123"},
		{href: "https://links.example.com/viahop", want: "https://www.example.com/product/b-wine/456"},
		{href: "https://links.example.com/relative", drop: DropLinkUnresolved},
		{href: "https://links.example.com/trailing", want: "https://www.example.com/product/a-wine-2022/123"},
		{href: "https://links.example.com/dotdot", want: "https://www.example.com/product/a-wine-2022/123"},
		{href: "https://links.example.com/offsite", drop: DropLinkOffsite},
		{href: "https://links.example.com/downgrade", drop: DropHTTPDowngrade},
		{href: "https://links.example.com/loop", drop: DropRedirectLoop},
		{href: "https://links.example.com/ip", drop: DropLinkOffsite},
		{href: "https://links.example.com/port", drop: DropLinkOffsite},
		{href: "https://links.example.com/userinfo", drop: DropLinkOffsite},
		{href: "https://links.example.com/landing", drop: DropLinkUnresolved},
		{href: "https://links.example.com/page", drop: DropLinkUnresolved},
		{href: "https://links.example.com/many", drop: DropTooManyHops},
		{href: "https://links.example.com/fivehops", want: "https://www.example.com/product/a-wine-2022/123"},
		{href: "https://links.example.com/busy", drop: DropLinkFailed, temp: true},
		{href: "https://evil.example.org/direct", drop: DropLinkOffsite},
		{href: "", drop: DropMissingLinkHref},
	} {
		got, drop, temp := r.Resolve(ctx, p, Candidate{Href: tc.href})
		if got != tc.want || drop != tc.drop || temp != tc.temp {
			t.Errorf("Resolve(%s) = %q,%q,%v want %q,%q,%v", tc.href, got, drop, temp, tc.want, tc.drop, tc.temp)
		}
	}
	// The product page itself is never requested, and no hostile host is.
	if n := w.count("www.example.com"); n != 1 { // only the /landing page
		t.Errorf("product host requested %d times, want 1 (the landing page only)", n)
	}
	if n := w.count("evil.example.org") + w.count("192.0.2.1"); n != 0 {
		t.Errorf("hostile hosts contacted %d times", n)
	}
}

func TestResolveNeweggNeverFollows(t *testing.T) {
	w := newWorld(t)
	r := &Resolver{Client: w.client()}
	ne := mustProfile(t, "newegg")
	got, drop, _ := r.Resolve(context.Background(), ne, Candidate{Item: "34-156-677", Href: "https://l.promo.newegg.com/rts/go2.aspx?t=1"})
	if got != "https://www.newegg.com/p/N82E16834156677" || drop != "" {
		t.Fatalf("got %q %q", got, drop)
	}
	if _, drop, _ := r.Resolve(context.Background(), ne, Candidate{Item: "bogus"}); drop != DropBadItemNumber {
		t.Fatalf("bogus item: %q", drop)
	}
	if w.count("l.promo.newegg.com") != 0 {
		t.Fatal("the tracked link was followed")
	}
}
