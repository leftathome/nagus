package deal

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

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

// urlText is a url's path, query and fragment, percent-decoded where
// possible, for the gate to read as text.
func urlText(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	parts := []string{u.Host}
	if p, err := url.PathUnescape(u.EscapedPath()); err == nil {
		parts = append(parts, p)
	}
	if q, err := url.QueryUnescape(u.RawQuery); err == nil && q != "" {
		parts = append(parts, q)
	}
	if u.Fragment != "" {
		parts = append(parts, u.Fragment)
	}
	return strings.Join(parts, " ")
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
	set(AspectURLText, urlText(d.URL))
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
