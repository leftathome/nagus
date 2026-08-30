package shipping

import (
	"strings"
	"testing"
)

func mustSource(t *testing.T, channel, origin string) Source {
	t.Helper()
	s, err := NewSource(channel, origin)
	if err != nil {
		t.Fatalf("NewSource(%q,%q): %v", channel, origin, err)
	}
	return s
}

// --- Jurisdiction ---

func TestParseJurisdiction(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"US-WA", "US-WA"},
		{"us-wa", "US-WA"},
		{" fr ", "FR"},
		{"CA-ON", "CA-ON"},
		{"AU", "AU"},
		{"FR-75", "FR-75"}, // numeric subdivisions are valid ISO 3166-2
	}
	for _, c := range cases {
		j, err := ParseJurisdiction(c.in)
		if err != nil {
			t.Errorf("ParseJurisdiction(%q): unexpected error %v", c.in, err)
			continue
		}
		if j.Code() != c.want {
			t.Errorf("ParseJurisdiction(%q).Code() = %q, want %q", c.in, j.Code(), c.want)
		}
	}
}

func TestParseJurisdictionRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "   ", "USA", "U", "Washington", "US-WASH", "US-WA-X", "12", "US_WA"} {
		if _, err := ParseJurisdiction(in); err == nil {
			t.Errorf("ParseJurisdiction(%q) should have failed", in)
		}
	}
}

func TestNormJurisdiction(t *testing.T) {
	if got, ok := NormJurisdiction("us-wa"); !ok || got != "US-WA" {
		t.Errorf("NormJurisdiction(us-wa) = %q,%v", got, ok)
	}
	if _, ok := NormJurisdiction("Cascadia"); ok {
		t.Errorf("NormJurisdiction should reject a non-code")
	}
}

// --- Source ---

func TestNewSourceValidates(t *testing.T) {
	if _, err := NewSource("RETAILER", "us-wa"); err != nil {
		t.Errorf("channel and origin should normalize: %v", err)
	}
	if _, err := NewSource("wa_retailer", "US-WA"); err == nil {
		t.Errorf("the old US-only channel vocabulary must not be accepted")
	}
	if _, err := NewSource("retailer", "Cascadia"); err == nil {
		t.Errorf("an unparseable origin must be rejected")
	}
}

func TestSourceValidate(t *testing.T) {
	if err := (Source{Channel: ChannelProducer, Origin: Jurisdiction{Country: "FR"}}).Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := (Source{Channel: "bogus", Origin: Jurisdiction{Country: "FR"}}).Validate(); err == nil {
		t.Errorf("unknown channel must be rejected")
	}
	if err := (Source{Channel: ChannelProducer}).Validate(); err == nil {
		t.Errorf("missing origin must be rejected")
	}
}

// --- Relate ---

func TestRelate(t *testing.T) {
	r := DefaultRules()
	j := func(s string) Jurisdiction {
		out, err := ParseJurisdiction(s)
		if err != nil {
			t.Fatalf("bad test jurisdiction %q: %v", s, err)
		}
		return out
	}
	cases := []struct {
		origin, dest string
		want         Relation
	}{
		{"US-WA", "US-WA", RelSameSubdivision},
		{"US-CA", "US-WA", RelSameCountry},
		{"FR", "FR", RelSameCountry},    // country-level: no subdivision to prove
		{"US", "US-WA", RelSameCountry}, // unstated origin subdivision cannot prove in-state
		{"FR", "ES", RelSameBloc},
		{"FR", "US-WA", RelForeign},
		{"US-WA", "FR", RelForeign},
		{"AU", "NZ", RelForeign}, // no modeled bloc between them
	}
	for _, c := range cases {
		if got := r.Relate(j(c.origin), j(c.dest)); got != c.want {
			t.Errorf("Relate(%s -> %s) = %s, want %s", c.origin, c.dest, got, c.want)
		}
	}
}

func TestRelateSameSubdivisionNeedsBothSides(t *testing.T) {
	// The asymmetric case matters: a source declared only as "US" must not
	// be credited with being in-state anywhere, or an out-of-state retailer
	// would sneak past a state that only permits in-state ones.
	r := DefaultRules()
	loose := mustSource(t, "retailer", "US")
	if r.Legal(loose, "US-WA") {
		t.Errorf("a retailer with an unstated subdivision must not count as in-state in WA")
	}
}

// --- Legal: the US cases that motivated the layer ---

