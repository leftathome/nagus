package shipping

import (
	"strings"
	"testing"
)

// --- Channel / Source validation ---

func TestChannelValid(t *testing.T) {
	if !ChannelWineryDirect.Valid() || !ChannelRetailer.Valid() {
		t.Fatalf("declared channels must be valid")
	}
	for _, c := range []Channel{"", "wa_retailer", "out_of_state_retailer", "mail_fraud"} {
		if c.Valid() {
			t.Errorf("channel %q must not be valid", c)
		}
	}
}

func TestSourceValidate(t *testing.T) {
	if err := (Source{Channel: ChannelRetailer, State: "WA"}).Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := (Source{Channel: "bogus", State: "WA"}).Validate(); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Errorf("unknown channel should error naming the channel, got %v", err)
	}
	if err := (Source{Channel: ChannelRetailer, State: "Washington"}).Validate(); err == nil || !strings.Contains(err.Error(), "state") {
		t.Errorf("non-USPS state should error naming the state, got %v", err)
	}
	// Normalization: lowercase codes are fine.
	if err := (Source{Channel: ChannelRetailer, State: "wa"}).Validate(); err != nil {
		t.Errorf("lowercase state code should normalize, got %v", err)
	}
}

func TestIsState(t *testing.T) {
	for _, s := range []string{"WA", "wa", " ca ", "DC"} {
		if !IsState(s) {
			t.Errorf("IsState(%q) should be true", s)
		}
	}
	for _, s := range []string{"", "Washington", "XX", "PR"} {
		if IsState(s) {
			t.Errorf("IsState(%q) should be false", s)
		}
	}
}

// --- Default table shape ---

func TestDefaultRulesCoversAllStatesPlusDC(t *testing.T) {
	r := DefaultRules()
	if len(r.Destinations) != 51 {
		t.Fatalf("expected 50 states + DC = 51 destinations, got %d", len(r.Destinations))
	}
}

func TestDefaultRulesWineryDirectBaseline(t *testing.T) {
	r := DefaultRules()
	winery := Source{Channel: ChannelWineryDirect, State: "CA"}
	// Legal into WA (the original motivating case) and CA.
	for _, dest := range []string{"WA", "CA", "FL", "NY"} {
		if !r.Legal(winery, dest) {
			t.Errorf("winery-direct should be legal into %s by default", dest)
		}
	}
	// The two prohibition states.
	for _, dest := range []string{"MS", "UT"} {
		if r.Legal(winery, dest) {
			t.Errorf("winery-direct must be illegal into %s by default", dest)
		}
	}
}

func TestDefaultRulesRetailerBaseline(t *testing.T) {
	r := DefaultRules()

	// In-state retailer: legal within its own state.
	waShop := Source{Channel: ChannelRetailer, State: "WA"}
	if !r.Legal(waShop, "WA") {
		t.Errorf("a WA retailer should be legal into WA")
	}
	// ...but out-of-state into WA is the motivating prohibition (SB 5007).
	caShop := Source{Channel: ChannelRetailer, State: "CA"}
	if r.Legal(caShop, "WA") {
		t.Errorf("an out-of-state retailer must be illegal into WA by default")
	}
	// The same CA shop IS legal into CA (its own state) and into OR
	// (out-of-state retailer permitted there).
	if !r.Legal(caShop, "CA") || !r.Legal(caShop, "OR") {
		t.Errorf("CA retailer should be legal into CA and OR")
	}
	// A WA shop shipping into FL: FL does not permit out-of-state retailers
	// in the baseline.
	if r.Legal(waShop, "FL") {
		t.Errorf("out-of-state retailer into FL must be illegal by default")
	}
}

// --- Fail-closed ---

func TestLegalFailsClosed(t *testing.T) {
	r := DefaultRules()
	cases := []struct {
		name string
		src  Source
		dest string
	}{
		{"unknown destination", Source{Channel: ChannelRetailer, State: "WA"}, "XX"},
		{"empty destination", Source{Channel: ChannelRetailer, State: "WA"}, ""},
		{"unknown channel", Source{Channel: "bogus", State: "WA"}, "WA"},
		{"retailer with invalid home state", Source{Channel: ChannelRetailer, State: "??"}, "CA"},
	}
	for _, c := range cases {
		if r.Legal(c.src, c.dest) {
			t.Errorf("%s: must fail closed", c.name)
		}
	}
	// An empty Rules table is illegal everywhere.
	empty := Rules{}
	if empty.Legal(Source{Channel: ChannelWineryDirect, State: "CA"}, "CA") {
		t.Errorf("empty rules must be illegal everywhere")
	}
}

