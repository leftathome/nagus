// Package shipping is the wine ship-legality CONSTRAINT LAYER: a data-driven
// rules table answering "may this source legally ship wine to a consumer in
// destination D?", for destinations anywhere -- not just the US.
//
// # The model
//
// A JURISDICTION is an ISO 3166 code: a country ("FR", "AU") optionally with
// a subdivision ("US-WA", "CA-ON"). Both a source's origin and a watch's
// destination are jurisdictions, so the same table serves a household in
// Seattle, a gift for someone in Barcelona, and a case shipped to Ontario.
//
// Direct-to-consumer wine law turns on two things, and the table is indexed
// by exactly those:
//
//   - the CHANNEL: is the seller the producer shipping its own wine
//     (ChannelProducer -- "winery direct"), or a licensed reseller
//     (ChannelRetailer)? Most jurisdictions treat these very differently.
//   - the ORIGIN RELATION between seller and buyer: same subdivision (an
//     in-state/in-province seller), same country (interstate/interprovincial),
//     same trade bloc (the EU single market's excise distance-selling
//     regime), or foreign (a third country).
//
// So each destination carries a Policy: for each channel, which of those
// four relations may ship to it. That is enough to express the cases that
// actually differ in law -- a WA retailer may ship within Washington but a
// California retailer may not ship in (SB 5007 died in committee, Jan 2024);
// a French winery may distance-sell to a Spanish consumer but not to a US
// one, because US imports must clear a licensed importer.
//
// # Fail closed, everywhere
//
// An unknown destination, an unmodeled destination, an unknown channel, an
// unparseable jurisdiction, or a missing policy all mean ILLEGAL, never
// default-legal. A destination absent from the table is "we have not modeled
// this", which surfaces nothing rather than guessing -- that is why the
// default table OMITS jurisdictions whose regime we could not state (rather
// than encoding an all-false entry that would look modeled).
//
// # The default table is a baseline, NOT legal advice
//
// DefaultRules is a good-faith engineering baseline over ~110 destinations,
// deliberately CONSERVATIVE where we are unsure (see DefaultRules' own doc
// for what is modeled at what confidence). These laws change with
// legislation and litigation, so every destination is overridable from a
// JSON file (LoadRules + Override) and no code change is needed to correct
// one. Verify a destination before relying on it.
package shipping

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Channel is the seller's role. It is the first index into a destination's
// Policy, because DTC law almost everywhere treats a producer selling its own
// wine differently from a reseller.
type Channel string

const (
	// ChannelProducer is the winery/estate shipping wine it made. In the US
	// this is the "winery direct" permit path; in the EU it is a producer
	// distance-selling into another member state.
	ChannelProducer Channel = "producer"
	// ChannelRetailer is a licensed reseller: a wine shop, an online
	// retailer, a flash-sale site, or a monopoly outlet.
	ChannelRetailer Channel = "retailer"
)

// Valid reports whether c is a declared channel value.
func (c Channel) Valid() bool {
	return c == ChannelProducer || c == ChannelRetailer
}

// Jurisdiction is a place: an ISO 3166-1 alpha-2 country, optionally with an
// ISO 3166-2 subdivision (the part after the hyphen). "US-WA" is Washington
// state; "FR" is France as a whole. A country-level jurisdiction is the right
// granularity wherever the law does not vary below the national level.
type Jurisdiction struct {
	Country     string // ISO 3166-1 alpha-2, e.g. "US", "CA", "FR"
	Subdivision string // ISO 3166-2 subdivision part, e.g. "WA", "ON"; "" = country-level
}

// Code renders the canonical "US-WA" / "FR" form.
func (j Jurisdiction) Code() string {
	if j.Subdivision == "" {
		return j.Country
	}
	return j.Country + "-" + j.Subdivision
}

// Zero reports whether j names no place at all.
func (j Jurisdiction) Zero() bool { return j.Country == "" }

var (
	countryRe     = regexp.MustCompile(`^[A-Z]{2}$`)
	subdivisionRe = regexp.MustCompile(`^[A-Z0-9]{1,3}$`)
)

// ParseJurisdiction parses "us-wa" / "US-WA" / "fr" into a canonical
// Jurisdiction. It validates SHAPE (ISO 3166 form), not existence: which
// jurisdictions are actually known is the rules table's business, so adding a
// country is pure data and never a code change.
func ParseJurisdiction(s string) (Jurisdiction, error) {
	up := strings.ToUpper(strings.TrimSpace(s))
	if up == "" {
		return Jurisdiction{}, fmt.Errorf("shipping: empty jurisdiction code")
	}
	parts := strings.Split(up, "-")
	if len(parts) > 2 {
		return Jurisdiction{}, fmt.Errorf("shipping: jurisdiction %q has too many parts (want COUNTRY or COUNTRY-SUBDIVISION)", s)
	}
	if !countryRe.MatchString(parts[0]) {
		return Jurisdiction{}, fmt.Errorf("shipping: jurisdiction %q: %q is not an ISO 3166-1 alpha-2 country code", s, parts[0])
	}
	j := Jurisdiction{Country: parts[0]}
	if len(parts) == 2 {
		if !subdivisionRe.MatchString(parts[1]) {
			return Jurisdiction{}, fmt.Errorf("shipping: jurisdiction %q: %q is not an ISO 3166-2 subdivision code", s, parts[1])
		}
		j.Subdivision = parts[1]
	}
	return j, nil
}

