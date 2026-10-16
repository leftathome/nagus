package shipping

import (
	"strings"
	"testing"
)

func TestExpandDeclared(t *testing.T) {
	r := DefaultRules()
	us, err := r.ExpandDeclared([]string{"US"})
	if err != nil {
		t.Fatal(err)
	}
	if len(us) < 50 || us[0] != "US-AK" {
		t.Fatalf("US expands to %d codes starting %v", len(us), us[:1])
	}
	for _, c := range us {
		if !strings.HasPrefix(c, "US-") {
			t.Fatalf("US expanded to %q", c)
		}
	}
	got, err := r.ExpandDeclared([]string{"us-wa", "US-WA", "US-OR"})
	if err != nil || strings.Join(got, " ") != "US-OR US-WA" {
		t.Fatalf("subdivisions: %v %v", got, err)
	}
	for _, bad := range [][]string{nil, {}, {"USA"}, {"US-WA-X"}, {"ZZ"}, {"US-ZZ"}, {""}, {"FR-75"}, {"GB-ZZZ"}} {
		if _, err := r.ExpandDeclared(bad); err == nil {
			t.Errorf("ExpandDeclared(%q) accepted", bad)
		}
	}
}
