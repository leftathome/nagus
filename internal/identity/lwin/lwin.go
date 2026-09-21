// Package lwin resolves wine listings to LWIN identifiers -- the wine
// category's canonical ID (design section 5's canonical_id extractor slot).
//
// LWIN is Liv-ex's Creative-Commons-licensed universal wine identifier: an
// LWIN-7 names a producer/wine, LWIN-11 appends a 4-digit vintage, LWIN-16
// appends a 5-digit bottle size in ml. The database itself is a free download
// -- the form at liv-ex.com/lwin/ leads to a public object, refreshed often, at
// https://s3-eu-west-1.amazonaws.com/lwin-dictionary/latest/LWINdatabase.xlsx
// (LoadXLSX reads it directly; package refdata keeps a local mirror); this package
// takes the loaded records and does ENTITY RESOLUTION from messy retailer
// listing titles to an LWIN-11.
//
// The resolver is deterministic and fully offline: normalize the query text
// (accent-fold, expand producer abbreviations), block candidates by shared
// tokens, score with token-set similarity (Jaro-Winkler as tiebreak), and
// route by confidence:
//
//	score >= AutoThreshold      -> RouteAuto        (safe to stamp the LWIN)
//	score >= ReviewThreshold    -> RouteAdjudicate  (an LLM/human should pick
//	                                                 among the top candidates)
//	otherwise                   -> RouteReview      (no credible candidate)
//
// Only RouteAuto matches should be written to item.CanonicalID: a wrong
// canonical identity silently corrupts every downstream quality join, which
// is worse than no identity (spec Stage 0 acceptance: <5% false auto-match).
package lwin

import (
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
)

// Record is one LWIN-7 entry: a producer/wine with its geography and colour.
type Record struct {
	LWIN7    string // 7-digit producer/wine code
	Producer string
	Wine     string
	Country  string
	Region   string
	Colour   string // red | white | rose | ...
	// SubRegion and Site are part of a wine's identity in LWIN: Mondavi's
	// Cabernet Sauvignon has distinct LWINs for Napa Valley and Stags Leap
	// District, and "To Kalon Vineyard The Reserve Cabernet Sauvignon" is its
	// own record. They are matched on (matchText), not displayed.
	SubRegion string
	Site      string
}

// DisplayName is the record's resolvable name: producer + wine.
func (r Record) DisplayName() string {
	if r.Wine == "" {
		return r.Producer
	}
	return r.Producer + " " + r.Wine
}

// matchText is the name plus the geography that distinguishes otherwise
// same-named wines. It feeds blocking and COVERAGE -- a title's "Napa Valley"
// is accounted for by a record whose sub-region is Napa Valley -- but not the
// similarity score, which stays on the name: a title need not repeat the
// region to match ("Chateau Margaux 2015").
func (r Record) matchText() string {
	return strings.TrimSpace(r.DisplayName() + " " + r.Region + " " + r.SubRegion + " " + r.Site)
}

// LWIN11 renders the record's LWIN-11 for a vintage. Vintage 0 (non-vintage)
// uses the LWIN convention "1000" as the vintage segment.
func (r Record) LWIN11(vintage int) string {
	if vintage <= 0 {
		return r.LWIN7 + "1000"
	}
	return fmt.Sprintf("%s%04d", r.LWIN7, vintage)
}

// Route is the confidence disposition of a resolution.
type Route string

const (
	// RouteAuto: high confidence, safe to stamp as the item's canonical id.
	RouteAuto Route = "auto"
	// RouteAdjudicate: mid confidence; surface the top candidates to an
	// adjudicator (local LLM or human) rather than guessing.
	RouteAdjudicate Route = "adjudicate"
	// RouteReview: no credible candidate; needs human review or stays
	// unidentified.
	RouteReview Route = "review"
)

// Match is one scored candidate.
type Match struct {
	Record Record
	Score  float64 // 0-100 similarity
}

