// Package deal is the nagus.deal/v1 submission format: one JSON object per
// line in the text/plain body of a mail to the deals mailbox, sent by a known
// household human or agent (design: docs/design/2026-09-26-deal-submission-jsonl.md).
//
// The package owns the whole format: the Go struct (the single source of
// truth), the strict per-line decoder and validator, the JSON Schema generated
// from the struct, the canonical examples, the mapping onto a listing.Raw, the
// imap parser that reads a message, and the in-memory status ledger behind the
// deal_submission_status MCP tool and the submission counters.
//
// A deal line is UNTRUSTED text from an open channel. Decoding is strict and
// deterministic -- no LLM, no guessing -- so the worst a malicious line can do
// is produce a wrong field value, and every text field still crosses the
// glovebox sanitize gate like any listing before it becomes an item.
package deal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SchemaID is the format's name, and the only value the optional "schema"
// field may carry.
const SchemaID = "nagus.deal/v1"

// Categories a deal may name. Each needs a deal source configured for it.
const (
	CategoryWine = "wine"
	CategoryHDD  = "hdd"
)

// Categories is every category the format accepts, in schema order.
var Categories = []string{CategoryWine, CategoryHDD}

// Limits. The schema generator and the validator read the SAME constants, so
// the published schema cannot promise something the validator does not do.
const (
	MaxLinesPerMessage = 50
	MaxLineBytes       = 4096
	MaxTitleLen        = 300
	MaxNoteLen         = 1000
	MaxNameLen         = 100 // seller, brand
	MaxMPNLen          = 64
	MaxURLLen          = 2048
	MinVintage         = 1800
	MaxVintage         = 2100
	MinBottleML        = 50
	MaxBottleML        = 30000
	MaxCapacityTB      = 1000
	// MaxPriceMajor bounds a price in major units (dollars, euros).
	MaxPriceMajor = 1000000
	// DefaultCurrency applies when a line names none.
	DefaultCurrency = "USD"
)

// PricePattern is the textual form a price must take, as a JSON string or a
// JSON number: whole major units with at most two decimals. No exponent, no
// sign, no thousands separator.
const PricePattern = `^[0-9]{1,7}(\.[0-9]{1,2})?$`

// CurrencyPattern is an ISO 4217 alphabetic code.
const CurrencyPattern = `^[A-Z]{3}$`

// GTINPattern is a GTIN-8, UPC-A (12), EAN-13 or GTIN-14.
const GTINPattern = `^([0-9]{8}|[0-9]{12,14})$`

// CategoryOnly lists the fields that apply to ONE category; on a line of any
// other category they refuse the line (field_not_for_category). The schema's
// if/then rules are generated from the same table.
var CategoryOnly = map[string][]string{
	CategoryWine: {"vintage", "bottle_ml"},
	CategoryHDD:  {"mpn", "capacity_tb", "condition"},
}

// has reports whether the named category-only field is set.
func (d *Deal) has(field string) bool {
	switch field {
	case "vintage":
		return d.Vintage != nil
	case "bottle_ml":
		return d.BottleML != nil
	case "mpn":
		return d.MPN != ""
	case "capacity_tb":
		return d.CapacityTB != nil
	case "condition":
		return d.Condition != ""
	}
	panic("deal: " + field + " is not a category-only field")
}

// Conditions an hdd deal may state, in the hdd extractor's own vocabulary.
var Conditions = []string{"new", "refurb", "used", "parts"}

var (
	priceRe    = regexp.MustCompile(PricePattern)
	currencyRe = regexp.MustCompile(CurrencyPattern)
	gtinRe     = regexp.MustCompile(GTINPattern)
)

// Price is a price in major units, given as a decimal string ("24.99") or a
// JSON number (24.99). It keeps the literal text, so a number is validated
// exactly as written: 12.999 and 1e3 are refused rather than rounded.
type Price string

// UnmarshalJSON accepts a JSON string or number. Anything else is a bad price.
func (p *Price) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return errBadPrice
		}
		*p = Price(s)
	case len(b) > 0 && (b[0] == '-' || (b[0] >= '0' && b[0] <= '9')):
		*p = Price(b)
	default:
		return errBadPrice
	}
	return nil
}

var errBadPrice = errors.New("deal: price must be a decimal string or number")

// Cents converts the price to minor units, refusing anything outside
// PricePattern, zero, or above MaxPriceMajor.
func (p Price) Cents() (int64, bool) {
	s := string(p)
	if !priceRe.MatchString(s) {
		return 0, false
	}
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	for len(frac) < 2 {
		frac += "0"
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, false
	}
	cents := w*100 + f
	if cents <= 0 || cents > MaxPriceMajor*100 {
		return 0, false
	}
	return cents, true
}