func TestLegalUSRetailerInStateVsOutOfState(t *testing.T) {
	r := DefaultRules()
	waShop := mustSource(t, "retailer", "US-WA")
	caShop := mustSource(t, "retailer", "US-CA")

	if !r.Legal(waShop, "US-WA") {
		t.Errorf("a WA retailer should reach WA")
	}
	if r.Legal(caShop, "US-WA") {
		t.Errorf("an out-of-state retailer must not reach WA (SB 5007 died in committee)")
	}
	// The same CA shop reaches CA (in-state) and OR (inbound-permitted).
	if !r.Legal(caShop, "US-CA") || !r.Legal(caShop, "US-OR") {
		t.Errorf("CA retailer should reach CA and OR")
	}
	// And not FL, which is not on the inbound list.
	if r.Legal(waShop, "US-FL") {
		t.Errorf("out-of-state retailer into FL must be closed by default")
	}
}

func TestLegalUSProducer(t *testing.T) {
	r := DefaultRules()
	winery := mustSource(t, "producer", "US-CA")
	for _, dest := range []string{"US-WA", "US-CA", "US-FL", "US-NY"} {
		if !r.Legal(winery, dest) {
			t.Errorf("winery-direct should reach %s by default", dest)
		}
	}
	for _, dest := range []string{"US-MS", "US-UT", "US-DE", "US-RI"} {
		if r.Legal(winery, dest) {
			t.Errorf("winery-direct must be closed into %s", dest)
		}
	}
}

// --- Legal: international ---

func TestLegalEUIntraBloc(t *testing.T) {
	r := DefaultRules()
	frWinery := mustSource(t, "producer", "FR")
	frShop := mustSource(t, "retailer", "FR")

	// Intra-EU distance selling: both channels reach other member states.
	for _, dest := range []string{"ES", "IT", "DE", "NL", "FR"} {
		if !r.Legal(frWinery, dest) {
			t.Errorf("FR winery should reach %s", dest)
		}
		if !r.Legal(frShop, dest) {
			t.Errorf("FR retailer should reach %s", dest)
		}
	}
	// But NOT into the US: imports must clear a licensed importer.
	if r.Legal(frWinery, "US-WA") || r.Legal(frShop, "US-CA") {
		t.Errorf("an EU seller must not reach a US consumer by default")
	}
	// Nor into Canada, whose provinces route imports through the boards.
	if r.Legal(frWinery, "CA-ON") {
		t.Errorf("an EU winery must not reach an Ontario consumer by default")
	}
}

func TestLegalNordicMonopolyExceptions(t *testing.T) {
	r := DefaultRules()
	seShop := mustSource(t, "retailer", "SE")
	frShop := mustSource(t, "retailer", "FR")

	// Domestic retail is the monopoly's business...
	if r.Legal(seShop, "SE") {
		t.Errorf("a domestic SE retailer should be closed under the monopoly model")
	}
	// ...but intra-bloc private importation stays open.
	if !r.Legal(frShop, "SE") {
		t.Errorf("an FR retailer should reach an SE consumer (intra-EU distance selling)")
	}
	if !r.Legal(frShop, "FI") {
		t.Errorf("an FR retailer should reach an FI consumer")
	}
}

func TestLegalCanadaInterprovincial(t *testing.T) {
	r := DefaultRules()
	bcWinery := mustSource(t, "producer", "CA-BC")
	onShop := mustSource(t, "retailer", "CA-ON")

	if !r.Legal(bcWinery, "CA-BC") {
		t.Errorf("a BC winery should reach BC")
	}
	if !r.Legal(bcWinery, "CA-MB") || !r.Legal(bcWinery, "CA-NS") {
		t.Errorf("a BC winery should reach the provinces that admit interprovincial winery-direct")
	}
	if r.Legal(bcWinery, "CA-ON") || r.Legal(bcWinery, "CA-QC") {
		t.Errorf("monopoly provinces must not admit interprovincial winery-direct by default")
	}
	if !r.Legal(onShop, "CA-ON") {
		t.Errorf("an ON retailer should reach ON")
	}
	if r.Legal(onShop, "CA-BC") {
		t.Errorf("interprovincial retail shipping must be closed by default")
	}
}

func TestLegalOtherMarketsAreDomesticOnly(t *testing.T) {
	r := DefaultRules()
	auShop := mustSource(t, "retailer", "AU")
	nzShop := mustSource(t, "retailer", "NZ")

	if !r.Legal(auShop, "AU") {
		t.Errorf("an AU retailer should reach AU")
	}
	// Subdivision destinations fall back to the country entry.
	if !r.Legal(auShop, "AU-NSW") {
		t.Errorf("AU-NSW should resolve through the AU country policy")
	}
	if r.Legal(nzShop, "AU") || r.Legal(auShop, "NZ") {
		t.Errorf("cross-border DTC is not modeled for AU/NZ by default")
	}
}

