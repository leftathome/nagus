package wine

import (
	"errors"
	"testing"
)

// nagus !28 final review nits 2-4 and Port Ellen: real port styles the cask,
// spirit and hyphen rules withdrew, and a Scotch they let through.
func TestExtract_PortStylesAndGuards(t *testing.T) {
	for _, title := range []string{
		// Oak or wood BEFORE the word is a style, not a cask.
		"Oak Aged Port", "Wood Port",
		// A named port style is exempt from oak/wood after it too.
		"Old Oak Tawny Port", "Tawny Port, oak aged", "Ruby Port Wood Aged", "Vintage Port Oak",
		// A spirit word in the producer name right before the wine word.
		"Porter Creek Tawny Port", "Rum Runner Port", "Gin Lane Port", "Ale House Sherry",
		"Porter Creek Vineyards Tawny Port",
		"Malt House Oloroso", "Oloroso Malt Street Cellars", // "malt" alone is not a spirit word
		// Hyphenated styles.
		"Tawny-Port", "Ruby-Port",
		// Angelica, California's fortified dessert wine.
		"2020 Angelica", "Angelica 375ml",
	} {
		if err := extractErr(title); err != nil {
			t.Errorf("%q: %v, want wine", title, err)
		}
	}
	for _, title := range []string{
		"Port Cask Finish Scotch",
		// The cask word two words away (pins the two-word window: a
		// one-word window lets this through).
		"Macallan Sherry Double Cask",
		"Cask Strength Sherry",
		// Oak and wood AFTER a bare fortified word are a cask.
		"Sherry Oak 12", "Port Wood Reserve",
		// Cask/barrel/finish still withdraw a named style.
		"Tawny Port Wood Finish", "Tawny Port Cask",
		// A spirit that is the drink, not a producer name.
		"Bourbon Barrel Aged Port", "Bourbon Barrel Port", "Rum Port", "Scotch Whisky Port", "Tawny Port Bourbon",
		"Single Malt Fino", "Bourbon Barrel Select Estate Port",
		"Porter Creek 12 Port", // a number is not part of a name
		// Spirit brands named after a place called Port.
		"Port Ellen 40 Year Old", "Port Askaig 8",
		// A hyphen that is not a port style.
		"Port-a-Potty", "Angelica-Root Tea",
	} {
		err := extractErr(title)
		if !errors.Is(err, ErrNotWine) || errors.Is(err, ErrCulinary) || errors.Is(err, ErrMerchandise) {
			t.Errorf("%q: %v, want a bare ErrNotWine", title, err)
		}
	}
}