// Resolution is the outcome of resolving one query.
type Resolution struct {
	Route Route
	// Best is the top candidate; only meaningful when Route != RouteReview.
	Best Match
	// Candidates are the top-N scored candidates (best first), for the
	// adjudication tier.
	Candidates []Match
}

// Query is one listing to resolve. Name is the sanitized listing title
// (treated purely as data: it is only ever tokenized and compared).
type Query struct {
	Name    string
	Vintage int // 0 = unknown / non-vintage
	// Producer, when known from outside the title -- the store a listing came
	// from is often the producer, and producer-storefront titles never name it
	// ("2021 The Estates Merlot, Oak Knoll" on Robert Mondavi's own store) --
	// is scored together with the title and restricts candidates to records of
	// that producer. Empty = title only.
	Producer string
}

// DB is an in-memory LWIN database with a token index for blocking.
type DB struct {
	records []Record
	// tokenIdx maps a normalized token to record indices containing it
	// (blocking: only records sharing at least one token are scored).
	tokenIdx map[string][]int
}

// NewDB builds a DB from records. Records without an LWIN7 or a producer are
// skipped (they could never be stamped or matched meaningfully).
func NewDB(records []Record) *DB {
	db := &DB{tokenIdx: map[string][]int{}}
	for _, r := range records {
		if strings.TrimSpace(r.LWIN7) == "" || strings.TrimSpace(r.Producer) == "" {
			continue
		}
		idx := len(db.records)
		db.records = append(db.records, r)
		for _, tok := range tokens(normalizeName(r.matchText())) {
			db.tokenIdx[tok] = append(db.tokenIdx[tok], idx)
		}
	}
	return db
}

// Len reports the number of usable records loaded.
func (db *DB) Len() int { return len(db.records) }

// LoadCSV reads an LWIN export. The header row names the columns; recognized
// (case-insensitive) names follow the Liv-ex export: LWIN, PRODUCER_NAME (or
// PRODUCER), WINE, COUNTRY, REGION, COLOUR (or COLOR). Unknown columns are
// ignored so a fuller export loads without modification. See loadRows for the
// normalization and filtering every loader applies.
func LoadCSV(r io.Reader) (*DB, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("lwin: reading header: %w", err)
	}
	return loadRows(header, func() ([]string, error) {
		row, err := cr.Read()
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("lwin: reading row: %w", err)
		}
		return row, err
	})
}

// loadRows builds a DB from a header and a row iterator (next returns io.EOF
// when done). Shared by LoadCSV and LoadXLSX so both formats load identically.
//
// Normalization, all driven by the real Liv-ex export (2026-09, 212,430 rows):
//   - The literal "NA" is the export's missing-value marker. It becomes "",
//     or every blank region would score as the token "na".
//   - Numeric cells arrive as floats ("1000001.0"); a trailing ".0" on the
//     LWIN is dropped, or no LWIN-11 built from it would be valid.
//   - When the STATUS column exists only "Live" rows load: "Combined" and
//     "Deleted" LWINs are retired and must never be stamped.
//   - When the TYPE column exists only wines load (Wine, Fortified Wine):
//     spirits, beer and cider are not what the wine category resolves, and
//     loading them costs memory and invites false matches.
//
// Exports without STATUS or TYPE (test fixtures, older files) load unfiltered.
func loadRows(header []string, next func() ([]string, error)) (*DB, error) {
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToUpper(strings.TrimSpace(h))] = i
	}
	get := func(row []string, names ...string) string {
		for _, n := range names {
			if i, ok := col[n]; ok && i < len(row) {
				v := strings.TrimSpace(row[i])
				if v == "NA" {
					return ""
				}
				return v
			}
		}
		return ""
	}
	if _, ok := col["LWIN"]; !ok {
		return nil, fmt.Errorf("lwin: header has no LWIN column (got %v)", header)
	}
	_, hasStatus := col["STATUS"]
	_, hasType := col["TYPE"]

	var records []Record
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hasStatus && get(row, "STATUS") != "Live" {
			continue
		}
		if hasType && !wineTypes[get(row, "TYPE")] {
			continue
		}
		records = append(records, Record{
			LWIN7:     strings.TrimSuffix(get(row, "LWIN"), ".0"),
			Producer:  get(row, "PRODUCER_NAME", "PRODUCER"),
			Wine:      get(row, "WINE"),
			Country:   get(row, "COUNTRY"),
			Region:    get(row, "REGION"),
			Colour:    strings.ToLower(get(row, "COLOUR", "COLOR")),
			SubRegion: get(row, "SUB_REGION"),
			Site:      get(row, "SITE"),
		})
	}
	return NewDB(records), nil
}

