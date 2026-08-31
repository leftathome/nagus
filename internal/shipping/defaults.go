package shipping

// This file is DATA: the baseline constraint table. Nothing here is legal
// advice (see the package doc), and every entry is overridable from a JSON
// file without touching code -- that is the whole point of keeping it here
// rather than in the pipeline.
//
// Two rules govern what goes in it:
//
//  1. OMIT what we cannot state. A destination absent from the table is
//     "unmodeled", and the layer surfaces nothing for it -- an honest signal
//     an operator can fix with an override. Encoding an all-false entry
//     instead would look modeled while behaving identically, so we don't.
//     This is why Norway, Iceland, and several Asian markets are absent
//     despite being real wine markets: their monopoly/import regimes did not
//     reduce to a confident policy here.
//  2. Prefer the conservative reading where the law is unsettled. The
//     RelForeign dimension is false almost everywhere in this table, not
//     because third-country shipping is universally banned, but because it
//     usually requires an importer the consumer-facing offer does not
//     involve. Destinations with genuine personal-import allowances (AU, NZ,
//     GB, JP, ...) are noted below and are a one-line override away.
//
// Confidence by region:
//
//   - US (51 destinations, per state): HIGH on structure, MEDIUM on the
//     lists. Winery-direct is broadly permitted; retailer shipping is the
//     narrow case. Verify a state before enabling it.
//   - Canada (13, per province/territory): MEDIUM. The provincial monopolies
//     are the dominant fact; interprovincial winery-direct is permitted by a
//     minority of provinces.
//   - EU-27 (country level) + the single market as a bloc: MEDIUM-HIGH on
//     the regime (excise distance selling exists and is used daily),
//     MEDIUM on the Nordic monopoly exceptions.
//   - Everything else (country level): LOW-MEDIUM. Domestic DTC is modeled
//     as normal; cross-border is not modeled.

// domestic is the common shape: a seller inside the destination's own
// country may ship to it, from its own subdivision or another.
func domestic() ChannelPolicy {
	return ChannelPolicy{SameSubdivision: true, SameCountry: true}
}

// domesticPlusBloc adds the trade bloc's cross-border distance-selling
// regime (the EU single market) to the domestic case.
func domesticPlusBloc() ChannelPolicy {
	return ChannelPolicy{SameSubdivision: true, SameCountry: true, SameBloc: true}
}

// --- United States ---------------------------------------------------------

// usStates is the 50 states plus DC.
var usStates = []string{
	"AL", "AK", "AZ", "AR", "CA", "CO", "CT", "DE", "FL", "GA",
	"HI", "ID", "IL", "IN", "IA", "KS", "KY", "LA", "ME", "MD",
	"MA", "MI", "MN", "MS", "MO", "MT", "NE", "NV", "NH", "NJ",
	"NM", "NY", "NC", "ND", "OH", "OK", "OR", "PA", "RI", "SC",
	"SD", "TN", "TX", "UT", "VT", "VA", "WA", "WV", "WI", "WY",
	"DC",
}

// usNoDTC are states where no direct-to-consumer wine shipment is modeled as
// available at all: Delaware, Mississippi and Utah prohibit DTC wine
// shipping, and Rhode Island permits only on-site purchases to be shipped --
// which is not an offer a watch could act on remotely. Modeled as fully
// closed rather than omitted, because "this state prohibits it" is a fact we
// are stating, unlike the omitted destinations above.
var usNoDTC = map[string]bool{"DE": true, "MS": true, "UT": true, "RI": true}

// usRetailerInbound are the states modeled as permitting an OUT-OF-STATE
// retailer to ship wine to a consumer. Washington is deliberately absent:
// SB 5007, which would have created an out-of-state wine retailer shipper's
// permit, died in the Senate Labor & Commerce Committee in January 2024.
// This list churns with legislation and litigation -- verify before relying.
var usRetailerInbound = map[string]bool{
	"AK": true, "CA": true, "CT": true, "DC": true, "ID": true,
	"LA": true, "NE": true, "NV": true, "NH": true, "NM": true,
	"ND": true, "OR": true, "VA": true, "WV": true, "WY": true,
}

// --- Canada ----------------------------------------------------------------

// caProvinces is the 10 provinces plus 3 territories.
var caProvinces = []string{
	"AB", "BC", "MB", "NB", "NL", "NS", "NT", "NU", "ON", "PE", "QC", "SK", "YT",
}

