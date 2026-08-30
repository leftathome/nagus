package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// SourceConfig is one ingest source. type selects the connector builder;
// category binds it to a category's extractor (slice 1); secretRef names a
// per-source secret the chart injects as env. Connector-specific fields are
// grouped by type.
type SourceConfig struct {
	Name            string `json:"name"`
	Category        string `json:"category"`
	Type            string `json:"type"` // "ebay" | "zillapi" | "shopify"
	IntervalMinutes int    `json:"intervalMinutes"`
	// SecretRef is parsed but not yet consumed: secrets currently flow via
	// global env/ExternalSecret. Reserved for per-source secret wiring in a
	// later slice.
	SecretRef string `json:"secretRef,omitempty"`

	// ebay
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`

	// zillapi (land). The acreage/price WINDOW is not repeated here: it is taken
	// from the source's CategoryConfig so the filter pushed upstream cannot drift
	// from the local hard-filter. Only the geography and the cost caps are
	// per-source.
	//
	// BBox is REQUIRED for a live zillapi source: upstream anchors a search on a
	// bounding box and rejects a free-text location alone with 400
	// invalid_filters. Get one from a zillow.com map URL's mapBounds.
	BBox *BBoxConfig `json:"bbox,omitempty"`
	// MaxItems caps results per poll. Every returned row costs one credit, so this
	// is a spend cap, not a page size. Clamped to the connector's sync ceiling.
	MaxItems int `json:"maxItems,omitempty"`
	// DaysOnZillow restricts a poll to recently-listed lots (1|7|14|30|90|6m|...),
	// which is what keeps a daily poll from re-billing the standing inventory.
	DaysOnZillow string `json:"daysOnZillow,omitempty"`

	// shopify (per-store). One generic connector, N store configs.
	//
	// BaseURL is the storefront root, e.g. "https://serverpartdeals.com".
	// ProductTypePrefixes is the allow-filter that stops a mixed catalog (HDDs AND
	// SSDs) pulling the whole store downstream -- pass EVERY spelling a store uses
	// for one category, since real catalogs are inconsistent ("Hard Drives" and
	// "HDDs" both occur at serverpartdeals).
	BaseURL             string   `json:"baseUrl,omitempty"`
	ProductTypePrefixes []string `json:"productTypePrefixes,omitempty"`
	IncludeUnavailable  bool     `json:"includeUnavailable,omitempty"`
	// Product-identity extraction, DECLARED per store because guessing mints
	// false identities: one store's `vendor` is the real manufacturer, another's
	// is its own house label on generic hardware. See shopify.Config.
	BrandTag    string   `json:"brandTag,omitempty"`
	SKUIsMPN    bool     `json:"skuIsMpn,omitempty"`
	SKUSuffixes []string `json:"skuSuffixes,omitempty"`
	MaxPages    int      `json:"maxPages,omitempty"`

	// wine (per-source). WineChannel declares the source's shipping channel
	// for Washington legality: "wa_retailer" | "winery_direct" |
	// "out_of_state_retailer". REQUIRED on a wine source -- legality is a
	// conscious per-source declaration, never a default (see
	// category.WineChannel).
	WineChannel string `json:"wineChannel,omitempty"`

	// offline/testing
	Fixture string `json:"fixture,omitempty"`
}

// BBoxConfig is a bounding box in decimal degrees, as read off a zillow.com map
// URL's searchQueryState mapBounds.
type BBoxConfig struct {
	West  float64 `json:"west"`
	South float64 `json:"south"`
	East  float64 `json:"east"`
	North float64 `json:"north"`
}

// CategoryConfig is the surface/scoring config for one category.
type CategoryConfig struct {
	MinCapacityTB   float64 `json:"minCapacityTB,omitempty"`
	MinAcreageAcres float64 `json:"minAcreageAcres,omitempty"`
	MaxAcreageAcres float64 `json:"maxAcreageAcres,omitempty"`
	BudgetCents     int64   `json:"budgetCents,omitempty"`

	// wine. MinWineScore hard-filters on the aggregated normalized critic
	// score; MinWineScoreCount is the valuation's minimum independent-critic
	// count before flagging value (0 = the default minimum-3 rule);
	// RequireShipLegalWA drops offers whose source channel cannot legally
	// ship wine to a WA consumer.
	MinWineScore       float64 `json:"minWineScore,omitempty"`
	MinWineScoreCount  int     `json:"minWineScoreCount,omitempty"`
	RequireShipLegalWA bool    `json:"requireShipLegalWA,omitempty"`
}

// RunConfig is the whole deployment declaration: what to ingest and what to
// surface. DefaultCategory (optional) pins the read default when more than one
// category is configured.
type RunConfig struct {
	Sources         []SourceConfig            `json:"sources"`
	Categories      map[string]CategoryConfig `json:"categories"`
	DefaultCategory string                    `json:"defaultCategory,omitempty"`
}

// LoadRunConfig reads and validates config.json.
func LoadRunConfig(path string) (RunConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RunConfig{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var c RunConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return RunConfig{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(c.Categories) == 0 {
		return RunConfig{}, fmt.Errorf("config %q: at least one category is required", path)
	}
	seen := map[string]bool{}
	for i, s := range c.Sources {
		if s.Name == "" {
			return RunConfig{}, fmt.Errorf("source #%d: name is required", i)
		}
		if seen[s.Name] {
			return RunConfig{}, fmt.Errorf("source %q: duplicate name", s.Name)
		}
		seen[s.Name] = true
		// An EMPTY category means an OFFER-ONLY source (gate-at-eval,
		// nagus-7yq): it feeds the offer store and nothing evaluates it. This is
		// how a source can be collected speculatively -- accumulating history for
		// goods we may want to evaluate later -- without inventing a category
		// bundle for it first, and without paying any extraction cost meanwhile.
		//
		// Offer-only sources require the offer layer to be ON; otherwise the
		// source would fetch and silently discard everything, which is worse than
		// refusing to start.
		if s.Category == "" {
			continue
		}
		if !supportedCategory(s.Category) {
			return RunConfig{}, fmt.Errorf("source %q: unsupported category %q", s.Name, s.Category)
		}
		if _, ok := c.Categories[s.Category]; !ok {
			return RunConfig{}, fmt.Errorf("source %q: references category %q not in categories{}", s.Name, s.Category)
		}
	}
	if c.DefaultCategory != "" {
		if _, ok := c.Categories[c.DefaultCategory]; !ok {
			return RunConfig{}, fmt.Errorf("defaultCategory %q not in categories{}", c.DefaultCategory)
		}
	}
	return c, nil
}