// wineTypes are the export TYPE values the wine category resolves.
var wineTypes = map[string]bool{"Wine": true, "Fortified Wine": true}

// Dictionary holds the current DB for a Resolver whose data is refreshed while
// it serves. Store swaps it atomically: a Resolve in flight keeps the DB it
// started with, and the next one sees the new DB. The zero value holds nothing.
type Dictionary struct {
	p atomic.Pointer[DB]
}

// Load returns the current DB, or nil.
func (d *Dictionary) Load() *DB { return d.p.Load() }

// Store replaces the current DB.
func (d *Dictionary) Store(db *DB) { d.p.Store(db) }

// Resolver resolves queries against a DB with confidence routing.
type Resolver struct {
	// DB is a fixed dictionary. Dict, when set, takes precedence: it is the
	// swappable holder for a dictionary refreshed while serving.
	DB   *DB
	Dict *Dictionary
	// AutoThreshold is the minimum score for RouteAuto; 0 defaults to 92.
	AutoThreshold float64
	// ReviewThreshold is the minimum score for RouteAdjudicate; 0 defaults
	// to 80. Below it, RouteReview.
	ReviewThreshold float64
	// MaxCandidates bounds Resolution.Candidates; 0 defaults to 5.
	MaxCandidates int
}

const (
	defaultAutoThreshold   = 92
	defaultReviewThreshold = 80
	defaultMaxCandidates   = 5
)

// db is the dictionary this Resolve uses: Dict's current DB when set, else DB.
func (r Resolver) db() *DB {
	if r.Dict != nil {
		if db := r.Dict.Load(); db != nil {
			return db
		}
	}
	return r.DB
}

// Len reports the number of records the resolver currently matches against.
func (r Resolver) Len() int {
	if db := r.db(); db != nil {
		return db.Len()
	}
	return 0
}

func (r Resolver) thresholds() (auto, review float64, maxC int) {
	auto, review, maxC = r.AutoThreshold, r.ReviewThreshold, r.MaxCandidates
	if auto <= 0 {
		auto = defaultAutoThreshold
	}
	if review <= 0 {
		review = defaultReviewThreshold
	}
	if maxC <= 0 {
		maxC = defaultMaxCandidates
	}
	return auto, review, maxC
}

