// Command genappellations regenerates the wine extractor's appellation table
// (internal/extract/wine/appellations_lwin.go) from the Liv-ex LWIN database
// export. It runs OFFLINE, by hand; nagus never loads LWIN at runtime (the
// LWIN resolver moved to quark, QUARK-04), so the table is static data.
//
//	go run ./tools/genappellations -in LWINdatabase.csv -out internal/extract/wine/appellations_lwin.go
//
// The input is the LWIN database exported to CSV (the columns used are STATUS,
// TYPE, COUNTRY, REGION and SUB_REGION). The LWIN database is published by
// Liv-ex under the Creative Commons Attribution 4.0 licence
// (http://www.liv-ex.com/lwin-creative-commons-licence/); the generated file
// carries that attribution and describes the changes made here.
//
// What it does, in order:
//
//  1. Keeps Live rows whose TYPE is "Wine" or "Fortified Wine".
//  2. Counts wines per distinct REGION and SUB_REGION name.
//  3. Drops names with fewer than -min wines (default 20): LWIN's long tail
//     is single-producer lieux-dits and misfiled names, and a rare name buys
//     little recall for its false-positive risk.
//  4. Drops the names on the exclude list (ordinary words, political regions
//     and cities, US states, and names another rule already handles).
//  5. Tiers the rest. A name on the broad list, or one whose wines are mostly
//     from a New World country, is BROAD: wine evidence only beside a
//     supporting cue (see the wine package). Everything else is an
//     APPELLATION: wine evidence on its own.
//
// Names are normalized exactly as the extractor normalizes titles: accents
// folded, lower-cased, every run of non-alphanumerics a single space, and
// "st"/"ste"/"mt" spelled out.
package main

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"sort"
	"strings"
)

// minWinesDefault is the smallest number of LWIN wines a name must carry.
// 1,862 names in the 2026-09 export, 881 of them with at least 20 wines.
const minWinesDefault = 20

// exclude is never evidence. Keys are normalized names.
var exclude = setOf(
	// US states, countries and other political units: they name where a
	// winery or an event is, far more often than a wine.
	"california", "washington", "oregon", "new york", "virginia", "texas",
	"arizona", "idaho", "michigan", "missouri", "pennsylvania", "new jersey",
	"colorado", "ontario", "british columbia", "quebec", "nova scotia",
	"england", "denmark", "crimea", "macedonia", "victoria", "queensland",
	"new south wales", "south australia", "western australia", "tasmania",
	"central victoria", "south eastern australia", "adelaide", "cape town",
	"auckland", "madrid", "valencia", "murcia", "aragon", "castilla",
	"extremadura", "hokkaido", "nagano", "yamagata", "shandong", "xinjiang",
	"ningxia", "baja california", "patagonia", "canary islands",
	"balearic islands", "mallorca", "crete", "wien", "geneve", "verona",
	"venezia", "treviso", "long island",
	// Ordinary English words and common names.
	"orange", "brand", "fully", "darling", "elgin", "robertson", "nelson",
	"wellington", "gladstone", "monticello", "canterbury", "salina", "pico",
	"sur", "var", "gard", "aude", "ay", "oger", "central coast", "north coast",
	"coastal region", "central valley", "great southern", "granite belt",
	"high valley", "grand valley", "swan valley", "red hills lake county",
	"lake county", "king valley", "riverland", "hilltops", "pyrenees",
	"grampians",
	// Handled by other rules: fortified styles (fortifiedRe) and sparkling
	// colour cues (colourKeywords). Leaving them out keeps each word on one
	// rule with one set of guards.
	"port", "porto", "madeira", "marsala", "champagne", "cava", "prosecco",
)

// broad is evidence only beside a supporting cue: a region wide enough (or a
// word common enough) that a title naming it alone may be a candle, a tour or
// a tote. New World names are broad by country; this list is the Old World
// regions and the few Old World appellations that double as common words.
var broad = setOf(
	"burgundy", "bourgogne", "bordeaux", "alsace", "piedmont", "piemonte",
	"rhone", "tuscany", "toscana", "costa toscana", "loire", "val de loire",
	"languedoc", "roussillon", "provence", "sud ouest", "jura", "savoie",
	"corsica", "ile de beaute", "mediterranee", "ardeche", "vaucluse",
	"cevennes", "veneto", "sicily", "sicilia", "sardinia", "puglia", "salento",
	"campania", "lazio", "marche", "umbria", "abruzzo", "molise", "calabria",
	"basilicata", "liguria", "lombardia", "emilia romagna", "emilia",
	"romagna", "friuli venezia giulia", "venezia giulia", "trentino",
	"trentino alto adige", "trento", "cortona", "noto", "paestum", "nizza",
	"mosel", "baden", "franken", "wurttemberg", "niederosterreich",
	"burgenland", "steiermark", "weinland", "valais", "ticino", "graubunden",
	"vaud", "neuchatel", "la cote", "alentejo", "alentejano", "lisboa", "tejo",
	"minho", "beiras", "beira interior", "tras os montes", "acores",
	"peninsula de setubal", "toro", "la mancha", "castilla la mancha",
	"castilla y leon", "alicante", "malaga", "tenerife", "lanzarote",
	"catalunya", "galicia", "andalucia", "pais vasco", "algarve", "kakheti",
	"galilee", "upper galilee", "judean hills", "golan heights",
	"bekaa valley", "thracian lowlands", "moselle luxembourgeoise",
	"peloponnese", "cyclades", "central greece", "aegean islands", "istria",
	"hrvatska istra", "dalmacija", "slavonija", "primorska", "podravje",
	"stajerska slovenija", "quincy", "maury", "graves", "fixin",
	"saint joseph", "l etoile", "brouilly", "sjalland", "guerrouane",
	"valul lui traian", "stefan voda", "vardar river valley",
)

