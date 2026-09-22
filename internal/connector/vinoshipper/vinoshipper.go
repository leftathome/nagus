// Package vinoshipper reads a producer's public Vinoshipper wine list
// (nagus-tub).
//
// Vinoshipper runs compliance and checkout for thousands of small producers.
// Each producer's catalogue is served, unauthenticated, as JSON at
//
//	https://vinoshipper.com/json-api/v2/wine-list?id=<accountId>
//
// -- the same feed Vinoshipper's embeddable storefront reads. The endpoint is
// undocumented, so this connector is best-effort: a changed shape is a loud
// decode error, never a silently empty catalogue. The account id is on the
// producer's shop page (Vinoshipper.init(<id>, ...)); `nagus fingerprint`
// finds it.
//
// The feed is unusually good for nagus: price AND msrp (a store sale when
// price < msrp), varietal, appellation, bottle size, inventory, and -- per
// producer -- the states it ships to, minus any state a given wine is barred
// from. That last part is legality evidence straight from the seller, carried
// as the ships_to aspect for the wine layer to intersect with its own rules.
package vinoshipper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/nagus/internal/connector/webfetch"
	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the connector family; a configured source is "vinoshipper:<Name>".
const SourceID = "vinoshipper"

// DefaultBaseURL is where the public feed lives.
const DefaultBaseURL = "https://vinoshipper.com"

// wineTypes are the feed's product types that are wine. Anything else (CIDER,
// SPIRITS, MEAD, merchandise, ...) is skipped and logged once per fetch; an
// unknown type is treated as not wine rather than guessed.
var wineTypes = map[string]bool{
	"RED": true, "WHITE": true, "ROSE": true, "ROS\u00c9": true, "SPARKLING": true,
	"DESSERT": true, "FORTIFIED": true, "ORANGE": true, "PORT": true, "SHERRY": true,
	"WINE": true,
}

// Config configures one producer.
type Config struct {
	// Name is the operator-chosen source name.
	Name string
	// Account is the producer's Vinoshipper account id. Required.
	Account int
	// BaseURL overrides DefaultBaseURL (tests).
	BaseURL string
	// FixturePath reads the feed from a file instead of the network.
	FixturePath string
	Fetch       *webfetch.Client
	Now         func() time.Time
	Logf        func(string, ...any)
}

// Connector implements listing.Connector.
type Connector struct {
	cfg          Config
	mu           sync.Mutex
	lastComplete bool
}

// NewConnector fills defaults.
func NewConnector(cfg Config) *Connector {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Fetch == nil {
		cfg.Fetch = &webfetch.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}
}

// SourceID returns "vinoshipper:<Name>".
func (c *Connector) SourceID() string { return SourceID + ":" + c.cfg.Name }

// FetchComplete reports whether the last Fetch read the whole catalogue. The
// feed is one document, so a successful decode is complete.
func (c *Connector) FetchComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastComplete
}

// feed is the subset of the wine-list document nagus reads.
type feed struct {
	Winery  *struct{ Name string } `json:"winery"`
	Wines   []wine                 `json:"wines"`
	ShipsTo []struct {
		Abbr string `json:"abbr"`
	} `json:"shipsTo"`
}

type wine struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Varietal    string   `json:"varietal"`
	Price       float64  `json:"price"`
	MSRP        *float64 `json:"msrp"`
	Inventory   *int     `json:"inventory"`
	Hidden      bool     `json:"hidden"`
	NonWine     bool     `json:"nonWineProduct"`
	URLSlug     string   `json:"urlSlug"`
	Unavailable []string `json:"unavailableInStates"`
	Brand       *struct {
		Name string `json:"name"`
	} `json:"brand"`
	Appellation *struct {
		Name string `json:"name"`
	} `json:"appellation"`
	BottleSize *struct {
		ML int `json:"ml"`
	} `json:"bottleSize"`
}