// Resolve scores the query against blocked candidates and routes by
// confidence. A nil/empty DB or a blank name is RouteReview (nothing to say).
func (r Resolver) Resolve(q Query) Resolution {
	auto, review, maxC := r.thresholds()
	db := r.db()
	if db == nil || db.Len() == 0 {
		return Resolution{Route: RouteReview}
	}
	text := q.Name
	var producerTokens []string
	if p := strings.TrimSpace(q.Producer); p != "" {
		producerTokens = tokens(normalizeName(p))
		text = p + " " + q.Name
	}
	norm := normalizeName(text)
	qTokens := tokens(norm)
	if len(qTokens) == 0 {
		return Resolution{Route: RouteReview}
	}

	// Blocking: candidates sharing at least one query token. This bounds
	// scoring to a tiny fraction of a ~100k-record DB.
	seen := map[int]bool{}
	var candidates []int
	for _, tok := range qTokens {
		for _, idx := range db.tokenIdx[tok] {
			if !seen[idx] {
				seen[idx] = true
				candidates = append(candidates, idx)
			}
		}
	}
	// A known producer is a hard constraint, not a hint to be outvoted: a
	// record of another producer is never the right identity, however well its
	// wine name happens to fit the title.
	if len(producerTokens) > 0 {
		kept := candidates[:0]
		for _, idx := range candidates {
			if producerAgrees(producerTokens, db.records[idx]) {
				kept = append(kept, idx)
			}
		}
		candidates = kept
	}
	if len(candidates) == 0 {
		return Resolution{Route: RouteReview}
	}

	distinct := append(distinctiveTokens(qTokens, producerTokens), initialsTokens(norm)...)
	matches := make([]Match, 0, len(candidates))
	for _, idx := range candidates {
		rec := db.records[idx]
		recNorm := normalizeName(rec.DisplayName())
		score := tokenSetRatio(norm, recNorm)
		matches = append(matches, Match{Record: rec, Score: score})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		// Jaro-Winkler tiebreak on the full normalized strings, then LWIN7
		// for full determinism.
		ci, cj := coverage(distinct, matches[i].Record), coverage(distinct, matches[j].Record)
		if ci != cj {
			return ci > cj
		}
		ji := jaroWinkler(norm, normalizeName(matches[i].Record.DisplayName()))
		jj := jaroWinkler(norm, normalizeName(matches[j].Record.DisplayName()))
		if ji != jj {
			return ji > jj
		}
		return matches[i].Record.LWIN7 < matches[j].Record.LWIN7
	})
	if len(matches) > maxC {
		matches = matches[:maxC]
	}

	best := matches[0]
	res := Resolution{Best: best, Candidates: matches}
	switch {
	case best.Score >= auto && producerAgrees(qTokens, best.Record) && coverage(distinct, best.Record) >= minAutoCoverage &&
		!uncoveredNamesSibling(distinct, best.Record, db, candidates):
		res.Route = RouteAuto
	case best.Score >= review:
		res.Route = RouteAdjudicate
	default:
		res.Route = RouteReview
	}
	return res
}

// --- text normalization ---

// accentFold maps the accented characters that actually occur in wine
// producer/appellation names to ASCII. A full Unicode NFD fold would pull in
// x/text; this table covers the practical set and is trivially extendable.
var accentFold = strings.NewReplacer(
	"à", "a", "â", "a", "ä", "a", "á", "a", "ã", "a",
	"ç", "c",
	"è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "î", "i", "ï", "i", "í", "i",
	"ñ", "n",
	"ò", "o", "ô", "o", "ö", "o", "ó", "o", "õ", "o", "ø", "o",
	"ù", "u", "û", "u", "ü", "u", "ú", "u",
	"ý", "y", "ÿ", "y",
	"æ", "ae", "œ", "oe", "ß", "ss",
)

// aliases expands common producer-word abbreviations AFTER accent folding and
// lowercasing, keyed by whole token.
var aliases = map[string]string{
	"ch":   "chateau",
	"ch.":  "chateau",
	"chat": "chateau",
	"dom":  "domaine",
	"dom.": "domaine",
	"bdx":  "bordeaux",
	"cab":  "cabernet",
	"sauv": "sauvignon",
}

var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeName lowercases, accent-folds, expands abbreviations, and
// collapses punctuation to single spaces. Pure text transformation over data.
func normalizeName(s string) string {
	s = strings.ToLower(s)
	s = accentFold.Replace(s)
	// Expand dotted abbreviations before punctuation stripping eats the dot.
	fields := strings.Fields(s)
	for i, f := range fields {
		if repl, ok := aliases[f]; ok {
			fields[i] = repl
		}
	}
	s = strings.Join(fields, " ")
	s = nonAlnumRe.ReplaceAllString(s, " ")
	// Second alias pass for tokens that only became bare after punctuation
	// stripping ("ch." -> "ch ").
	fields = strings.Fields(s)
	for i, f := range fields {
		if repl, ok := aliases[f]; ok {
			fields[i] = repl
		}
	}
	return strings.Join(fields, " ")
}