func TestLegalNormalizesCase(t *testing.T) {
	r := DefaultRules()
	src := Source{Channel: ChannelRetailer, State: "wa"}
	if !r.Legal(src, "wa") {
		t.Errorf("lowercase source and destination codes should normalize")
	}
}

// --- LegalDestinations ---

func TestLegalDestinationsSortedAndConsistent(t *testing.T) {
	r := DefaultRules()
	winery := Source{Channel: ChannelWineryDirect, State: "WA"}
	dests := r.LegalDestinations(winery)
	if len(dests) != 49 { // 51 - MS - UT
		t.Fatalf("winery-direct should reach 49 destinations, got %d", len(dests))
	}
	for i := 1; i < len(dests); i++ {
		if dests[i-1] >= dests[i] {
			t.Fatalf("destinations must be sorted: %v", dests)
		}
	}
	// Every listed destination must agree with Legal (the tagger stamps
	// this set; disagreement would corrupt the filter).
	for _, d := range dests {
		if !r.Legal(winery, d) {
			t.Errorf("LegalDestinations includes %s but Legal disagrees", d)
		}
	}

	// An in-state retailer in a no-out-of-state destination reaches its own
	// state plus the permitted out-of-state list.
	waShop := Source{Channel: ChannelRetailer, State: "WA"}
	got := map[string]bool{}
	for _, d := range r.LegalDestinations(waShop) {
		got[d] = true
	}
	if !got["WA"] {
		t.Errorf("a WA retailer must reach WA")
	}
	if got["FL"] {
		t.Errorf("a WA retailer must not reach FL by default")
	}
	if !got["CA"] {
		t.Errorf("a WA retailer should reach CA (out-of-state retailer permitted there)")
	}
}

// --- LoadRules / Override ---

func TestLoadRulesAndOverride(t *testing.T) {
	// Suppose WA passes an SB-5007-style law: the operator overrides one
	// destination without restating the other 50.
	doc := `{"destinations": {"wa": {"wineryDirect": true, "inStateRetailer": true, "outOfStateRetailer": true}}}`
	loaded, err := LoadRules(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := DefaultRules().Override(loaded)

	caShop := Source{Channel: ChannelRetailer, State: "CA"}
	if !r.Legal(caShop, "WA") {
		t.Errorf("override should make out-of-state retailer legal into WA")
	}
	// Untouched destinations keep the baseline.
	if r.Legal(caShop, "FL") {
		t.Errorf("override must not disturb other destinations")
	}
	if len(r.Destinations) != 51 {
		t.Errorf("override must keep the full table, got %d", len(r.Destinations))
	}
}

func TestLoadRulesRejectsUnknownDestination(t *testing.T) {
	doc := `{"destinations": {"Wash": {"wineryDirect": true}}}`
	if _, err := LoadRules(strings.NewReader(doc)); err == nil {
		t.Fatalf("an unknown destination code must be a load error, not a silent unreachable entry")
	}
}

func TestLoadRulesRejectsUnknownFields(t *testing.T) {
	doc := `{"destinations": {"WA": {"wineryDirect": true, "retailShipping": true}}}`
	if _, err := LoadRules(strings.NewReader(doc)); err == nil {
		t.Fatalf("a misspelled policy field must be a load error (it would silently fail closed otherwise)")
	}
}

func TestOverrideDoesNotMutateBase(t *testing.T) {
	base := DefaultRules()
	before := base.Destinations["WA"]
	loaded := Rules{Destinations: map[string]Policy{"WA": {OutOfStateRetailer: true}}}
	_ = base.Override(loaded)
	if base.Destinations["WA"] != before {
		t.Fatalf("Override must return a copy, not mutate the base")
	}
}
