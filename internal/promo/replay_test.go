package promo

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
)

// Newest-first processing must not let a later copy that FAILS
// authentication, carrying the genuine message's Message-ID, suppress or
// replace the genuine message's products; it never reaches the parser.
func TestNewestFirstReplayCannotOverride(t *testing.T) {
	r := newPRig(t, rigOpts{profile: "newegg", category: deal.CategoryHDD, perPoll: 1000}, "newegg.html")
	r.fv.set(1, answers{A: product("WD Red Plus 12TB NAS Internal Hard Drive", "219.99", "")})
	keys := dkimKeys{}
	html := fixture(t, "newegg.html")
	driveSrc, _ := url.Parse(r.cands[1].Src)
	evilTile := driveSrc.Path + "?v=evil"
	r.addImage(evilTile, 60)
	r.fv.set(60, answers{A: product("WD Red Plus 12TB NAS Internal Hard Drive", "1.00", "")})
	evilHTML := strings.Replace(html, r.cands[1].Src, "https://c2.neweggimages.com"+evilTile, 1)

	genuine := keys.sign(t, htmlMail("same-id@example.com", neweggSender, html), "newegg.com")
	replay := htmlMail("same-id@example.com", neweggSender, evilHTML) // newer, unsigned, same Message-ID
	host, port := imapServer(t, []string{genuine, replay})
	conn, err := imapmail.NewConnector(imapmail.Config{
		Name: "promo-newegg", Host: host, Port: port, TLS: "none", Username: testMailbox, Password: "pw",
		Senders: []string{neweggSender}, Parser: r.p, LookupTXT: keys.lookupTXT, NewestFirst: true,
		Now: func() time.Time { return testNow }, Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	src := &Source{Inner: conn, Parser: r.p}
	for poll := 0; poll < 2; poll++ {
		raws, err := src.Fetch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(raws) != 1 || raws[0].PriceCents != 21999 {
			t.Fatalf("poll %d: %+v", poll, raws)
		}
	}
	// Only the genuine message's images were read; the replay's tile never.
	if c := r.fv.calls.Load(); c != int64(2*len(r.cands)) {
		t.Fatalf("vision calls %d, want %d", c, 2*len(r.cands))
	}
}