func TestDefaultRulesCoverage(t *testing.T) {
	r := DefaultRules()
	// The four regions are all present.
	for _, code := range []string{"US-WA", "US-DC", "CA-QC", "FR", "ES", "IT", "AU", "NZ", "ZA", "CL", "AR", "JP", "GB"} {
		if _, ok := r.Destinations[code]; !ok {
			t.Errorf("default table should model %s", code)
		}
	}
	if got := len(r.Destinations); got < 100 {
		t.Errorf("expected ~110 modeled destinations, got %d", got)
	}
	// The EU bloc must be populated, or RelSameBloc could never fire.
	if len(r.Blocs["EU"]) != 27 {
		t.Errorf("EU bloc should list 27 members, got %d", len(r.Blocs["EU"]))
	}
}

// --- Fail closed ---

func TestLegalFailsClosed(t *testing.T) {
	r := DefaultRules()
	cases := []struct {
		name string
		src  Source
		dest string
	}{
		{"unparseable destination", mustSource(t, "retailer", "US-WA"), "Cascadia"},
		{"empty destination", mustSource(t, "retailer", "US-WA"), ""},
		{"unmodeled destination", mustSource(t, "retailer", "US-WA"), "NO"},
		{"unmodeled subdivision falls back to unmodeled country", mustSource(t, "retailer", "US-WA"), "NO-03"},
		{"unknown channel", Source{Channel: "bogus", Origin: Jurisdiction{Country: "US", Subdivision: "WA"}}, "US-WA"},
		{"source with no origin", Source{Channel: ChannelRetailer}, "US-WA"},
	}
	for _, c := range cases {
		if r.Legal(c.src, c.dest) {
			t.Errorf("%s: must fail closed", c.name)
		}
	}

	// An empty table is illegal everywhere.
	if (Rules{}).Legal(mustSource(t, "producer", "FR"), "FR") {
		t.Errorf("empty rules must be illegal everywhere")
	}
}

func TestUnmodeledIsDistinctFromProhibited(t *testing.T) {
	r := DefaultRules()
	// Norway is OMITTED (unmodeled), Utah is present-but-closed. Both are
	// illegal, but only one is claimed as modeled -- config paths use
	// Modeled to tell an operator which is which.
	if r.Modeled("NO") {
		t.Errorf("NO should be unmodeled in the default table")
	}
	if !r.Modeled("US-UT") {
		t.Errorf("US-UT should be modeled (and closed), not unmodeled")
	}
	if r.Legal(mustSource(t, "producer", "US-CA"), "US-UT") {
		t.Errorf("US-UT must still be illegal")
	}
	if r.Modeled("Cascadia") {
		t.Errorf("an unparseable destination is never modeled")
	}
	// A subdivision of a modeled country is modeled via fallback.
	if !r.Modeled("AU-VIC") {
		t.Errorf("AU-VIC should resolve through AU")
	}
}

// --- LegalDestinations ---

func TestLegalDestinationsAgreesWithLegal(t *testing.T) {
	r := DefaultRules()
	for _, src := range []Source{
		mustSource(t, "retailer", "US-WA"),
		mustSource(t, "producer", "US-CA"),
		mustSource(t, "producer", "FR"),
		mustSource(t, "retailer", "CA-ON"),
	} {
		dests := r.LegalDestinations(src)
		for i := 1; i < len(dests); i++ {
			if dests[i-1] >= dests[i] {
				t.Fatalf("%v: destinations must be sorted and unique: %v", src, dests)
			}
		}
		got := map[string]bool{}
		for _, d := range dests {
			got[d] = true
			if !r.Legal(src, d) {
				t.Errorf("%v: LegalDestinations includes %s but Legal disagrees", src, d)
			}
		}
		for code := range r.Destinations {
			if r.Legal(src, code) && !got[code] {
				t.Errorf("%v: Legal allows %s but LegalDestinations omits it", src, code)
			}
		}
	}
}

func TestLegalDestinationsShape(t *testing.T) {
	r := DefaultRules()

	// A French winery reaches the EU-27 and nothing else by default.
	frDests := r.LegalDestinations(mustSource(t, "producer", "FR"))
	if len(frDests) != 27 {
		t.Errorf("an FR winery should reach the 27 EU members, got %d: %v", len(frDests), frDests)
	}
	for _, d := range frDests {
		if strings.HasPrefix(d, "US-") || strings.HasPrefix(d, "CA-") {
			t.Errorf("an FR winery must not reach %s", d)
		}
	}

	// A WA retailer reaches WA plus the inbound-permitting states only.
	waDests := r.LegalDestinations(mustSource(t, "retailer", "US-WA"))
	want := len(usRetailerInbound) + 1 // the inbound list plus its own state
	if len(waDests) != want {
		t.Errorf("a WA retailer should reach %d destinations, got %d: %v", want, len(waDests), waDests)
	}
}

