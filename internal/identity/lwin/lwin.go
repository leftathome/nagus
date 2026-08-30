// Package lwin resolves wine listings to LWIN identifiers -- the wine
// category's canonical ID (design section 5's canonical_id extractor slot).
//
// LWIN is Liv-ex's Creative-Commons-licensed universal wine identifier: an
// LWIN-7 names a producer/wine, LWIN-11 appends a 4-digit vintage, LWIN-16
// appends a 5-digit bottle size in ml. The database itself is a free download
// (registration form at liv-ex.com/lwin/), refreshed quarterly; this package
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
)

// Record is one LWIN-7 entry: a producer/wine with its geography and colour.
type Record struct {
	LWIN7    string // 7-digit producer/wine code
	Producer string
	Wine     string
	Country  string
	Region   string
	Colour   string // red | white | rose | ...
}

// DisplayName is the record's resolvable name: producer + wine.
func (r Record) DisplayName() string {
	if r.Wine == "" {
		return r.Producer
	}
	return r.Producer + " " + r.Wine
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
		for _, tok := range tokens(normalizeName(r.DisplayName())) {
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
// ignored so a fuller export loads without modification.
func LoadCSV(r io.Reader) (*DB, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("lwin: reading header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToUpper(strings.TrimSpace(h))] = i
	}
	get := func(row []string, names ...string) string {
		for _, n := range names {
			if i, ok := col[n]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
		}
		return ""
	}
	if _, ok := col["LWIN"]; !ok {
		return nil, fmt.Errorf("lwin: header has no LWIN column (got %v)", header)
	}

	var records []Record
	for {
		row, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("lwin: reading row: %w", err)
		}
		records = append(records, Record{
			LWIN7:    get(row, "LWIN"),
			Producer: get(row, "PRODUCER_NAME", "PRODUCER"),
			Wine:     get(row, "WINE"),
			Country:  get(row, "COUNTRY"),
			Region:   get(row, "REGION"),
			Colour:   strings.ToLower(get(row, "COLOUR", "COLOR")),
		})
	}
	return NewDB(records), nil
}

// Resolver resolves queries against a DB with confidence routing.
type Resolver struct {
	DB *DB
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
	if r.DB == nil || r.DB.Len() == 0 {
		return Resolution{Route: RouteReview}
	}
	norm := normalizeName(q.Name)
	qTokens := tokens(norm)
	if len(qTokens) == 0 {
		return Resolution{Route: RouteReview}
	}

	// Blocking: candidates sharing at least one query token. This bounds
	// scoring to a tiny fraction of a ~100k-record DB.
	seen := map[int]bool{}
	var candidates []int
	for _, tok := range qTokens {
		for _, idx := range r.DB.tokenIdx[tok] {
			if !seen[idx] {
				seen[idx] = true
				candidates = append(candidates, idx)
			}
		}
	}
	if len(candidates) == 0 {
		return Resolution{Route: RouteReview}
	}

	matches := make([]Match, 0, len(candidates))
	for _, idx := range candidates {
		rec := r.DB.records[idx]
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
	case best.Score >= auto:
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
