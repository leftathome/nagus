package main

import (
	"bytes"
	"strings"
	"testing"
)

// csvRows builds an LWIN-shaped CSV: n rows per (status, type, country,
// region, sub-region) spec.
func csvRows(specs ...spec) string {
	var b strings.Builder
	b.WriteString("LWIN,STATUS,DISPLAY_NAME,COUNTRY,REGION,SUB_REGION,TYPE\n")
	for _, s := range specs {
		for i := 0; i < s.n; i++ {
			b.WriteString("1," + s.status + ",x," + s.country + "," + s.region + "," + s.subRegion + "," + s.typ + "\n")
		}
	}
	return b.String()
}

type spec = struct {
	n                                       int
	status, typ, country, region, subRegion string
}

func TestBuild(t *testing.T) {
	in := csvRows(
		spec{25, "Live", "Wine", "Italy", "Piedmont", "Barolo"},
		spec{25, "Live", "Fortified Wine", "Spain", "Andalucia", "Jerez"},
		spec{25, "Live", "Wine", "France", "Rhone", "Ch\u00e2teauneuf-du-Pape"},
		spec{25, "Live", "Wine", "United States", "California", "Napa Valley"},
		spec{19, "Live", "Wine", "France", "Loire", "Tiny"},        // under the threshold
		spec{30, "Delisted", "Wine", "France", "Loire", "Gone"},    // not Live
		spec{30, "Live", "Spirit", "France", "Cognac", "Grande"},   // not wine
		spec{30, "Live", "Wine", "Portugal", "Porto", "NA"},        // excluded; NA skipped
		spec{30, "Live", "Wine", "France", "Loire", "St. Nicolas"}, // abbreviation spelled out
	)
	got, err := Build(strings.NewReader(in), 20)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{ // name -> broad
		"barolo": false, "piedmont": true, "jerez": false, "andalucia": true,
		"chateauneuf du pape": false, "rhone": true, "napa valley": true,
		"loire": true, "saint nicolas": false,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d names %+v, want %d", len(got), got, len(want))
	}
	for _, e := range got {
		b, ok := want[e.Name]
		if !ok || b != e.Broad {
			t.Errorf("%+v: want present=%v broad=%v", e, ok, b)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Name >= got[i].Name {
			t.Fatalf("not sorted: %q then %q", got[i-1].Name, got[i].Name)
		}
	}
}

func TestBuildMissingColumn(t *testing.T) {
	if _, err := Build(strings.NewReader("LWIN,STATUS\n"), 20); err == nil {
		t.Fatal("want an error for a missing column")
	}
}

func TestRender(t *testing.T) {
	var b bytes.Buffer
	if err := Render(&b, []Entry{{Name: "barolo", Wines: 1606}, {Name: "napa valley", Wines: 3773, Broad: true}}, 20); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"DO NOT EDIT", "Creative Commons Attribution 4.0", "liv-ex.com/lwin-creative-commons-licence", "fewer than 20 wines", `"barolo":      false, // 1606`, `"napa valley": true,  // 3773`} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output lacks %q:\n%s", want, out)
		}
	}
	if err := Render(&b, []Entry{{Name: "caf\u00e9"}}, 20); err == nil {
		t.Error("want an error for a non-ASCII name")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Ch\u00e2teauneuf-du-Pape": "chateauneuf du pape",
		"Hawke's Bay":              "hawke s bay",
		"Lincoln\u00a0Lakeshore":   "lincoln lakeshore",
		"Mt. Veeder":               "mount veeder",
		"Ste. Croix":               "sainte croix",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
