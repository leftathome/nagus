package lwin

import (
	"strings"
	"testing"
)

func sampleDB() *DB {
	return NewDB([]Record{
		{LWIN7: "1012361", Producer: "Chateau Margaux", Wine: "", Country: "France", Region: "Bordeaux", Colour: "red"},
		{LWIN7: "1012362", Producer: "Chateau Margaux", Wine: "Pavillon Rouge", Country: "France", Region: "Bordeaux", Colour: "red"},
		{LWIN7: "1014086", Producer: "Domaine de la Romanee-Conti", Wine: "La Tache", Country: "France", Region: "Burgundy", Colour: "red"},
		{LWIN7: "1101245", Producer: "Leonetti Cellar", Wine: "Cabernet Sauvignon", Country: "USA", Region: "Walla Walla", Colour: "red"},
		{LWIN7: "1102900", Producer: "Quilceda Creek", Wine: "Cabernet Sauvignon", Country: "USA", Region: "Columbia Valley", Colour: "red"},
		{LWIN7: "1200001", Producer: "Ridge", Wine: "Monte Bello", Country: "USA", Region: "Santa Cruz Mountains", Colour: "red"},
	})
}

// --- Record ---

func TestLWIN11_VintageAndNV(t *testing.T) {
	r := Record{LWIN7: "1012361"}
	if got := r.LWIN11(2015); got != "10123612015" {
		t.Fatalf("expected 10123612015, got %s", got)
	}
	if got := r.LWIN11(0); got != "10123611000" {
		t.Fatalf("NV should use the 1000 vintage segment, got %s", got)
	}
}

// --- normalizeName ---

