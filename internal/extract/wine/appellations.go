package wine

import (
	"regexp"
	"strings"
)

// APPELLATIONS AS WINE EVIDENCE (nagus-tmr).
//
// Old World wines are named by place, not grape: "Castello di Ama Chianti
// Classico", "Barolo Vietti Castiglione", "Sancerre Vacheron" carry no
// varietal, no colour word and often no year. The place is the evidence.
//
// The names come from two tables:
//
//   - lwinAppellations (appellations_lwin.go), generated offline by
//     tools/genappellations from the Liv-ex LWIN export (CC BY 4.0): the
//     REGION and SUB_REGION names of at least 20 live wines, minus ordinary
//     words, political places and names other rules own (port, champagne).
//   - appellationSupplement below, kept by hand: short forms and names LWIN
//     files under a wider region ("Etna" beside LWIN's "Etna Rosso",
//     "Amarone" beside "Amarone della Valpolicella").
//
// Each name has a tier. An APPELLATION (Barolo, Chablis, Rioja) is wine
// evidence on its own. A BROAD name -- a large region (Burgundy, Tuscany,
// Bordeaux), a New World AVA (Napa Valley, Willamette Valley) or an
// appellation that doubles as a common word (Graves, Quincy) -- is evidence
// only beside a supporting cue: a classification token (DOC, AOC, Grand Cru,
// Riserva ...) or a bare colour word (Red, Rosso, Rouge, Blanc ...). Broad
// names alone are merchandise and events far too often: "Tuscany Candle",
// "Napa Valley Tote", "Sonoma County Tasting".
//
// An appellation cue is withdrawn, like the fortified cue, when the title is
// a spirit or beer finished in its casks ("Sauternes Cask Finish"), an event
// or printed matter ("Barolo Tasting Dinner", "Bordeaux Map"), or a food
// ("Chianti Cooking Sauce", which is then culinary). The merchandise lists
// run first and still reject "Bordeaux Wine Glass" and "Burgundy Leather
// Wallet".
//
// Matching is on accent-folded, whole-word phrases: a title and every name
// are normalized the same way (normalizeWords) and compared word by word, so
// "Chateauneuf-du-Pape", "Chateauneuf du Pape" with a circumflex, and
// "CHATEAUNEUF DU PAPE" are
// one name, and "Toro" never matches inside "Torontal".

// appellationSupplement are hand-kept names; true marks a broad one.
var appellationSupplement = map[string]bool{
	// Short forms LWIN files only under a longer name.
	"etna":          false, // LWIN: Etna Rosso / Etna Bianco / Etna Rosato
	"brunello":      true,  // Brunello di Montalcino; also a fashion house
	"amarone":       false, // Amarone della Valpolicella
	"ripasso":       false, // Valpolicella Ripasso
	"recioto":       false, // Recioto della Valpolicella / di Soave
	"vino nobile":   false, // Vino Nobile di Montepulciano
	"morellino":     false, // Morellino di Scansano
	"chateauneuf":   false, // Chateauneuf-du-Pape
	"sassicaia":     false, // Bolgheri Sassicaia DOC
	"lambrusco":     false, // Lambrusco di Sorbara and siblings
	"tokaji":        false, // the wine of Tokaj
	"cremant":       false, // Cremant d'Alsace / de Bourgogne / de Loire ...
	"txakoli":       false, // Getariako Txakolina
	"txakolina":     false,
	"priorato":      false, // Priorat, Spanish spelling
	"sudtirol":      false, // Alto Adige, German name
	"sud tirol":     false,
	"asti spumante": false, // LWIN: Asti, 17 wines
	// Real appellations under LWIN's 20-wine threshold.
	"muscadet":          false, // 12
	"brachetto d acqui": false, // 18
	"carmignano":        false, // 16
	"ghemme":            false, // 19
	"frascati":          false, // 16
	// Towns that are also appellations, broad by the same reasoning as the
	// generated table's.
	"montalcino": true,
	"napa":       true,
	"sonoma":     true,
}

// appellationMaxWords is the longest name, in words, either table holds.
var appellationMaxWords = func() int {
	n := 0
	for _, m := range []map[string]bool{lwinAppellations, appellationSupplement} {
		for name := range m {
			if w := len(strings.Fields(name)); w > n {
				n = w
			}
		}
	}
	return n
}()

// appellationTier looks a normalized phrase up: known, and whether broad.
func appellationTier(phrase string) (known, broad bool) {
	if b, ok := appellationSupplement[phrase]; ok {
		return true, b
	}
	b, ok := lwinAppellations[phrase]
	return ok, b
}

// spelledOut are the abbreviations normalizeWords expands, so "St. Emilion"
// and "Saint-Emilion" are one name.
var spelledOut = map[string]string{"st": "saint", "ste": "sainte", "mt": "mount"}