// NormJurisdiction canonicalizes a jurisdiction code, returning ok=false when
// it is not well-formed. Callers that merely need to validate an untrusted
// token (e.g. the wine extractor) use this.
func NormJurisdiction(s string) (string, bool) {
	j, err := ParseJurisdiction(s)
	if err != nil {
		return "", false
	}
	return j.Code(), true
}

// Source is one wine source's shipping declaration: its channel and the
// jurisdiction it ships FROM.
type Source struct {
	Channel Channel
	Origin  Jurisdiction
}

// NewSource builds a Source from a channel and an origin jurisdiction code,
// validating both. Config paths use this so a typo fails at startup.
func NewSource(channel string, origin string) (Source, error) {
	ch := Channel(strings.ToLower(strings.TrimSpace(channel)))
	if !ch.Valid() {
		return Source{}, fmt.Errorf("shipping: unknown channel %q (want %s|%s)", channel, ChannelProducer, ChannelRetailer)
	}
	j, err := ParseJurisdiction(origin)
	if err != nil {
		return Source{}, err
	}
	return Source{Channel: ch, Origin: j}, nil
}

// Validate returns an error naming the problem when the declaration is
// unusable.
func (s Source) Validate() error {
	if !s.Channel.Valid() {
		return fmt.Errorf("shipping: unknown channel %q (want %s|%s)", s.Channel, ChannelProducer, ChannelRetailer)
	}
	if s.Origin.Zero() {
		return fmt.Errorf("shipping: source has no origin jurisdiction (want e.g. US-WA or FR)")
	}
	if _, err := ParseJurisdiction(s.Origin.Code()); err != nil {
		return err
	}
	return nil
}

// Relation is how a source's origin stands to a destination. It is the second
// index into a Policy.
type Relation string

const (
	// RelSameSubdivision: seller and buyer are in the same subdivision (an
	// in-state or in-province seller). Requires BOTH sides to name a
	// subdivision -- a source declared only as "US" cannot be PROVEN to be in
	// Washington, so it is treated as the weaker RelSameCountry.
	RelSameSubdivision Relation = "same_subdivision"
	// RelSameCountry: same country, different (or unstated) subdivision --
	// interstate/interprovincial shipping.
	RelSameCountry Relation = "same_country"
	// RelSameBloc: different countries inside one trade bloc that has its own
	// distance-selling regime (the EU single market).
	RelSameBloc Relation = "same_bloc"
	// RelForeign: a third country. DTC into most markets requires clearing a
	// licensed importer, which a consumer-facing offer does not do.
	RelForeign Relation = "foreign"
)

// ChannelPolicy says, for one channel, which origin relations may ship to a
// destination. Zero value = nothing may ship (fail closed).
type ChannelPolicy struct {
	SameSubdivision bool `json:"sameSubdivision"`
	SameCountry     bool `json:"sameCountry"`
	SameBloc        bool `json:"sameBloc"`
	Foreign         bool `json:"foreign"`
}

// Allows reports whether this channel policy permits the given relation.
func (p ChannelPolicy) Allows(r Relation) bool {
	switch r {
	case RelSameSubdivision:
		return p.SameSubdivision
	case RelSameCountry:
		return p.SameCountry
	case RelSameBloc:
		return p.SameBloc
	case RelForeign:
		return p.Foreign
	}
	return false
}

// Policy is one destination's rules for both channels.
type Policy struct {
	Producer ChannelPolicy `json:"producer"`
	Retailer ChannelPolicy `json:"retailer"`
}

// forChannel returns the channel's policy, and ok=false for an unknown
// channel (fail closed).
func (p Policy) forChannel(c Channel) (ChannelPolicy, bool) {
	switch c {
	case ChannelProducer:
		return p.Producer, true
	case ChannelRetailer:
		return p.Retailer, true
	}
	return ChannelPolicy{}, false
}

// Rules is the constraint table: destinations plus the trade blocs whose
// internal cross-border selling is its own regime.
type Rules struct {
	// Destinations is keyed by jurisdiction code ("US-WA", "FR"). Lookup is
	// hierarchical: an exact subdivision entry wins, else the country entry,
	// else the destination is unmodeled and nothing may ship there.
	Destinations map[string]Policy `json:"destinations"`
	// Blocs maps a bloc name to its member COUNTRY codes, e.g.
	// {"EU": ["FR","ES",...]}. Two different countries sharing a bloc are
	// RelSameBloc rather than RelForeign.
	Blocs map[string][]string `json:"blocs,omitempty"`
}