func TestNormalizeName_AccentFoldAndAliases(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Château Margaux", "chateau margaux"},
		{"Ch. Margaux", "chateau margaux"},
		{"Dom. de la Romanée-Conti", "domaine de la romanee conti"},
		{"LEONETTI Cellar", "leonetti cellar"},
	}
	for _, c := range cases {
		if got := normalizeName(c.in); got != c.want {
			t.Errorf("normalizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- LoadCSV ---

func TestLoadCSV_LivExHeader(t *testing.T) {
	csvData := "LWIN,STATUS,PRODUCER_NAME,WINE,COUNTRY,REGION,COLOUR\n" +
		"1012361,Live,Chateau Margaux,,France,Bordeaux,Red\n" +
		"1101245,Live,Leonetti Cellar,Cabernet Sauvignon,USA,Walla Walla,Red\n" +
		",Live,No LWIN,,,,\n" // skipped: no LWIN7
	db, err := LoadCSV(strings.NewReader(csvData))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if db.Len() != 2 {
		t.Fatalf("expected 2 usable records, got %d", db.Len())
	}
	res := Resolver{DB: db}.Resolve(Query{Name: "Chateau Margaux 2015"})
	if res.Route != RouteAuto || res.Best.Record.LWIN7 != "1012361" {
		t.Fatalf("expected auto-match to 1012361, got %+v", res)
	}
	if res.Best.Record.Colour != "red" {
		t.Errorf("colour should load lowercased, got %q", res.Best.Record.Colour)
	}
}

func TestLoadCSV_MissingLWINColumn(t *testing.T) {
	if _, err := LoadCSV(strings.NewReader("A,B\n1,2\n")); err == nil {
		t.Fatalf("a header without LWIN should be an error")
	}
}

// --- Resolve ---

func TestResolve_ExactRetailTitleAutoMatches(t *testing.T) {
	r := Resolver{DB: sampleDB()}
	res := r.Resolve(Query{Name: "Leonetti Cellar Cabernet Sauvignon Walla Walla 2019 750ml", Vintage: 2019})
	if res.Route != RouteAuto {
		t.Fatalf("expected auto route, got %s (best %.1f %s)", res.Route, res.Best.Score, res.Best.Record.DisplayName())
	}
	if res.Best.Record.LWIN7 != "1101245" {
		t.Fatalf("expected Leonetti 1101245, got %s", res.Best.Record.LWIN7)
	}
	if got := res.Best.Record.LWIN11(2019); got != "11012452019" {
		t.Fatalf("expected LWIN11 11012452019, got %s", got)
	}
}

func TestResolve_AccentedAbbreviatedTitle(t *testing.T) {
	r := Resolver{DB: sampleDB()}
	res := r.Resolve(Query{Name: "Ch. Margaux 2015"})
	if res.Route != RouteAuto || res.Best.Record.LWIN7 != "1012361" {
		t.Fatalf("expected auto-match to Chateau Margaux, got route=%s best=%s score=%.1f",
			res.Route, res.Best.Record.DisplayName(), res.Best.Score)
	}
}

func TestResolve_SecondWinePrefersLongerMatch(t *testing.T) {
	// "Pavillon Rouge" is Margaux's second wine and a DIFFERENT LWIN; the
	// grand vin's name is a subset of the query, so both records score as
	// full token-subset matches -- the more complete name must win the tie.
	r := Resolver{DB: sampleDB()}
	res := r.Resolve(Query{Name: "Chateau Margaux Pavillon Rouge 2016"})
	if res.Best.Record.LWIN7 != "1012362" {
		t.Fatalf("expected the second wine 1012362, got %s (%s)",
			res.Best.Record.LWIN7, res.Best.Record.DisplayName())
	}
}

func TestResolve_NoOverlapIsReview(t *testing.T) {
	r := Resolver{DB: sampleDB()}
	res := r.Resolve(Query{Name: "Screaming Eagle Napa"})
	if res.Route != RouteReview {
		t.Fatalf("no credible candidate should route to review, got %s (best %.1f %s)",
			res.Route, res.Best.Score, res.Best.Record.DisplayName())
	}
}

func TestResolve_PartialOverlapRoutesBelowAuto(t *testing.T) {
	// Shares "cabernet sauvignon" tokens with two records but no producer
	// token: must never auto-match.
	r := Resolver{DB: sampleDB()}
	res := r.Resolve(Query{Name: "Random Winery Cabernet Sauvignon 2020"})
	if res.Route == RouteAuto {
		t.Fatalf("a producer-less overlap must not auto-match, got best %.1f %s",
			res.Best.Score, res.Best.Record.DisplayName())
	}
}

func TestResolve_EmptyInputs(t *testing.T) {
	r := Resolver{DB: sampleDB()}
	if res := r.Resolve(Query{Name: "   "}); res.Route != RouteReview {
		t.Errorf("blank name should be review, got %s", res.Route)
	}
	empty := Resolver{DB: NewDB(nil)}
	if res := empty.Resolve(Query{Name: "Chateau Margaux"}); res.Route != RouteReview {
		t.Errorf("empty DB should be review, got %s", res.Route)
	}
	var nilDB Resolver
	if res := nilDB.Resolve(Query{Name: "Chateau Margaux"}); res.Route != RouteReview {
		t.Errorf("nil DB should be review, got %s", res.Route)
	}
}

func TestResolve_CandidatesBoundedAndOrdered(t *testing.T) {
	r := Resolver{DB: sampleDB(), MaxCandidates: 2}
	res := r.Resolve(Query{Name: "Cabernet Sauvignon"})
	if len(res.Candidates) > 2 {
		t.Fatalf("candidates should be bounded to 2, got %d", len(res.Candidates))
	}
	for i := 1; i < len(res.Candidates); i++ {
		if res.Candidates[i].Score > res.Candidates[i-1].Score {
			t.Fatalf("candidates should be sorted best-first")
		}
	}
}

func TestResolve_Deterministic(t *testing.T) {
	r := Resolver{DB: sampleDB()}
	q := Query{Name: "Quilceda Creek Cabernet Sauvignon Columbia Valley 2018"}
	first := r.Resolve(q)
	for i := 0; i < 5; i++ {
		again := r.Resolve(q)
		if again.Best.Record.LWIN7 != first.Best.Record.LWIN7 || again.Route != first.Route {
			t.Fatalf("resolution must be deterministic")
		}
	}
}

// --- similarity primitives ---

func TestTokenSetRatio_SubsetScoresFull(t *testing.T) {
	// The record name fully contained in a noisier retail title is the
	// canonical case and must score at the top of the scale.
	got := tokenSetRatio("ridge monte bello 2018 750ml california", "ridge monte bello")
	if got < 99 {
		t.Fatalf("full subset should score ~100, got %.1f", got)
	}
}

func TestTokenSetRatio_DisjointScoresLow(t *testing.T) {
	got := tokenSetRatio("screaming eagle napa", "ridge monte bello")
	if got > 50 {
		t.Fatalf("disjoint names should score low, got %.1f", got)
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"margaux", "margaux", 0},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestJaroWinkler_Basics(t *testing.T) {
	if got := jaroWinkler("margaux", "margaux"); got != 1 {
		t.Errorf("identical strings should be 1, got %v", got)
	}
	if got := jaroWinkler("abc", "xyz"); got != 0 {
		t.Errorf("disjoint strings should be 0, got %v", got)
	}
	// Shared-prefix boost: "martha" vs "marhta" > plain jaro.
	if jaroWinkler("martha", "marhta") <= jaro("martha", "marhta") {
		t.Errorf("winkler prefix boost should raise the score")
	}
}
