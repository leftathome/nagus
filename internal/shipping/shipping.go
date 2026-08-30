// Package shipping is the wine ship-legality CONSTRAINT LAYER: a data-driven
// rules table answering "may this source legally ship wine to a consumer in
// destination state D?".
//
// US direct-to-consumer wine shipping law is per-destination-state and per
// CHANNEL: an out-of-state WINERY holding the destination's shipper permit
// may ship into most states; an IN-STATE licensed retailer may generally
// deliver within its own state; an OUT-OF-STATE RETAILER may ship into only
// a short list of states (Washington, notably, is not one -- SB 5007 died in
// committee Jan 2024). None of that is hardcoded into the pipeline: a source
// declares its channel and home state, Rules decides per destination, and
// the whole table is DATA -- DefaultRules ships a documented baseline and an
// operator can override any destination's policy from a JSON file
// (LoadRules / Override), because these laws change and because a household
// buying gifts for someone in CA or FL needs a different destination than
// the operator's own state.
//
// Everything FAILS CLOSED: an unknown destination, an unknown channel, or a
// missing policy is illegal, never a default-legal. Surfacing an
// un-shippable offer as actionable is the failure mode this layer exists to
// prevent.
//
// The default table is a good-faith engineering baseline, NOT legal advice:
// verify a destination's current law before relying on it, and override via
// the rules file when it drifts.
package shipping

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Channel is HOW a source ships: as a winery selling direct (under the
// destination's shipper permit) or as a licensed retailer. WHERE it ships
// from is the source's home state -- a separate declaration, so "in-state
// retailer" is derived per destination instead of baked into the channel
// name.
type Channel string

const (
	// ChannelWineryDirect: a winery shipping its own wine direct-to-consumer.
	// Legality per destination assumes the winery holds that destination's
	// permit where one is required; verifying the permit is part of
	// onboarding the source.
	ChannelWineryDirect Channel = "winery_direct"
	// ChannelRetailer: a licensed wine retailer. In-state vs out-of-state is
	// derived by comparing the source's home state to the destination.
	ChannelRetailer Channel = "retailer"
)

// Valid reports whether c is a declared channel value.
func (c Channel) Valid() bool {
	return c == ChannelWineryDirect || c == ChannelRetailer
}

// Source is one wine source's shipping declaration: its channel and its
// home state (USPS code).
type Source struct {
	Channel Channel
	State   string
}

// Validate returns an error naming the problem when the declaration is
// unusable (unknown channel, or a home state that is not a known
// state/district code).
func (s Source) Validate() error {
	if !s.Channel.Valid() {
		return fmt.Errorf("shipping: unknown channel %q (want %s|%s)", s.Channel, ChannelWineryDirect, ChannelRetailer)
	}
	if !IsState(s.State) {
		return fmt.Errorf("shipping: unknown source state %q (want a USPS state code, e.g. WA)", s.State)
	}
	return nil
}

// Policy is one destination state's rules, per channel/origin combination.
type Policy struct {
	// WineryDirect: an appropriately-permitted winery (in- or out-of-state)
	// may ship to consumers in this destination.
	WineryDirect bool `json:"wineryDirect"`
	// InStateRetailer: a retailer licensed IN this destination may
	// ship/deliver within it.
	InStateRetailer bool `json:"inStateRetailer"`
	// OutOfStateRetailer: a retailer outside this destination may ship into
	// it.
	OutOfStateRetailer bool `json:"outOfStateRetailer"`
}

// Rules is the full constraint table, keyed by destination state code.
type Rules struct {
	Destinations map[string]Policy `json:"destinations"`
}

// states is the canonical destination universe: the 50 states plus DC.
var states = []string{
	"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "FL", "GA",
	"HI", "ID", "IL", "IN", "IA", "KS", "KY", "LA", "ME", "MD",
	"MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ",
	"NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC",
	"SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY",
	"DC",
}

var stateSet = func() map[string]bool {
	m := make(map[string]bool, len(states))
	for _, s := range states {
		m[s] = true
	}
	return m
}()

// IsState reports whether code (after normalization) is a known state/DC
// USPS code.
func IsState(code string) bool {
	return stateSet[NormState(code)]
}

// NormState uppercases and trims a state code for table lookup.
func NormState(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// wineryDirectProhibited are the destinations whose law prohibits
// direct-to-consumer winery shipping entirely (baseline: Mississippi and
// Utah; every other state plus DC permits it with the destination's permit).
var wineryDirectProhibited = map[string]bool{"MS": true, "UT": true}

// outOfStateRetailerPermitted is the short baseline list of destinations
// that permit OUT-OF-STATE retailer direct shipping. Washington is absent
// deliberately (SB 5007 died in committee, Jan 2024). This list churns with
// legislation and litigation -- verify before relying on a destination, and
// override via the rules file when it changes.
var outOfStateRetailerPermitted = map[string]bool{
	"AK": true, "CA": true, "CT": true, "DC": true, "ID": true,
	"LA": true, "NE": true, "NV": true, "NH": true, "NM": true,
	"ND": true, "OR": true, "VA": true, "WV": true, "WY": true,
}

// DefaultRules returns the documented baseline table (see the package doc's
// not-legal-advice caveat): winery-direct legal everywhere except MS/UT,
// in-state retailer delivery legal everywhere, out-of-state retailer legal
// only into the short permitted list.
func DefaultRules() Rules {
	d := make(map[string]Policy, len(states))
	for _, s := range states {
		d[s] = Policy{
			WineryDirect:       !wineryDirectProhibited[s],
			InStateRetailer:    true,
			OutOfStateRetailer: outOfStateRetailerPermitted[s],
		}
	}
	return Rules{Destinations: d}
}

// LoadRules decodes a rules-override JSON document:
//
//	{"destinations": {"WA": {"wineryDirect": true, "inStateRetailer": true, "outOfStateRetailer": false}}}
//
// Destination keys are normalized; an unknown state code is an error (a
// typo like "WA " or "Wash" must not silently create an unreachable entry).
// The result is usually merged over DefaultRules via Override.
func LoadRules(r io.Reader) (Rules, error) {
	var raw Rules
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Rules{}, fmt.Errorf("shipping: decoding rules: %w", err)
	}
	out := Rules{Destinations: make(map[string]Policy, len(raw.Destinations))}
	for code, p := range raw.Destinations {
		norm := NormState(code)
		if !stateSet[norm] {
			return Rules{}, fmt.Errorf("shipping: rules name unknown destination %q (want a USPS state code)", code)
		}
		out.Destinations[norm] = p
	}
	return out, nil
}

// Override returns a copy of r with o's destination entries replacing r's
// (whole-Policy replacement per state, so an override is explicit about all
// three fields for the states it touches).
func (r Rules) Override(o Rules) Rules {
	out := Rules{Destinations: make(map[string]Policy, len(r.Destinations))}
	for k, v := range r.Destinations {
		out.Destinations[k] = v
	}
	for k, v := range o.Destinations {
		out.Destinations[NormState(k)] = v
	}
	return out
}

// Legal reports whether src may legally ship wine to a consumer in dest.
// FAIL CLOSED: unknown destination, unknown channel, or invalid source all
// return false.
func (r Rules) Legal(src Source, dest string) bool {
	p, ok := r.Destinations[NormState(dest)]
	if !ok {
		return false
	}
	switch src.Channel {
	case ChannelWineryDirect:
		return p.WineryDirect
	case ChannelRetailer:
		if !IsState(src.State) {
			return false
		}
		if NormState(src.State) == NormState(dest) {
			return p.InStateRetailer
		}
		return p.OutOfStateRetailer
	}
	return false
}

// LegalDestinations returns the sorted state codes src may ship to under r.
// This is what the ingest-side tagger stamps onto listings (as a token set),
// so the destination check downstream stays a cheap deterministic filter.
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