// Relate derives the origin relation between a source's origin and a
// destination under this table's blocs.
func (r Rules) Relate(origin, dest Jurisdiction) Relation {
	if origin.Country == dest.Country {
		if origin.Subdivision != "" && origin.Subdivision == dest.Subdivision {
			return RelSameSubdivision
		}
		return RelSameCountry
	}
	for _, members := range r.Blocs {
		var hasOrigin, hasDest bool
		for _, m := range members {
			switch strings.ToUpper(m) {
			case origin.Country:
				hasOrigin = true
			case dest.Country:
				hasDest = true
			}
		}
		if hasOrigin && hasDest {
			return RelSameBloc
		}
	}
	return RelForeign
}

// policyFor resolves a destination's policy hierarchically: the exact
// jurisdiction, else its country. ok=false means the destination is not
// modeled at all.
func (r Rules) policyFor(dest Jurisdiction) (Policy, bool) {
	if p, ok := r.Destinations[dest.Code()]; ok {
		return p, true
	}
	if dest.Subdivision != "" {
		if p, ok := r.Destinations[dest.Country]; ok {
			return p, true
		}
	}
	return Policy{}, false
}

// Legal reports whether src may legally ship wine to a consumer in dest.
// FAIL CLOSED: an unparseable or unmodeled destination, an invalid source, or
// an unknown channel all return false.
func (r Rules) Legal(src Source, dest string) bool {
	d, err := ParseJurisdiction(dest)
	if err != nil {
		return false
	}
	if err := src.Validate(); err != nil {
		return false
	}
	p, ok := r.policyFor(d)
	if !ok {
		return false
	}
	cp, ok := p.forChannel(src.Channel)
	if !ok {
		return false
	}
	return cp.Allows(r.Relate(src.Origin, d))
}

// LegalDestinations returns the sorted destination codes src may ship to
// under r. This is what the ingest-side tagger stamps onto listings, so the
// per-watch destination check downstream stays a cheap deterministic filter
// over one corpus.
func (r Rules) LegalDestinations(src Source) []string {
	var out []string
	for code := range r.Destinations {
		if r.Legal(src, code) {
			out = append(out, code)
		}
	}
	sort.Strings(out)
	return out
}

// Modeled reports whether dest has a policy (directly or via its country).
// Config paths use this to reject a destination that would silently surface
// nothing.
func (r Rules) Modeled(dest string) bool {
	d, err := ParseJurisdiction(dest)
	if err != nil {
		return false
	}
	_, ok := r.policyFor(d)
	return ok
}

// LoadRules decodes a rules-override JSON document:
//
//	{
//	  "destinations": {"US-WA": {"producer": {"sameSubdivision": true, "sameCountry": true},
//	                             "retailer": {"sameSubdivision": true}}},
//	  "blocs": {"EU": ["FR", "ES", "IT"]}
//	}
//
// Destination keys and bloc members are canonicalized; a malformed code is an
// error, because a typo would otherwise create an entry nothing can ever
// match. Unknown fields are rejected so a misspelled policy key cannot
// silently read as false. The result is usually merged over DefaultRules with
// Override.
func LoadRules(r io.Reader) (Rules, error) {
	var raw Rules
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Rules{}, fmt.Errorf("shipping: decoding rules: %w", err)
	}
	out := Rules{Destinations: make(map[string]Policy, len(raw.Destinations))}
	for code, p := range raw.Destinations {
		j, err := ParseJurisdiction(code)
		if err != nil {
			return Rules{}, fmt.Errorf("shipping: rules destination %q: %w", code, err)
		}
		out.Destinations[j.Code()] = p
	}
	if len(raw.Blocs) > 0 {
		out.Blocs = make(map[string][]string, len(raw.Blocs))
		for name, members := range raw.Blocs {
			canon := make([]string, 0, len(members))
			for _, m := range members {
				j, err := ParseJurisdiction(m)
				if err != nil {
					return Rules{}, fmt.Errorf("shipping: bloc %q member %q: %w", name, m, err)
				}
				if j.Subdivision != "" {
					return Rules{}, fmt.Errorf("shipping: bloc %q member %q must be a country, not a subdivision", name, m)
				}
				canon = append(canon, j.Country)
			}
			sort.Strings(canon)
			out.Blocs[name] = canon
		}
	}
	return out, nil
}

// Override returns a COPY of r with o's entries replacing r's: whole-Policy
// replacement per destination, whole-membership replacement per bloc. Neither
// input is mutated.
func (r Rules) Override(o Rules) Rules {
	out := Rules{
		Destinations: make(map[string]Policy, len(r.Destinations)),
		Blocs:        make(map[string][]string, len(r.Blocs)),
	}
	for k, v := range r.Destinations {
		out.Destinations[k] = v
	}
	for k, v := range r.Blocs {
		out.Blocs[k] = append([]string(nil), v...)
	}
	for k, v := range o.Destinations {
		if j, err := ParseJurisdiction(k); err == nil {
			out.Destinations[j.Code()] = v
		}
	}
	for k, v := range o.Blocs {
		out.Blocs[k] = append([]string(nil), v...)
	}
	return out
}
