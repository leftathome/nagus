// Package textclean holds small, dependency-free text predicates shared by
// the packages that must agree on them.
package textclean

import (
	"strings"
	"unicode"
)

// Invisible is Cf, Other_Default_Ignorable_Code_Point or a variation
// selector: together, a superset of Unicode's Default_Ignorable_Code_Point
// set and every format character. quark's hint gate (quark-d0r) refuses
// every one of them in every hint field.
func Invisible(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) ||
		unicode.Is(unicode.Variation_Selector, r)
}

// StripInvisible removes every Invisible rune and trims the ends. A string
// with none is returned unchanged (no allocation).
func StripInvisible(s string) string {
	if strings.IndexFunc(s, Invisible) < 0 {
		return s
	}
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if Invisible(r) {
			return -1
		}
		return r
	}, s))
}