// caInterprovincialProducer are the provinces modeled as permitting a winery
// in ANOTHER province to ship direct to their consumers. Federal law (2012)
// removed the national barrier, but each province decides; most keep the
// liquor-board monopoly on inbound wine.
var caInterprovincialProducer = map[string]bool{
	"BC": true, "MB": true, "NS": true, "SK": true,
}

// --- Europe ----------------------------------------------------------------

// euMembers are the EU-27, whose internal cross-border distance selling of
// alcohol to consumers has its own excise regime (the seller registers and
// pays excise in the destination). This is the bloc that makes RelSameBloc a
// real distinction rather than a synonym for RelForeign.
var euMembers = []string{
	"AT", "BE", "BG", "HR", "CY", "CZ", "DK", "EE", "FI", "FR",
	"DE", "GR", "HU", "IE", "IT", "LV", "LT", "LU", "MT", "NL",
	"PL", "PT", "RO", "SK", "SI", "ES", "SE",
}

// euRetailMonopoly are EU members with a state alcohol retail monopoly, where
// a domestic private retailer or producer selling direct to a consumer is
// restricted, but private importation from another member state is permitted
// (the CJEU distance-selling line of cases). Modeled as: domestic closed,
// intra-bloc open.
var euRetailMonopoly = map[string]bool{"SE": true, "FI": true}

// otherMarkets are non-EU destinations modeled at country level with normal
// DOMESTIC direct-to-consumer delivery and no modeled cross-border DTC.
// Several of these (AU, NZ, GB, JP) do allow personal importation subject to
// duty; that is an override away (set the channel's "foreign" to true) and is
// left off by default per rule 2 above.
var otherMarkets = []string{
	"GB", "CH", // Europe outside the EU
	"AU", "NZ", // Oceania
	"AR", "BR", "CL", "MX", "UY", // Americas outside the US/Canada
	"ZA", // Africa
	"JP", // Asia
}

// DefaultRules returns the baseline constraint table described in this file's
// comments: ~110 destinations across the US (per state), Canada (per
// province), the EU-27 (with the single market as a bloc), and other major
// wine markets at country level.
//
// It is a starting point to verify and override, not legal advice. Callers
// merge an operator's corrections over it with Override.
func DefaultRules() Rules {
	dests := make(map[string]Policy, len(usStates)+len(caProvinces)+len(euMembers)+len(otherMarkets))

	// United States: winery-direct is the broad path; retailer shipping is
	// in-state plus the narrow inbound list. Foreign DTC is closed
	// everywhere -- US imports must clear a licensed importer, which a
	// consumer-facing offer does not do.
	for _, st := range usStates {
		p := Policy{}
		if !usNoDTC[st] {
			p.Producer = ChannelPolicy{SameSubdivision: true, SameCountry: true}
			p.Retailer = ChannelPolicy{SameSubdivision: true, SameCountry: usRetailerInbound[st]}
		}
		dests["US-"+st] = p
	}

	// Canada: a winery may ship within its own province; only some provinces
	// admit wine direct from a winery in another province. Retail is the
	// provincial monopoly's business, so it stays in-province.
	for _, pr := range caProvinces {
		dests["CA-"+pr] = Policy{
			Producer: ChannelPolicy{SameSubdivision: true, SameCountry: caInterprovincialProducer[pr]},
			Retailer: ChannelPolicy{SameSubdivision: true},
		}
	}

	// EU-27: domestic sale plus intra-bloc distance selling. The Nordic
	// monopoly members close the domestic side but keep the intra-bloc
	// private-import path open.
	for _, c := range euMembers {
		if euRetailMonopoly[c] {
			blocOnly := ChannelPolicy{SameBloc: true}
			dests[c] = Policy{Producer: blocOnly, Retailer: blocOnly}
			continue
		}
		dests[c] = Policy{Producer: domesticPlusBloc(), Retailer: domesticPlusBloc()}
	}

	// Everywhere else modeled: domestic only.
	for _, c := range otherMarkets {
		dests[c] = Policy{Producer: domestic(), Retailer: domestic()}
	}

	return Rules{
		Destinations: dests,
		Blocs:        map[string][]string{"EU": append([]string(nil), euMembers...)},
	}
}
