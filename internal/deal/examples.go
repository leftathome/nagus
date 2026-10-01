package deal

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/leftathome/nagus/internal/listing"
)

// ExampleWine and ExampleHDD are the canonical examples: one line each,
// exactly as a sender pastes them. TestExamplesDecode keeps them valid; the
// spec tool and docs/deal-submission.md show them.
const (
	ExampleWine = `{"category":"wine","title":"Example Cellars Columbia Valley Syrah 2021","brand":"Example Cellars","vintage":2021,"bottle_ml":750,"price":"24.99","url":"https://shop.example.com/syrah-2021","seller":"Example Wine Shop","note":"in store only, ends Sunday"}`
	ExampleHDD  = `{"category":"hdd","title":"Example Digital 20TB SATA 7200rpm enterprise drive","brand":"Example Digital","mpn":"EX20T-0001","capacity_tb":20,"condition":"refurb","price":219.99,"url":"https://store.example.com/p/ex20t","seller":"Example Store"}`
)

// ExampleBody is a whole message body: two deals between ordinary prose,
// which is ignored.
const ExampleBody = "Two deals from today:\n\n" + ExampleWine + "\n" + ExampleHDD + "\n\nThanks!\n"

// Examples returns the examples decoded, for structured output.
func Examples() []map[string]any {
	var out []map[string]any
	for _, l := range []string{ExampleWine, ExampleHDD} {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			panic("deal: example is not JSON: " + err.Error())
		}
		out = append(out, m)
	}
	return out
}

// Aspect keys a deal sets on its listing.
const (
	AspectSubmittedBy = "submitted_by"
	AspectSchema      = "deal_schema"
	AspectDealGTIN    = "deal_gtin"
	// AspectURLText is the url path and query, percent-decoded, as text for
	// the glovebox gate (which scans aspects) (rv35 I2).
	AspectURLText = "deal_url_text"
)

// MaxURLDecodeRounds bounds repeated percent-decoding of a url for the gate:
// "%2569" is "%69" is "i". A url still holding an escape after this many
// rounds is refused rather than gated half-decoded.
const MaxURLDecodeRounds = 2

var pctRe = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)

// urlGateText is what the glovebox gate reads for a url (the gate scans
// aspects; nagus stores this as AspectURLText): the host, then the path,
// query and fragment percent-decoded until stable, then the same text again
// with every separator turned into a space, so "ignore+previous" and
// "ignore%2520previous" and "ignore/previous" all read as words.
//
// It FAILS CLOSED (rv35b NEW-1): ok is false, and the url is refused, when
// any "%" is not a valid escape, when escapes are nested deeper than
// MaxURLDecodeRounds, or when the decoded text is not valid, visible text.
// "Decode failed" never means "not gated".
func urlGateText(raw string) (string, bool) {
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		return "", false
	}
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, rest = rest[:i], rest[i:]
	} else {
		rest = ""
	}
	// Every % must begin a valid escape.
	if strings.Count(rest, "%") != len(pctRe.FindAllStringIndex(rest, -1)) {
		return "", false
	}
	decoded := rest
	for round := 0; pctRe.MatchString(decoded); round++ {
		if round == MaxURLDecodeRounds {
			return "", false
		}
		decoded = pctRe.ReplaceAllStringFunc(decoded, func(e string) string {
			b, _ := strconv.ParseUint(e[1:], 16, 8)
			return string([]byte{byte(b)})
		})
	}
	if !utf8.ValidString(decoded) || !cleanText(decoded, len(decoded)) {
		return "", false
	}
	words := strings.Join(strings.FieldsFunc(decoded, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}), " ")
	return strings.TrimSpace(host + " " + decoded + " " + words), true
}

// ToRaw maps a validated deal onto a listing so the existing category
// extractors, the glovebox gate and the quark hint path handle it unchanged.
// key is the source key (message id and line); principal is who submitted it.
// SourceID and SeenAt are the connector's to fill.
func (d Deal) ToRaw(key, principal string) listing.Raw {
	cents, _ := d.Price.Cents()
	a := map[string]string{
		AspectSubmittedBy: principal,
		AspectSchema:      SchemaID,
	}
	set := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			a[k] = v
		}
	}
	set("seller", d.Seller)
	// The url is free text that reaches agents: its path and query, decoded,
	// cross the glovebox gate as an aspect (the gate scans aspects).
	if text, ok := urlGateText(d.URL); ok {
		set(AspectURLText, text)
	}
	switch d.Category {
	case CategoryHDD:
		// brand/mpn/gtin are the offer's product hint (pipeline offerFromRaw):
		// how quark resolves a drive.
		set("brand", d.Brand)
		set("mpn", d.MPN)
		set("gtin", d.GTIN)
		if d.CapacityTB != nil {
			a["capacity_tb"] = strconv.FormatFloat(*d.CapacityTB, 'f', -1, 64)
		}
	case CategoryWine:
		// The producer and the title are the quark NAME hint (the ingester's
		// NameHintProducer = "wine_producer"). A wine GTIN is kept apart: as
		// "gtin" it would take quark's key path and mint a product beside
		// the LWIN catalog's.
		set("wine_producer", d.Brand)
		set(AspectDealGTIN, d.GTIN)
		if d.Vintage != nil {
			a["vintage"] = strconv.Itoa(*d.Vintage)
		}
		if d.BottleML != nil {
			a["bottle_ml"] = strconv.Itoa(*d.BottleML)
		}
	}
	return listing.Raw{
		SourceKey:    key,
		SourceURL:    d.URL,
		Title:        strings.TrimSpace(d.Title),
		Body:         strings.TrimSpace(d.Note),
		PriceCents:   cents,
		Currency:     d.Currency,
		ConditionRaw: d.Condition,
		Aspects:      a,
	}
}