// tokens splits a normalized name, dropping single-character noise.
// producerAgrees is the gate an AUTO route must also pass: every meaningful
// token of the record's producer appears in the query.
//
// Token-set similarity scores 100 whenever the record's name is a SUBSET of the
// query. Against the full Liv-ex dictionary that is most of the time: measured
// on nagus's 91 live wine listings (2026-09-21), 48 of 49 auto matches were
// wrong -- "2021 The Estates Merlot, Oak Knoll" (Robert Mondavi) matched
// producer "A", wine "Merlot", Tasmania, because producer-storefront titles
// never name the producer and "A" normalizes to nothing. A listing that does
// not name the producer cannot be auto-identified from its title alone; it
// routes to adjudication instead, where the producer can be supplied.
//
// Generic words carry no identity ("Vineyard", "Winery", "The"), and a
// producer with no meaningful token left can never agree. Agreement must also
// rest on a DISTINCTIVE token -- one that is not a grape, colour, style word or
// article -- because the dictionary holds producers literally named
// "Chardonnay", "Malbec" and "Barbera": with the gate alone, "2024 Napa Valley
// Chardonnay" (Mondavi) still auto-matched producer "Chardonnay", Burgundy.
func producerAgrees(query []string, rec Record) bool {
	have := make(map[string]bool, len(query))
	for _, t := range query {
		have[t] = true
	}
	distinctive := 0
	for _, t := range tokens(normalizeName(rec.Producer)) {
		if genericProducerWords[t] {
			continue
		}
		if !have[t] {
			return false
		}
		if !wineVocabulary[t] {
			distinctive++
		}
	}
	return distinctive > 0
}

