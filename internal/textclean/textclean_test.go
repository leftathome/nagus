package textclean

import "testing"

func TestStripInvisible(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Seagate 8TB", "Seagate 8TB"},
		{"Seagate \u2764\ufe0f 8TB", "Seagate \u2764 8TB"},                                   // VS16 emoji
		{"\U0001F468\u200d\U0001F469 family", "\U0001F468\U0001F469 family"},                 // ZWJ sequence
		{"ultra\u00adstar", "ultrastar"},                                                     // soft hyphen
		{"\ufeffWD Red", "WD Red"},                                                           // BOM
		{"\u202eevil\u202c \u2066x\u2069", "evil x"},                                         // bidi override + isolates
		{"tag\U000E0041\U000E0042", "tag"},                                                   // Tags block
		{"\u180e\u115f", ""},                                                                 // Mongolian vowel separator, Hangul filler
		{"Ch\u00e2teau Margaux 2015 \u2013 750ml", "Ch\u00e2teau Margaux 2015 \u2013 750ml"}, // visible non-ASCII kept
	} {
		if got := StripInvisible(tc.in); got != tc.want {
			t.Errorf("StripInvisible(%+q) = %+q, want %+q", tc.in, got, tc.want)
		}
	}
}
