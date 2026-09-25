package offer

import "strings"

// The vintage statuses ComparisonKey reports, for display and for consumers
// that group rows.
const (
	// VintageStatusVintage: a wine whose year is part of what it is, with the
	// year known. Compared only against the same year.
	VintageStatusVintage = "vintage"
	// VintageStatusNonVintage: a blend made to a house style (NV Champagne,
	// Bollinger Special Cuvee). Every listing of the product is the same thing
	// to compare, whatever year a title mentions -- in these titles a year is
	// a disgorgement date, a "bottled 2018", an anniversary edition.
	VintageStatusNonVintage = "non_vintage"
	// VintageStatusUnknown: a vintage-sensitive (or not known to be
	// vintage-insensitive) wine whose listing states no year. It must not be
	// priced against any specific vintage, so it has no comparison key.
	VintageStatusUnknown = "vintage_unknown"
	// VintageStatusConflictNV: quark says the wine's year matters, but the
	// listing says NV. One of the two is wrong -- the listing, or the match --
	// and any year in the title is not trustworthy as the vintage, so it has
	// no comparison key.
	VintageStatusConflictNV = "conflict_nv"
)

// ComparisonKey is the key two offers must share to be compared with each
// other on price -- "the same thing at different sellers".
//
// A quark product id alone is not that key for wine. quark's wine product is
// the LWIN-7 wine (QUARK-04), which covers every vintage: the 2014 and the
// 2015 Bollinger La Grande Annee share one product id and are not the same
// thing to buy. quark says, per product, whether the year matters
// (vintageMode, from the LWIN export's VINTAGE_CONFIG), and the key follows
// it:
//
//	vintage_mode   listing year   key                  status
//	non_vintage    ignored        productID            non_vintage
//	vintage        2019           productID@2019       vintage
//	vintage        none           "" (not compared)    vintage_unknown
//	vintage        title says NV  "" (not compared)    conflict_nv
//	unknown        as vintage, except that the listing's own explicit "NV"
//	               marks it non-vintage
//	""             (quark stated no mode: every non-wine product)
//	               productID, status ""
//
// vintage is the listing's extracted year (the wine item's "vintage"
// attribute) and explicitNV whether its title carried an NV marker (the "nv"
// attribute; the extractor reads the title, not this read path). An empty
// productID has no key.
func ComparisonKey(productID, vintageMode, vintage string, explicitNV bool) (key, status string) {
	if productID == "" {
		return "", ""
	}
	vintage = strings.TrimSpace(vintage)
	switch vintageMode {
	case "":
		return productID, ""
	case "non_vintage":
		return productID, VintageStatusNonVintage
	case "unknown":
		if explicitNV {
			return productID, VintageStatusNonVintage
		}
	case "vintage":
		if explicitNV {
			return "", VintageStatusConflictNV
		}
	}
	if vintage == "" {
		return "", VintageStatusUnknown
	}
	return productID + "@" + vintage, VintageStatusVintage
}