// Deal is one nagus.deal/v1 object. It is the single source of truth for the
// format: the JSON Schema is generated from it (schema.go) and the decoder
// accepts exactly its JSON field names.
//
// A `deal:"required"` tag marks a required field; the schema's "required"
// list is built from it and Decode enforces it.
type Deal struct {
	Schema     string   `json:"schema,omitempty"`
	Category   string   `json:"category" deal:"required"`
	Title      string   `json:"title" deal:"required"`
	Price      Price    `json:"price" deal:"required"`
	URL        string   `json:"url" deal:"required"`
	Currency   string   `json:"currency,omitempty"`
	Seller     string   `json:"seller,omitempty"`
	Brand      string   `json:"brand,omitempty"`
	MPN        string   `json:"mpn,omitempty"`
	GTIN       string   `json:"gtin,omitempty"`
	Vintage    *int     `json:"vintage,omitempty"`
	BottleML   *int     `json:"bottle_ml,omitempty"`
	CapacityTB *float64 `json:"capacity_tb,omitempty"`
	Condition  string   `json:"condition,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// Reason is a per-line (or per-message) outcome code. It is a closed set:
// reason codes are what the status tool and the metrics carry, and they are
// the ONLY thing reported about a line -- never its content.
type Reason string

// Line refusal reasons, in the order a line meets them.
const (
	ReasonNone                Reason = ""
	ReasonLineTooLong         Reason = "line_too_long"
	ReasonTooManyLines        Reason = "too_many_lines"
	ReasonBadJSON             Reason = "bad_json"
	ReasonUnknownField        Reason = "unknown_field"
	ReasonBadType             Reason = "bad_type"
	ReasonMissingField        Reason = "missing_field"
	ReasonBadSchema           Reason = "bad_schema"
	ReasonBadCategory         Reason = "bad_category"
	ReasonCategoryNotEnabled  Reason = "category_not_enabled"
	ReasonFieldNotForCategory Reason = "field_not_for_category"
	ReasonBadPrice            Reason = "bad_price"
	ReasonBadCurrency         Reason = "bad_currency"
	ReasonBadURL              Reason = "bad_url"
	ReasonBadValue            Reason = "bad_value"
	// Downstream of the parser: the glovebox gate and the category extractor.
	ReasonGateRefused     Reason = "gate_refused"
	ReasonNotInCategory   Reason = "not_in_category"
	ReasonExtractFailed   Reason = "extract_failed"
	ReasonGateUnavailable Reason = "gate_unavailable" // transient: retried next poll
	ReasonStoreFailed     Reason = "store_failed"     // transient
	ReasonOfferFailed     Reason = "offer_store_failed"
)

// LineReasons is every per-line reason code with its meaning, for the spec
// tool, the docs and the metrics' bounded label set.
var LineReasons = []struct {
	Code    Reason
	Final   bool // false: the line stays pending and is retried next poll
	Meaning string
}{
	{ReasonLineTooLong, true, fmt.Sprintf("the line is longer than %d bytes", MaxLineBytes)},
	{ReasonTooManyLines, true, fmt.Sprintf("more than %d deal lines in one message; only the first %d are read", MaxLinesPerMessage, MaxLinesPerMessage)},
	{ReasonBadJSON, true, "not one valid JSON object (a hard-wrapped line lands here)"},
	{ReasonUnknownField, true, "a field the schema does not define, or a known field spelled with different case"},
	{ReasonBadType, true, "a field has the wrong JSON type (e.g. vintage as a string)"},
	{ReasonMissingField, true, "category, title, price or url is missing or empty"},
	{ReasonBadSchema, true, "the schema field is present but is not " + SchemaID},
	{ReasonBadCategory, true, "category is not wine or hdd"},
	{ReasonCategoryNotEnabled, true, "that category is valid but not enabled for submission here"},
	{ReasonFieldNotForCategory, true, "a field that does not apply to this category (vintage on hdd, capacity_tb on wine)"},
	{ReasonBadPrice, true, fmt.Sprintf("price is not a positive amount with at most 2 decimals, up to %d", MaxPriceMajor)},
	{ReasonBadCurrency, true, "currency is not an ISO 4217 code such as USD"},
	{ReasonBadURL, true, "url is not an https URL with a host"},
	{ReasonBadValue, true, "a value is out of range, too long, or contains control characters"},
	{ReasonGateRefused, true, "the glovebox sanitize gate refused the line's text"},
	{ReasonNotInCategory, true, "the category extractor says it is not an item of that category (e.g. an SSD as hdd)"},
	{ReasonExtractFailed, true, "the category extractor could not form an item"},
	{ReasonGateUnavailable, false, "the sanitize gate was unavailable; retried on the next poll"},
	{ReasonStoreFailed, false, "the item store failed; retried on the next poll"},
	{ReasonOfferFailed, false, "the offer store failed; retried on the next poll"},
}

// fieldNames are the exact JSON keys of Deal.
var fieldNames = jsonFields()

func jsonFields() map[string]bool {
	out := map[string]bool{}
	for _, f := range structFields() {
		out[f.name] = true
	}
	return out
}

// Decode parses and validates ONE deal line strictly. It returns the deal, or
// the reason it was refused. The line has already been trimmed and is known
// to start with '{'.
func Decode(line []byte) (Deal, Reason) {
	if len(line) > MaxLineBytes {
		return Deal{}, ReasonLineTooLong
	}
	if !utf8.Valid(line) {
		return Deal{}, ReasonBadJSON
	}
	// Keys first, EXACTLY: encoding/json matches keys case-insensitively, so
	// "Title" would otherwise fill Title. The raw map also rejects a line that
	// is not an object at all.
	var keys map[string]json.RawMessage
	if err := strictUnmarshal(line, &keys); err != nil {
		return Deal{}, ReasonBadJSON
	}
	for k := range keys {
		if !fieldNames[k] {
			return Deal{}, ReasonUnknownField
		}
	}
	var d Deal
	if err := strictUnmarshal(line, &d); err != nil {
		var te *json.UnmarshalTypeError
		switch {
		case errors.Is(err, errBadPrice):
			return Deal{}, ReasonBadPrice
		case errors.As(err, &te):
			return Deal{}, ReasonBadType
		default:
			return Deal{}, ReasonBadJSON
		}
	}
	if r := d.validate(); r != ReasonNone {
		return Deal{}, r
	}
	return d, ReasonNone
}

// strictUnmarshal decodes exactly one JSON value with no trailing data.
func strictUnmarshal(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("deal: trailing data after the object")
	}
	return nil
}

// validate applies every value rule after a successful decode. It also fills
// the currency default.
func (d *Deal) validate() Reason {
	if strings.TrimSpace(d.Category) == "" || strings.TrimSpace(d.Title) == "" ||
		d.Price == "" || strings.TrimSpace(d.URL) == "" {
		return ReasonMissingField
	}
	if d.Schema != "" && d.Schema != SchemaID {
		return ReasonBadSchema
	}
	if d.Category != CategoryWine && d.Category != CategoryHDD {
		return ReasonBadCategory
	}
	for other, fields := range CategoryOnly {
		if other == d.Category {
			continue
		}
		for _, f := range fields {
			if d.has(f) {
				return ReasonFieldNotForCategory
			}
		}
	}
	if _, ok := d.Price.Cents(); !ok {
		return ReasonBadPrice
	}
	if d.Currency == "" {
		d.Currency = DefaultCurrency
	}
	if !currencyRe.MatchString(d.Currency) {
		return ReasonBadCurrency
	}
	if !httpsURL(d.URL) {
		return ReasonBadURL
	}
	for _, t := range []struct {
		v   string
		max int
	}{
		{d.Title, MaxTitleLen}, {d.Note, MaxNoteLen}, {d.Seller, MaxNameLen},
		{d.Brand, MaxNameLen}, {d.MPN, MaxMPNLen},
	} {
		if !cleanText(t.v, t.max) {
			return ReasonBadValue
		}
	}
	if d.GTIN != "" && !gtinRe.MatchString(d.GTIN) {
		return ReasonBadValue
	}
	if d.Vintage != nil && (*d.Vintage < MinVintage || *d.Vintage > MaxVintage) {
		return ReasonBadValue
	}
	if d.BottleML != nil && (*d.BottleML < MinBottleML || *d.BottleML > MaxBottleML) {
		return ReasonBadValue
	}
	if d.CapacityTB != nil && (*d.CapacityTB <= 0 || *d.CapacityTB > MaxCapacityTB) {
		return ReasonBadValue
	}
	if d.Condition != "" && !contains(Conditions, d.Condition) {
		return ReasonBadValue
	}
	return ReasonNone
}

// httpsURL is an absolute https URL with a host and no userinfo.
func httpsURL(s string) bool {
	if len(s) > MaxURLLen || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}

// cleanText is at most max characters (runes) with no control characters.
func cleanText(s string, max int) bool {
	if utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