// Fetch reads the feed and returns one Raw per purchasable wine.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	c.mu.Lock()
	c.lastComplete = false
	c.mu.Unlock()
	var data []byte
	var err error
	switch {
	case c.cfg.FixturePath != "":
		data, err = os.ReadFile(c.cfg.FixturePath)
	case c.cfg.Account <= 0:
		return nil, errors.New("vinoshipper: no account id configured")
	default:
		data, err = c.cfg.Fetch.Get(ctx, fmt.Sprintf("%s/json-api/v2/wine-list?id=%d", c.cfg.BaseURL, c.cfg.Account), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("vinoshipper %s: %w", c.cfg.Name, err)
	}
	var f feed
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("vinoshipper %s: feed shape changed or not JSON: %w", c.cfg.Name, err)
	}
	if f.Wines == nil {
		return nil, fmt.Errorf("vinoshipper %s: feed has no wines array (shape changed?)", c.cfg.Name)
	}
	producerStates := map[string]bool{}
	for _, s := range f.ShipsTo {
		if a := strings.ToUpper(strings.TrimSpace(s.Abbr)); len(a) == 2 {
			producerStates[a] = true
		}
	}
	now := c.cfg.Now()
	skipped := map[string]int{}
	var out []listing.Raw
	for _, w := range f.Wines {
		switch {
		case w.Hidden:
			skipped["hidden"]++
			continue
		case w.NonWine || !wineTypes[strings.ToUpper(strings.TrimSpace(w.Type))]:
			skipped["type "+w.Type]++
			continue
		case w.Inventory != nil && *w.Inventory <= 0:
			skipped["sold out"]++
			continue
		case w.Price <= 0:
			skipped["no price"]++
			continue
		}
		out = append(out, c.raw(w, f, producerStates, now))
	}
	if len(skipped) > 0 && c.cfg.Logf != nil {
		c.cfg.Logf("vinoshipper %s: %d wines, skipped %v", c.cfg.Name, len(out), skipped)
	}
	c.mu.Lock()
	c.lastComplete = true
	c.mu.Unlock()
	return out, nil
}

func (c *Connector) raw(w wine, f feed, producerStates map[string]bool, now time.Time) listing.Raw {
	aspects := map[string]string{"wine_type": strings.ToLower(w.Type)}
	switch {
	case w.Brand != nil && w.Brand.Name != "":
		aspects["vendor"] = w.Brand.Name
	case f.Winery != nil && f.Winery.Name != "":
		aspects["vendor"] = f.Winery.Name
	}
	if v := strings.TrimSpace(w.Varietal); v != "" && !strings.EqualFold(v, "other") {
		aspects["varietal"] = v
	}
	if w.Appellation != nil && w.Appellation.Name != "" {
		aspects["appellation"] = w.Appellation.Name
	}
	if w.BottleSize != nil && w.BottleSize.ML > 0 {
		aspects["bottle_ml"] = strconv.Itoa(w.BottleSize.ML)
	}
	price := cents(w.Price)
	if w.MSRP != nil && cents(*w.MSRP) > price {
		aspects["compare_at_cents"] = strconv.FormatInt(cents(*w.MSRP), 10)
	}
	if st := shipsTo(producerStates, w.Unavailable); st != "" {
		aspects["ships_to"] = st
	}
	url := c.cfg.BaseURL + "/shop/"
	if w.URLSlug != "" {
		url += w.URLSlug
	}
	return listing.Raw{
		SourceID:   c.SourceID(),
		SourceKey:  strconv.FormatInt(w.ID, 10),
		SourceURL:  url,
		Title:      w.Name,
		Body:       w.Description,
		PriceCents: price,
		Currency:   "USD",
		Aspects:    aspects,
		SeenAt:     now,
	}
}

// shipsTo is the producer's states minus the wine's barred ones, as a sorted,
// space-separated list of ISO 3166-2 codes ("US-CA US-WA").
func shipsTo(producer map[string]bool, unavailable []string) string {
	barred := map[string]bool{}
	for _, s := range unavailable {
		barred[strings.ToUpper(strings.TrimSpace(s))] = true
	}
	var out []string
	for st := range producer {
		if !barred[st] {
			out = append(out, "US-"+st)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func cents(dollars float64) int64 { return int64(math.Round(dollars * 100)) }