// normalizeWords folds accents, lower-cases, and splits on every run of
// characters that is not an ASCII letter or digit. "&" and "+" become the
// word "and" (the culinary rule reads them as conjunctions). It must agree
// with tools/genappellations' Normalize (TestAppellationTableIsNormalized).
func normalizeWords(s string) []string {
	s = foldASCII(strings.ToLower(s))
	s = strings.NewReplacer("&", " and ", "+", " and ").Replace(s)
	words := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	for i, w := range words {
		if l, ok := spelledOut[w]; ok {
			words[i] = l
		}
	}
	return words
}

// phraseMatch is one name found in a word list: words[start:end].
type phraseMatch struct {
	start, end int
	broad      bool
}

// findAppellations scans words left to right, taking the LONGEST known name
// at each position ("chianti classico" over "chianti").
func findAppellations(words []string) []phraseMatch {
	var out []phraseMatch
	for i := 0; i < len(words); {
		matched := false
		for n := min(appellationMaxWords, len(words)-i); n >= 1; n-- {
			if known, broad := appellationTier(strings.Join(words[i:i+n], " ")); known {
				out = append(out, phraseMatch{start: i, end: i + n, broad: broad})
				i += n
				matched = true
				break
			}
		}
		if !matched {
			i++
		}
	}
	return out
}

// classificationRe is a quality or origin classification: a SUPPORTING cue
// only, never evidence alone ("Reserva" is also rum, "Grand Cru" also
// chocolate). The acronyms are matched case-sensitively: "DO" and "DOC" in
// lower case are English words and names.
var (
	classificationAcronymRe = regexp.MustCompile(`\b(DOCG|DOCa|DOC|DOQ|DO|AOC|AOP|IGT|IGP|VdP|D\.O\.(C\.)?(G\.)?)(\s|$|[^A-Za-z0-9])`)
	classificationWordRe    = regexp.MustCompile(`(?i)\b(grand cru|premier cru|1er cru|gran reserva|reserva|riserva|gran selezione|crianza|vin de pays|vino de la tierra)\b`)
)

// hasClassification reports a classification token in a title.
func hasClassification(title string) bool {
	folded := foldASCII(title)
	return classificationAcronymRe.MatchString(folded) || classificationWordRe.MatchString(folded)
}

// bareColours are colour words that are NOT wine evidence on their own (a
// "Red" hat, a "White" tee), but name the wine's colour beside an
// appellation or an NV marker: "Corvee de Trousseau Arbois Red NV",
// "Bourgogne Rouge", "Etna Rosso".
var bareColours = map[string]string{
	"red": "red", "rosso": "red", "rouge": "red", "tinto": "red",
	"white": "white", "blanco": "white", "bianco": "white", "blanc": "white",
	"rosado": "rose", "rosato": "rose",
}

// englishColours are the bare colours that are also English colour names:
// beside a region that is itself a colour name ("Burgundy Red", "Bordeaux
// White" are paint and fabric), they support nothing.
var englishColours = setOfWords("red", "white")

// colourNamedRegions are broad regions that are English colour names.
var colourNamedRegions = setOfWords("burgundy", "bordeaux")

// bareColour returns the LAST bare colour word in words, or "": a wine's
// colour follows its name ("Melon a Queue Rouge ... White 2022" is a white).
func bareColour(words []string) string {
	c := ""
	for _, w := range words {
		if cc, ok := bareColours[w]; ok {
			c = cc
		}
	}
	return c
}

// inSpan reports whether words index i falls inside any match.
func inSpan(ms []phraseMatch, i int) bool {
	for _, m := range ms {
		if m.start <= i && i < m.end {
			return true
		}
	}
	return false
}

// eventWords mark a title as an event, a service or printed matter: an
// appellation there is its subject, not the bottle ("Barolo Tasting Dinner",
// "Burgundy Harvest Tour", "Map of Bordeaux").
var eventWords = setOfWords("dinner", "lunch", "brunch", "tasting", "tastings", "tour", "tours", "class",
	"classes", "seminar", "webinar", "event", "events", "experience", "festival", "trip", "travel",
	"book", "books", "map", "maps", "poster", "print", "prints", "puzzle", "guide", "course",
	"cruise", "cruises", "vacation", "vacations", "visit", "visits", "hike", "hikes", "museum",
	"voucher", "vouchers", "package", "packages", "cookbook", "cookbooks")

// spiritWords are the whole-word spirit and beer names
// ("single malt" is two words; see spiritTitle).
var spiritWords = setOfWords("whisky", "whiskey", "scotch", "bourbon", "rum", "gin", "vodka", "tequila",
	"mezcal", "brandy", "cognac", "armagnac", "malt", "stout", "porter", "ale", "ipa", "lager", "beer", "cider")

// caskBothSides are cask words that name a cask finish on either side of a
// wine word; oakWords only AFTER it ("Sherry Oak" is a cask, "Oak Aged Port"
// is a port).
var (
	caskBothSides = setOfWords("cask", "casks", "barrel", "barrels", "finish", "finished")
	oakWords      = setOfWords("oak", "wood")
)