// wineVocabulary is vocabulary every wine listing shares -- grapes, colours,
// styles, articles -- and so cannot, on its own, name a producer.
var wineVocabulary = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		chardonnay malbec barbera merlot cabernet sauvignon franc pinot noir gris
		grigio blanc syrah shiraz zinfandel riesling grenache garnacha tempranillo
		sangiovese nebbiolo viognier roussanne marsanne mourvedre petite petit
		sirah verdot albarino gewurztraminer semillon chenin muscat moscato
		carmenere primitivo gamay dolcetto aglianico torrontes vermentino
		rose rosado rosato rouge red white bianco blanco rosso tinto
		sparkling brut reserve reserva riserva cuvee blend
		la le les de du des di del da el los las von der und et y`) {
		m[w] = true
	}
	return m
}()

// coverage is the share of the query's distinctive tokens the record's match
// text contains (1 when the query has none).
//
// Token-set similarity rewards a record whose name is a SUBSET of the title, so
// the shortest, most generic record wins ties: with the producer known,
// "2022 The Estates Merlot, Oak Knoll" still scored 100 against plain
// "Robert Mondavi Winery Merlot" (Napa Valley) -- a different wine. An AUTO
// route therefore requires the distinctive title tokens to be accounted for
// by the record (minAutoCoverage); a title that names something the record does
// not (Oak Knoll, Heritage Clone) routes to adjudication, and among tied
// candidates the one covering more of the title ranks first.
func coverage(distinct []string, rec Record) float64 {
	if len(distinct) == 0 {
		return 1
	}
	have := map[string]bool{}
	for _, t := range tokens(normalizeName(rec.matchText())) {
		have[t] = true
	}
	n := 0
	for _, t := range distinct {
		if have[t] {
			n++
		}
	}
	return float64(n) / float64(len(distinct))
}

// minAutoCoverage is the share of distinctive title tokens an AUTO match must
// account for. Not 1: listing titles carry marketing words no record has
// ("Commemorative"). At 0.8 one stray word in five passes, while a title naming
// a different vineyard, clone or district (two or more uncovered tokens) does
// not.
const minAutoCoverage = 0.8

// sizeOrNumberRe matches vintages, bottle sizes and other bare numbers, which
// never identify a wine by name.
var sizeOrNumberRe = regexp.MustCompile(`^\d+(\.\d+)?(ml|cl|l|lt|ltr)?$`)

// distinctiveTokens are the query tokens a correct record must account for:
// all of them except the producer the caller supplied, numbers and bottle
// sizes, and words that name no particular wine.
func distinctiveTokens(query, producer []string) []string {
	skip := map[string]bool{}
	for _, t := range producer {
		skip[t] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range query {
		if skip[t] || seen[t] || sizeOrNumberRe.MatchString(t) || coverageFillerWords[t] || genericProducerWords[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// uncoveredNamesSibling reports whether a distinctive title token the best
// record does NOT cover appears in another candidate record of the same
// producer. minAutoCoverage lets one stray word through, which is right for
// marketing words ("Commemorative") and wrong for identity: "2023 Hayne
// Vineyard Cabernet Sauvignon" (Turley) auto-matched Turley's plain Napa
// Valley Cabernet Sauvignon, leaving "hayne" uncovered -- while "hayne" names
// Turley's Hayne Vineyard Petite Syrah and Hayne Zinfandel. A word that
// distinguishes the producer's other wines is identity-bearing, so the match
// goes to adjudication instead.
func uncoveredNamesSibling(distinct []string, best Record, db *DB, candidates []int) bool {
	have := map[string]bool{}
	for _, t := range tokens(normalizeName(best.matchText())) {
		have[t] = true
	}
	var uncovered []string
	for _, t := range distinct {
		if !have[t] {
			uncovered = append(uncovered, t)
		}
	}
	if len(uncovered) == 0 {
		return false
	}
	producer := tokens(normalizeName(best.Producer))
	for _, idx := range candidates {
		rec := db.records[idx]
		if rec.LWIN7 == best.LWIN7 || !producerAgrees(producer, rec) {
			continue
		}
		sib := map[string]bool{}
		for _, t := range tokens(normalizeName(rec.matchText())) {
			sib[t] = true
		}
		for _, t := range uncovered {
			if sib[t] {
				return true
			}
		}
	}
	return false
}

// initialsTokens joins runs of single letters in a normalized title into one
// token -- "W.H Vineyard" normalizes to "w h vineyard", and tokens drops the
// single letters, so a vineyard designate named by initials would otherwise be
// invisible to coverage and collapse into the estate's generic record (it did:
// "2023 The Estates Cabernet Sauvignon, W.H Vineyard" auto-matched The Estates
// Cabernet Sauvignon, Oakville). The joined token is distinctive and is almost
// never in a record, which is the point: it forces adjudication.
func initialsTokens(norm string) []string {
	var out []string
	var run []string
	flush := func() {
		if len(run) >= 2 {
			out = append(out, strings.Join(run, ""))
		}
		run = run[:0]
	}
	for _, f := range strings.Fields(norm) {
		if len(f) == 1 {
			run = append(run, f)
			continue
		}
		flush()
	}
	flush()
	return out
}

// coverageFillerWords appear in listing titles without naming a wine.
var coverageFillerWords = map[string]bool{
	"and": true, "of": true, "ml": true, "cl": true, "lt": true, "magnum": true,
	"bottle": true, "btl": true, "half": true, "case": true, "pack": true,
	"750ml": true, "375ml": true, "1500ml": true,
}

// genericProducerWords appear in producer names without identifying one.
var genericProducerWords = map[string]bool{
	"the": true, "winery": true, "wines": true, "wine": true, "vineyard": true,
	"vineyards": true, "estate": true, "estates": true, "cellar": true,
	"cellars": true, "co": true, "company": true, "family": true,
}

func tokens(norm string) []string {
	var out []string
	for _, f := range strings.Fields(norm) {
		if len(f) >= 2 {
			out = append(out, f)
		}
	}
	return out
}

// --- similarity ---

// tokenSetRatio is the rapidfuzz/fuzzywuzzy token_set_ratio: compare the
// sorted token intersection against intersection+differences, taking the max
// of the three pairings. It handles reordered and partially-overlapping
// producer/wine strings far better than plain edit distance -- a retailer
// title carries vintage/size/marketing tokens an LWIN name lacks, and the
// intersection-anchored comparisons discount exactly that.
func tokenSetRatio(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	setA := map[string]bool{}
	for _, t := range ta {
		setA[t] = true
	}
	setB := map[string]bool{}
	for _, t := range tb {
		setB[t] = true
	}

	var inter, diffA, diffB []string
	for t := range setA {
		if setB[t] {
			inter = append(inter, t)
		} else {
			diffA = append(diffA, t)
		}
	}
	for t := range setB {
		if !setA[t] {
			diffB = append(diffB, t)
		}
	}
	sort.Strings(inter)
	sort.Strings(diffA)
	sort.Strings(diffB)

	s0 := strings.Join(inter, " ")
	s1 := strings.TrimSpace(s0 + " " + strings.Join(diffA, " "))
	s2 := strings.TrimSpace(s0 + " " + strings.Join(diffB, " "))

	r1 := levenshteinRatio(s0, s1)
	r2 := levenshteinRatio(s0, s2)
	r3 := levenshteinRatio(s1, s2)
	return maxF(r1, r2, r3)
}

// levenshteinRatio is the normalized similarity 100 * (1 - dist/maxLen).
// Both empty = 0 (an empty intersection carries no signal, so it must not
// score 100 against an empty diff).
func levenshteinRatio(a, b string) float64 {
	if a == "" && b == "" {
		return 0
	}
	la, lb := len(a), len(b)
	maxLen := la
	if lb > maxLen {
		maxLen = lb
	}
	d := levenshtein(a, b)
	return 100 * (1 - float64(d)/float64(maxLen))
}

// levenshtein computes edit distance with the two-row dynamic program.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = minI(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// jaroWinkler computes the Jaro-Winkler similarity (0-1), used only as a
// deterministic tiebreak between equal token-set scores; near-identical
// short strings with a shared prefix win.
func jaroWinkler(a, b string) float64 {
	j := jaro(a, b)
	// Winkler prefix boost: up to 4 shared leading characters, p=0.1.
	prefix := 0
	for i := 0; i < len(a) && i < len(b) && i < 4; i++ {
		if a[i] != b[i] {
			break
		}
		prefix++
	}
	return j + float64(prefix)*0.1*(1-j)
}

func jaro(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 && lb == 0 {
		return 1
	}
	if la == 0 || lb == 0 {
		return 0
	}
	window := maxI(la, lb)/2 - 1
	if window < 0 {
		window = 0
	}
	matchA := make([]bool, la)
	matchB := make([]bool, lb)
	matches := 0
	for i := 0; i < la; i++ {
		lo := maxI(0, i-window)
		hi := minI2(lb-1, i+window)
		for j := lo; j <= hi; j++ {
			if matchB[j] || ra[i] != rb[j] {
				continue
			}
			matchA[i], matchB[j] = true, true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}
	transpositions := 0
	j := 0
	for i := 0; i < la; i++ {
		if !matchA[i] {
			continue
		}
		for !matchB[j] {
			j++
		}
		if ra[i] != rb[j] {
			transpositions++
		}
		j++
	}
	m := float64(matches)
	t := float64(transpositions) / 2
	return (m/float64(la) + m/float64(lb) + (m-t)/m) / 3
}

func maxF(vals ...float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func minI(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func minI2(a, b int) int {
	if b < a {
		return b
	}
	return a
}

func maxI(a, b int) int {
	if b > a {
		return b
	}
	return a
}
