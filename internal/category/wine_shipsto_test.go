package category

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/shipping"
	"github.com/leftathome/nagus/internal/store"
)

func stampedLegalTo(t *testing.T, deps WineDeps) string {
	t.Helper()
	conn := &fakeWineConn{id: "imap:promo-winecom", raws: []listing.Raw{rawWine("a", "Example Cellars Syrah 2021", "", 2499, "USD")}}
	src, _ := shipping.NewSource("retailer", "US-CA")
	deps.Store = store.NewMemoryStore()
	ing, err := NewWineIngester(conn, src, deps)
	if err != nil {
		t.Fatal(err)
	}
	raws, err := ing.Connector.Fetch(context.Background())
	if err != nil || len(raws) != 1 {
		t.Fatalf("fetch %v %d", err, len(raws))
	}
	return raws[0].Aspects["ship_legal_to"]
}

// An operator declaration REPLACES the computed legal set for that source;
// without one, the default legal model applies unchanged.
func TestWineShipsToOverride(t *testing.T) {
	def := stampedLegalTo(t, WineDeps{})
	if strings.Contains(" "+def+" ", " US-WA ") {
		t.Fatalf("the default model now lets a CA retailer ship to WA: %q", def)
	}
	if want := strings.Join(shipping.DefaultRules().LegalDestinations(mustSource(t, "retailer", "US-CA")), " "); def != want {
		t.Fatalf("absent declaration changed the stamp: %q, want %q", def, want)
	}
	us := stampedLegalTo(t, WineDeps{ShipsTo: []string{"US"}})
	if !strings.Contains(" "+us+" ", " US-WA ") || strings.Contains(us, "CA-") {
		t.Fatalf("US declaration stamped %q", us)
	}
	if got := stampedLegalTo(t, WineDeps{ShipsTo: []string{"US-WA"}}); got != "US-WA" {
		t.Fatalf("US-WA declaration stamped %q", got)
	}
	// The override is logged at construction with what it adds.
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	stampedLegalTo(t, WineDeps{ShipsTo: []string{"US-WA"}, Logf: logf})
	if len(logged) == 0 || !strings.Contains(logged[0], "wineShipsTo") || !strings.Contains(logged[0], "US-WA") {
		t.Fatalf("override not logged: %q", logged)
	}
	src, _ := shipping.NewSource("retailer", "US-CA")
	for _, bad := range [][]string{{}, {"ZZ"}, {"US-ZZ"}, {"not a code"}} {
		if _, err := NewWineIngester(&fakeWineConn{id: "x"}, src, WineDeps{Store: store.NewMemoryStore(), ShipsTo: bad}); err == nil {
			t.Errorf("ShipsTo %q accepted", bad)
		}
	}
}
