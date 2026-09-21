package lwin

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildXLSX writes a minimal workbook shaped like the Liv-ex export: inline
// strings, numeric LWINs stored as floats, "NA" for missing values. shared is
// the shared string table (the export ships an almost empty one).
func buildXLSX(t *testing.T, sheetXML string, shared []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("[Content_Types].xml", `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`)
	if shared != nil {
		var sb strings.Builder
		sb.WriteString(`<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
		for _, s := range shared {
			sb.WriteString("<si><t>" + s + "</t></si>")
		}
		sb.WriteString("</sst>")
		add("xl/sharedStrings.xml", sb.String())
	}
	add("xl/worksheets/sheet1.xml", `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`+sheetXML+`</sheetData></worksheet>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func is(ref, v string) string {
	return `<c r="` + ref + `" t="inlineStr"><is><t>` + v + `</t></is></c>`
}
func num(ref, v string) string { return `<c r="` + ref + `"><v>` + v + `</v></c>` }
func shr(ref string, i string) string {
	return `<c r="` + ref + `" t="s"><v>` + i + `</v></c>`
}

func row(n string, cells ...string) string {
	return `<row r="` + n + `">` + strings.Join(cells, "") + `</row>`
}

func TestLoadXLSX_LivExShape(t *testing.T) {
	sheet := row("1", is("A1", "LWIN"), is("B1", "STATUS"), is("C1", "PRODUCER_NAME"), is("D1", "WINE"),
		is("E1", "COUNTRY"), is("F1", "REGION"), is("G1", "COLOUR"), is("H1", "TYPE")) +
		// Live wine: float LWIN, NA region.
		row("2", num("A2", "1101245.0"), is("B2", "Live"), is("C2", "Leonetti Cellar"), is("D2", "Cabernet Sauvignon"),
			is("E2", "United States"), is("F2", "NA"), is("G2", "Red"), is("H2", "Wine")) +
		// Retired LWIN: must not load.
		row("3", num("A3", "1000002.0"), is("B3", "Deleted"), is("C3", "Gone"), is("D3", "Red"),
			is("E3", "France"), is("F3", "Bordeaux"), is("G3", "Red"), is("H3", "Wine")) +
		// A spirit: not what the wine category resolves.
		row("4", num("A4", "1000003.0"), is("B4", "Live"), is("C4", "Distillery"), is("D4", "Single Malt"),
			is("E4", "Scotland"), is("F4", "Speyside"), is("G4", "NA"), is("H4", "Spirit")) +
		// Fortified wine via a SHARED string producer, with the WINE cell
		// absent (a sparse row: D is skipped entirely).
		row("5", num("A5", "1000004.0"), is("B5", "Live"), shr("C5", "0"),
			is("E5", "Portugal"), is("F5", "Douro"), is("G5", "Red"), is("H5", "Fortified Wine"))
	data := buildXLSX(t, sheet, []string{"Taylor&apos;s"})
	db, err := LoadXLSX(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("LoadXLSX: %v", err)
	}
	if db.Len() != 2 {
		t.Fatalf("loaded %d records, want 2 (the live wine and the fortified wine)", db.Len())
	}
	got := map[string]Record{}
	for _, r := range db.records {
		got[r.LWIN7] = r
	}
	leo, ok := got["1101245"]
	if !ok {
		t.Fatalf("float LWIN not normalized to 7 digits: %+v", db.records)
	}
	if leo.Region != "" || leo.Colour != "red" || leo.Producer != "Leonetti Cellar" {
		t.Fatalf("Leonetti = %+v; NA must become empty and colour lower-cased", leo)
	}
	if port := got["1000004"]; port.Producer != "Taylor's" || port.Wine != "" || port.Region != "Douro" {
		t.Fatalf("shared-string / sparse row = %+v", port)
	}
}

// The same export as CSV loads identically: both formats go through loadRows.
func TestLoadCSV_AppliesTheSameFilters(t *testing.T) {
	csv := "LWIN,STATUS,PRODUCER_NAME,WINE,COUNTRY,REGION,COLOUR,TYPE\n" +
		"1101245.0,Live,Leonetti Cellar,Cabernet Sauvignon,United States,NA,Red,Wine\n" +
		"1000002.0,Deleted,Gone,Red,France,Bordeaux,Red,Wine\n" +
		"1000003.0,Live,Distillery,Single Malt,Scotland,Speyside,NA,Spirit\n"
	db, err := LoadCSV(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	if db.Len() != 1 || db.records[0].LWIN7 != "1101245" || db.records[0].Region != "" {
		t.Fatalf("got %+v", db.records)
	}
}

func TestLoadXLSX_RejectsWhatIsNotTheExport(t *testing.T) {
	if _, err := LoadXLSX(strings.NewReader("<html>not a workbook</html>"), 26); err == nil {
		t.Error("an HTML error page served as the file must not load")
	}
	noSheet := func() []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		_, _ = zw.Create("hello.txt")
		_ = zw.Close()
		return buf.Bytes()
	}()
	if _, err := LoadXLSX(bytes.NewReader(noSheet), int64(len(noSheet))); err == nil {
		t.Error("a zip with no worksheet must not load")
	}
	noLWIN := buildXLSX(t, row("1", is("A1", "PRODUCER_NAME"))+row("2", is("A2", "X")), nil)
	if _, err := LoadXLSX(bytes.NewReader(noLWIN), int64(len(noLWIN))); err == nil {
		t.Error("a sheet without an LWIN column must not load")
	}
}

// A swapped Dictionary is what an in-flight refresh changes: Resolve sees the
// new DB on its next call, and Dict wins over the fixed DB field.
func TestResolverFollowsTheDictionary(t *testing.T) {
	var d Dictionary
	r := Resolver{Dict: &d, DB: NewDB(nil)}
	if r.Len() != 0 || r.Resolve(Query{Name: "Leonetti Cellar Cabernet Sauvignon"}).Route != RouteReview {
		t.Fatal("an empty dictionary must match nothing")
	}
	d.Store(NewDB([]Record{{LWIN7: "1101245", Producer: "Leonetti Cellar", Wine: "Cabernet Sauvignon"}}))
	if r.Len() != 1 {
		t.Fatalf("resolver did not follow the swapped dictionary: len %d", r.Len())
	}
	if res := r.Resolve(Query{Name: "Leonetti Cellar Cabernet Sauvignon 2019"}); res.Route != RouteAuto {
		t.Fatalf("after the swap: %+v", res)
	}
}

// Regression from the real export: these auto-matched at 100 before the
// producer gate. Producer-storefront titles do not name the producer, and a
// record whose name is a subset of the title scores 100 on token-set alone.
func TestResolve_AutoRequiresProducerAgreement(t *testing.T) {
	db := NewDB([]Record{
		{LWIN7: "1000001", Producer: "A", Wine: "Merlot", Region: "Tasmania"},
		{LWIN7: "1000002", Producer: "B & E Vineyard", Wine: "Reserve Cabernet Sauvignon", Region: "California"},
		{LWIN7: "1000003", Producer: "Robert Mondavi Winery", Wine: "60th Anniversary Cabernet Sauvignon", Region: "California"},
		// Real producers named after grapes and styles.
		{LWIN7: "1000004", Producer: "Chardonnay", Wine: "", Region: "Burgundy"},
		{LWIN7: "1000005", Producer: "La Rose", Wine: "", Region: "Bordeaux"},
	})
	r := Resolver{DB: db}
	for _, title := range []string{
		"2021 The Reserve Merlot, To Kalon Vineyard",
		"2018 The Reserve Cabernet Sauvignon, To Kalon Vineyard",
		"Merlot",
		"2024 Napa Valley Chardonnay",
		"La Vie en Rose",
	} {
		if res := r.Resolve(Query{Name: title}); res.Route == RouteAuto {
			t.Errorf("%q auto-matched %q with no producer in the title", title, res.Best.Record.DisplayName())
		}
	}
	res := r.Resolve(Query{Name: "Robert Mondavi Winery 60th Anniversary Commemorative Cabernet Sauvignon"})
	if res.Route != RouteAuto || res.Best.Record.LWIN7 != "1000003" {
		t.Fatalf("a title naming the producer must still auto-match: %+v", res)
	}
}

// Records shaped like Robert Mondavi's real LWIN rows (2026-09 export).
func mondaviDB() *DB {
	return NewDB([]Record{
		{LWIN7: "1122646", Producer: "Robert Mondavi Winery", Wine: "Cabernet Sauvignon", Region: "California", SubRegion: "Napa Valley"},
		{LWIN7: "1250624", Producer: "Robert Mondavi Winery", Wine: "Cabernet Sauvignon", Region: "California", SubRegion: "Stags Leap District"},
		{LWIN7: "1186602", Producer: "Robert Mondavi Winery", Wine: "The Estates Cabernet Sauvignon", Region: "California", SubRegion: "Oakville"},
		{LWIN7: "1186312", Producer: "Mondavi", Wine: "To Kalon Vineyard The Reserve Cabernet Sauvignon", Region: "California", SubRegion: "Oakville"},
		{LWIN7: "1122659", Producer: "Robert Mondavi Winery", Wine: "Reserve Cabernet Sauvignon", Region: "California", SubRegion: "Napa Valley"},
		{LWIN7: "1241626", Producer: "Robert Mondavi Winery", Wine: "Merlot", Region: "California", SubRegion: "Napa Valley"},
		{LWIN7: "9000001", Producer: "Bolero", Wine: "Rouge", Region: "Bordeaux"},
		{LWIN7: "9000002", Producer: "B & E Vineyard", Wine: "Reserve Cabernet Sauvignon", Region: "California"},
	})
}

func TestResolve_ProducerHintPicksTheRightWine(t *testing.T) {
	r := Resolver{DB: mondaviDB()}
	const mondavi = "Robert Mondavi Winery"
	for _, tc := range []struct {
		title, want string
		route       Route
	}{
		// Geography picks among same-named records.
		{"2022 Napa Valley Cabernet Sauvignon", "1122646", RouteAuto},
		{"2022 The Estates Cabernet Sauvignon, Oakville", "1186602", RouteAuto},
		// The specific record beats the generic one it contains.
		{"2021 The Reserve Cabernet Sauvignon, To Kalon Vineyard", "1186312", RouteAuto},
		// Names something no record covers: never auto.
		{"2022 The Estates Merlot, Oak Knoll", "", RouteAdjudicate},
		{"2021 The Reserve Heritage Clone Cabernet Sauvignon, To Kalon Vineyard", "", RouteAdjudicate},
		{"2023 The Estates Cabernet Sauvignon, W.H Vineyard", "", RouteAdjudicate},
	} {
		res := r.Resolve(Query{Name: tc.title, Producer: mondavi})
		if res.Route != tc.route || (tc.want != "" && res.Best.Record.LWIN7 != tc.want) {
			t.Errorf("%q: route %s best %s (%s), want %s %s", tc.title, res.Route,
				res.Best.Record.LWIN7, res.Best.Record.DisplayName(), tc.route, tc.want)
		}
	}
}

// A known producer is a hard constraint: Harbinger's wine "Bolero" must not
// resolve to the real producer named Bolero.
func TestResolve_ProducerHintExcludesOtherProducers(t *testing.T) {
	r := Resolver{DB: mondaviDB()}
	if res := r.Resolve(Query{Name: "Bolero", Producer: "Harbinger Winery"}); res.Route == RouteAuto {
		t.Fatalf("Bolero with producer Harbinger auto-matched %s", res.Best.Record.DisplayName())
	}
	if res := r.Resolve(Query{Name: "2018 The Reserve Cabernet Sauvignon, To Kalon Vineyard", Producer: "Robert Mondavi Winery"}); res.Best.Record.Producer == "B & E Vineyard" {
		t.Fatal("a known producer must exclude other producers' records")
	}
}