// --- LoadRules / Override ---

func TestLoadRulesAndOverride(t *testing.T) {
	// Suppose WA passes an SB-5007-style law: one destination overridden,
	// the other ~110 untouched.
	doc := `{"destinations": {"us-wa": {
		"producer": {"sameSubdivision": true, "sameCountry": true},
		"retailer": {"sameSubdivision": true, "sameCountry": true}}}}`
	loaded, err := LoadRules(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := DefaultRules().Override(loaded)

	caShop := mustSource(t, "retailer", "US-CA")
	if !r.Legal(caShop, "US-WA") {
		t.Errorf("the override should open WA to out-of-state retailers")
	}
	if r.Legal(caShop, "US-FL") {
		t.Errorf("the override must not disturb other destinations")
	}
	if len(r.Destinations) != len(DefaultRules().Destinations) {
		t.Errorf("override should keep the full table")
	}
}

func TestLoadRulesOverrideOpensForeignImports(t *testing.T) {
	// The documented personal-import case: an operator in Australia decides
	// third-country retailers may ship in.
	doc := `{"destinations": {"AU": {
		"producer": {"sameSubdivision": true, "sameCountry": true, "foreign": true},
		"retailer": {"sameSubdivision": true, "sameCountry": true, "foreign": true}}}}`
	loaded, err := LoadRules(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := DefaultRules().Override(loaded)

	frShop := mustSource(t, "retailer", "FR")
	if !r.Legal(frShop, "AU") {
		t.Errorf("with foreign enabled, an FR retailer should reach AU")
	}
	if !r.Legal(frShop, "AU-NSW") {
		t.Errorf("the AU override should apply to subdivisions via fallback")
	}
	// Other destinations keep the conservative default.
	if r.Legal(frShop, "NZ") {
		t.Errorf("overriding AU must not open NZ")
	}
}

func TestLoadRulesBlocOverride(t *testing.T) {
	// A bloc can be redefined as data too -- e.g. modeling a customs union
	// the baseline does not carry.
	doc := `{"destinations": {"AU": {"retailer": {"sameCountry": true, "sameBloc": true}},
	                          "NZ": {"retailer": {"sameCountry": true, "sameBloc": true}}},
	         "blocs": {"ANZ": ["au", "nz"]}}`
	loaded, err := LoadRules(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := DefaultRules().Override(loaded)

	if got := r.Blocs["ANZ"]; len(got) != 2 || got[0] != "AU" || got[1] != "NZ" {
		t.Fatalf("bloc members should be canonicalized and sorted, got %v", got)
	}
	nzShop := mustSource(t, "retailer", "NZ")
	if !r.Legal(nzShop, "AU") {
		t.Errorf("an NZ retailer should reach AU once they share a modeled bloc")
	}
	// The EU bloc survives the merge.
	if len(r.Blocs["EU"]) != 27 {
		t.Errorf("overriding one bloc must not drop the others")
	}
}

func TestLoadRulesRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown destination code":   `{"destinations": {"Wash": {"producer": {}}}}`,
		"subdivision as bloc member": `{"blocs": {"EU": ["US-WA"]}}`,
		"unknown bloc member code":   `{"blocs": {"EU": ["France"]}}`,
		"misspelled policy field":    `{"destinations": {"US-WA": {"producer": {"sameStateish": true}}}}`,
		"misspelled channel":         `{"destinations": {"US-WA": {"vintner": {"sameCountry": true}}}}`,
		"not json":                   `nope`,
	}
	for name, doc := range cases {
		if _, err := LoadRules(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected a load error", name)
		}
	}
}

func TestOverrideDoesNotMutateInputs(t *testing.T) {
	base := DefaultRules()
	before := base.Destinations["US-WA"]
	beforeBloc := len(base.Blocs["EU"])

	o := Rules{
		Destinations: map[string]Policy{"US-WA": {Retailer: ChannelPolicy{SameCountry: true}}},
		Blocs:        map[string][]string{"EU": {"FR"}},
	}
	merged := base.Override(o)

	if base.Destinations["US-WA"] != before {
		t.Errorf("Override must not mutate the base table")
	}
	if len(base.Blocs["EU"]) != beforeBloc {
		t.Errorf("Override must not mutate the base blocs")
	}
	// And the merged copy's slices must not alias the override's.
	merged.Blocs["EU"][0] = "ZZ"
	if o.Blocs["EU"][0] != "FR" {
		t.Errorf("Override must copy bloc slices, not alias them")
	}
}