// newWorld countries' names are broad: their wines are named by grape, which
// the varietal rule already catches, and their place names are where the
// wineries, tours and tasting rooms are.
var newWorld = setOf(
	"united states", "canada", "mexico", "australia", "new zealand",
	"south africa", "chile", "argentina", "uruguay", "brazil", "china",
	"japan", "india",
)

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Entry is one generated table row.
type Entry struct {
	Name  string // normalized
	Wines int
	Broad bool
}

// Build reads an LWIN CSV export and returns the table, sorted by name.
func Build(r io.Reader, minWines int) ([]Entry, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	for _, need := range []string{"STATUS", "TYPE", "COUNTRY", "REGION", "SUB_REGION"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("missing column %s", need)
		}
	}
	count := map[string]int{}
	country := map[string]map[string]int{}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		get := func(k string) string {
			if i := col[k]; i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if get("STATUS") != "Live" {
			continue
		}
		if t := get("TYPE"); t != "Wine" && t != "Fortified Wine" {
			continue
		}
		for _, k := range []string{"REGION", "SUB_REGION"} {
			v := get(k)
			if v == "" || v == "NA" {
				continue
			}
			n := Normalize(v)
			if n == "" {
				continue
			}
			count[n]++
			if country[n] == nil {
				country[n] = map[string]int{}
			}
			country[n][strings.ToLower(get("COUNTRY"))]++
		}
	}
	var out []Entry
	for n, c := range count {
		if c < minWines || exclude[n] {
			continue
		}
		out = append(out, Entry{Name: n, Wines: c, Broad: broad[n] || newWorld[majority(country[n])]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// majority returns the key with the highest count (ties broken by name).
func majority(m map[string]int) string {
	best, bestN := "", -1
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

// fold is the accent fold; it must stay equivalent to the wine package's
// foldASCII for every character LWIN uses (TestTableIsNormalized checks the
// output).
var fold = strings.NewReplacer(
	"\u00e0", "a", "\u00e2", "a", "\u00e4", "a", "\u00e1", "a", "\u00e3", "a",
	"\u00e7", "c",
	"\u00e8", "e", "\u00e9", "e", "\u00ea", "e", "\u00eb", "e",
	"\u00ee", "i", "\u00ef", "i", "\u00ed", "i",
	"\u00f1", "n",
	"\u00f4", "o", "\u00f6", "o", "\u00f3", "o", "\u00f8", "o",
	"\u00fb", "u", "\u00fc", "u", "\u00fa", "u",
)

// spelled are the abbreviations the extractor spells out.
var spelled = map[string]string{"st": "saint", "ste": "sainte", "mt": "mount"}

// Normalize is the extractor's phrase normalization (wine.normalizePhrase).
func Normalize(s string) string {
	s = fold.Replace(strings.ToLower(s))
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	for i, w := range words {
		if l, ok := spelled[w]; ok {
			words[i] = l
		}
	}
	return strings.Join(words, " ")
}

// Render writes the Go source of the table.
func Render(w io.Writer, entries []Entry, minWines int) error {
	var b strings.Builder
	b.WriteString(`// Code generated by tools/genappellations; DO NOT EDIT.

// Appellation data derived from the LWIN database, (c) Liv-ex, licensed under
// the Creative Commons Attribution 4.0 licence
// (http://www.liv-ex.com/lwin-creative-commons-licence/).
//
// Changes from the source: only the REGION and SUB_REGION names of Live wines
// (TYPE "Wine" or "Fortified Wine") are used; names are accent-folded,
// lower-cased and punctuation-normalized; names with fewer than `)
	fmt.Fprintf(&b, "%d", minWines)
	b.WriteString(` wines,
// and the ordinary words, political places and names other rules handle, are
// dropped; the rest are tiered into appellations and broad regions. The
// generator's exclude and broad lists document every hand decision.

package wine

// lwinAppellations maps a normalized name to its tier: true for a broad
// region (evidence only beside a supporting cue), false for an appellation
// (evidence on its own).
var lwinAppellations = map[string]bool{
`)
	for _, e := range entries {
		for _, r := range e.Name {
			if r > 0x7e {
				return fmt.Errorf("non-ASCII name %q", e.Name)
			}
		}
		fmt.Fprintf(&b, "\t%q: %t, // %d\n", e.Name, e.Broad, e.Wines)
	}
	b.WriteString("}\n")
	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("format: %w", err)
	}
	_, err = w.Write(src)
	return err
}

func main() {
	in := flag.String("in", "", "LWIN database CSV export")
	out := flag.String("out", "internal/extract/wine/appellations_lwin.go", "generated Go file")
	minWines := flag.Int("min", minWinesDefault, "minimum LWIN wines per name")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "genappellations: -in is required")
		os.Exit(2)
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genappellations:", err)
		os.Exit(1)
	}
	defer f.Close()
	entries, err := Build(f, *minWines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genappellations:", err)
		os.Exit(1)
	}
	o, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genappellations:", err)
		os.Exit(1)
	}
	if err := Render(o, entries, *minWines); err != nil {
		fmt.Fprintln(os.Stderr, "genappellations:", err)
		os.Exit(1)
	}
	if err := o.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "genappellations:", err)
		os.Exit(1)
	}
	broadN := 0
	for _, e := range entries {
		if e.Broad {
			broadN++
		}
	}
	fmt.Printf("genappellations: %d names (%d appellations, %d broad) -> %s\n", len(entries), len(entries)-broadN, broadN, *out)
}