func setOfWords(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// nearCask reports a cask word within two words of words[start:end]: a
// cask/barrel/finish word on either side, or oak/wood after.
func nearCask(words []string, start, end int) bool {
	for i := max(0, start-2); i < start; i++ {
		if caskBothSides[words[i]] {
			return true
		}
	}
	for i := end; i < min(len(words), end+2); i++ {
		if caskBothSides[words[i]] || oakWords[words[i]] {
			return true
		}
	}
	return false
}

// spiritTitle reports a spirit or beer word in words that is not part of a
// producer name directly before the wine cue at cueStart. "Porter Creek
// Tawny Port", "Rum Runner Port", "Gin Lane Port": a spirit word followed by
// one to three plain name words and then the cue names the producer, not the
// drink. Anywhere else -- after the cue ("Sherry Cask Bourbon"), directly
// before it ("Rum Port"), or with a cask, spirit or number word between
// ("Bourbon Barrel Aged Port", "Scotch Whisky Port") -- it is the drink.
func spiritTitle(words []string, cueStart int) bool {
	for i, w := range words {
		if !spiritWords[w] {
			continue
		}
		if w == "malt" && (i == 0 || words[i-1] != "single") {
			continue
		}
		if i < cueStart && producerName(words[i+1:cueStart]) {
			continue
		}
		return true
	}
	return false
}

// producerName reports 1-3 words that can be a producer name's tail: no
// spirit, cask or oak word and no number.
func producerName(ws []string) bool {
	if len(ws) < 1 || len(ws) > 3 {
		return false
	}
	for _, w := range ws {
		if spiritWords[w] || caskBothSides[w] || oakWords[w] || w == "aged" || strings.ContainsAny(w, "0123456789") {
			return false
		}
	}
	return true
}

// appellationEvidence is what a title's place names say.
type appellationEvidence struct {
	// wine: an appellation, or a broad name beside a supporting cue, that
	// no guard withdrew.
	wine bool
	// culinary: a place name withdrawn because the title is a food made
	// with the wine ("Chianti Cooking Sauce").
	culinary bool
	// colour: the bare colour word the title gives its wine, when wine is
	// set: one inside an appellation's own name first ("Etna Bianco Red
	// Label" is a white), else the last one outside every place name
	// ("Arbois Red"). A colour inside a broad name ("Red Mountain", "Red
	// Hills Lake County") is a place, not a colour.
	colour string
}

// titleAppellations judges a title's place names.
func titleAppellations(title string) appellationEvidence {
	words := normalizeWords(title)
	matches := findAppellations(words)
	if len(matches) == 0 {
		return appellationEvidence{}
	}
	var ev appellationEvidence
	for _, w := range words {
		if eventWords[w] {
			return ev
		}
	}
	classified := hasClassification(title)
	for _, m := range matches {
		if m.broad && !classified && !colourBeside(words, m, colourNamedRegions[strings.Join(words[m.start:m.end], " ")]) {
			continue
		}
		if nearCask(words, m.start, m.end) || spiritTitle(words, m.start) {
			continue
		}
		if culinaryWordRe.MatchString(strings.Join(words, " ")) {
			ev.culinary = true
			return ev
		}
		ev.wine = true
		break
	}
	if !ev.wine {
		return ev
	}
	var outside, inside string
	for i, w := range words {
		c, ok := bareColours[w]
		switch {
		case !ok:
		case !inSpan(matches, i):
			outside = c
		default:
			for _, m := range matches {
				if !m.broad && m.start <= i && i < m.end {
					inside = c
				}
			}
		}
	}
	ev.colour = inside
	if ev.colour == "" {
		ev.colour = outside
	}
	return ev
}

// colourBeside reports a bare colour word NEXT TO broad match m -- right
// before or after it, or with one word between ("Red Mountain Pioneer Red")
// -- in a title with no object noun: the supporting cue a broad name needs.
// "Burgundy Blanc Throw Pillow" and "Napa Valley Cap Red" are home goods and
// clothes in a colour. noEnglish drops "red" and "white" (englishColours).
func colourBeside(words []string, m phraseMatch, noEnglish bool) bool {
	for _, w := range words {
		if broadMerchNouns[w] || objectNouns[w] {
			return false
		}
	}
	for i, w := range words {
		if i >= m.start && i < m.end {
			continue
		}
		if i < m.start-2 || i > m.end+1 {
			continue
		}
		if _, ok := bareColours[w]; ok && !(noEnglish && englishColours[w]) {
			return true
		}
	}
	return false
}

// broadMerchNouns are things sold "in Burgundy" or "in Bordeaux Blanc": a
// broad region's colour cue does not count in a title that names one.
var broadMerchNouns = setOfWords("cap", "caps", "pillow", "pillows", "throw", "throws", "blanket", "blankets",
	"swatch", "swatches", "paint", "paints", "scarf", "scarves", "bag", "bags", "napkin", "napkins", "rug",
	"rugs", "fabric", "yarn", "lipstick", "polish", "dress", "jacket", "vest", "coat", "shoes", "tie",
	"ribbon", "balloon", "balloons", "frame", "cushion", "cushions", "curtain", "curtains", "sofa", "chair",
	"bowl", "bowls", "velvet")
