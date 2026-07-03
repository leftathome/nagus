// Package craigslist implements a nagus-direct listing.Connector over
// Craigslist's per-city search RSS feeds (RDF/RSS 1.0), e.g.
//
//	https://<city>.craigslist.org/search/<cat>?format=rss
//
// It is a plain unauthenticated GET + XML parse: no scraping of rendered
// HTML, no glovebox connector hop, just an RDF feed mapped straight into
// listing.Raw. Craigslist RSS feeds are intentionally terse (title, link,
// description, dc:date) with no structured price/condition fields, so this
// connector recovers price and a location aspect from the free-text title
// with regexes; everything else it maps directly.
//
// # Trust boundary
//
// Title, Body, and Aspects values on the emitted Raw are UNTRUSTED free text
// per internal/listing's contract; this connector does no sanitization, only
// field mapping. Body carries the feed's raw (HTML-bearing) description
// untouched -- sanitizing/stripping it is the glovebox boundary's job, not
// this connector's. PriceCents and Currency are derived/structured scalars
// and are treated as trusted once computed here.
package craigslist

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/listing"
)

// SourceID is the stable connector identity stamped onto every Raw this
// package emits.
const SourceID = "craigslist"

// Defaults for Config fields left unset by the caller.
const (
	// DefaultBaseURL is a format string: "%s" is replaced with Config.City to
	// build the per-city host, e.g. "https://sfbay.craigslist.org". Override
	// with a plain scheme+host (no "%s") in tests to point at an
	// httptest.Server; see (*Connector).feedURL.
	DefaultBaseURL = "https://%s.craigslist.org"
	// DefaultCategory is used when Config.Category is empty. "reo" is
	// Craigslist's real-estate-by-owner category (includes bare land), the
	// category of primary interest to nagus.
	DefaultCategory = "reo"
	// DefaultUserAgent identifies this connector politely. Craigslist 403s
	// requests with an empty or absent User-Agent, so Fetch always sends one.
	DefaultUserAgent = "nagus/0.1 (+https://gitlab.orac.local/agentic/nagus)"
)

// Config configures a Connector.
type Config struct {
	// City is the Craigslist subdomain to search, e.g. "sfbay". Required.
	City string
	// Category is the Craigslist search category, e.g. "reo" (real estate by
	// owner), "sss" (all for-sale), "cta" (cars+trucks by owner). Defaults to
	// DefaultCategory when empty.
	Category string

	// HTTPClient performs HTTP requests. Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// BaseURL is either a "%s"-format string for the per-city host (the
	// default) or, in tests, a plain scheme+host (e.g. an httptest.Server's
	// URL) to hit directly. See feedURL for how the two are distinguished.
	BaseURL string
	// UserAgent is sent on every request. Defaults to DefaultUserAgent.
	//
	// In-cluster egress for nagus is residential (not flagged datacenter/VPN
	// ranges), so Craigslist does not IP-block this connector the way it does
	// known cloud egress. That headroom is not license to hammer the feed:
	// keep a polite poll cadence (roughly every 30-60 minutes per
	// city/category). This connector does not sleep or schedule itself --
	// that cadence is the caller's responsibility (the spine's fetch
	// scheduler), not something Fetch enforces.
	UserAgent string
	// Now returns the current time; used as the SeenAt fallback when an
	// item's dc:date is missing or unparseable. Defaults to time.Now.
	Now func() time.Time

	// FixturePath, when non-empty, makes Fetch read a local RSS/RDF file
	// instead of making any network call -- the offline proving path.
	FixturePath string
}

// Connector implements listing.Connector over a Craigslist city/category
// search RSS feed.
type Connector struct {
	cfg Config
}

// NewConnector builds a Connector from cfg, filling in defaults for any
// unset seam.
func NewConnector(cfg Config) *Connector {
	if cfg.Category == "" {
		cfg.Category = DefaultCategory
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Connector{cfg: cfg}
}

// SourceID returns the stable connector identity stamped onto every Raw
// this Connector emits.
func (c *Connector) SourceID() string {
	return SourceID
}

// feedURL builds the search RSS URL. When Config.BaseURL still contains the
// "%s" city placeholder (the default, or any caller-supplied format string),
// it is formatted with Config.City to produce the per-city host. Otherwise
// BaseURL is treated as a full scheme+host already (the httptest.Server
// override case) and used as-is.
func (c *Connector) feedURL() string {
	base := c.cfg.BaseURL
	if strings.Contains(base, "%s") {
		base = fmt.Sprintf(base, c.cfg.City)
	}
	return strings.TrimRight(base, "/") + "/search/" + c.cfg.Category + "?format=rss"
}

// Fetch returns the current search feed as listing.Raw. When
// Config.FixturePath is set, Fetch reads that file instead of calling the
// network at all -- the offline proving path documented at package level.
func (c *Connector) Fetch(ctx context.Context) ([]listing.Raw, error) {
	var data []byte
	if c.cfg.FixturePath != "" {
		d, err := os.ReadFile(c.cfg.FixturePath)
		if err != nil {
			return nil, fmt.Errorf("craigslist: read fixture %s: %w", c.cfg.FixturePath, err)
		}
		data = d
	} else {
		d, err := c.fetchRemote(ctx)
		if err != nil {
			return nil, err
		}
		data = d
	}

	var feed rdfFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return nil, fmt.Errorf("craigslist: decode rss: %w", err)
	}

	now := c.cfg.Now()
	raws := make([]listing.Raw, 0, len(feed.Items))
	for _, it := range feed.Items {
		r, ok := mapItem(it, now)
		if !ok {
			// No usable link/rdf:about at all: cannot form provenance
			// (SourceKey), so this item cannot become a valid Raw. Skip
			// rather than emit a broken record.
			continue
		}
		raws = append(raws, r)
	}
	return raws, nil
}

// fetchRemote issues the GET against the search feed URL, always sending a
// User-Agent (Craigslist 403s empty-UA requests), and returns the raw
// response body on a 2xx status.
func (c *Connector) fetchRemote(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.feedURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("craigslist: build request: %w", err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")

	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("craigslist: request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("craigslist: read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("craigslist: search endpoint returned status %d: %s", resp.StatusCode, truncate(data, 200))
	}
	return data, nil
}

// --- RDF/RSS 1.0 feed shape -------------------------------------------------

// rdfFeed is the top-level <rdf:RDF> element of a Craigslist search feed.
// encoding/xml matches struct-tag element/attribute names against the LOCAL
// part of the XML name (ignoring namespace), so "about,attr" matches
// rdf:about and "date" matches dc:date without needing namespace-qualified
// tags.
type rdfFeed struct {
	XMLName xml.Name  `xml:"RDF"`
	Items   []rdfItem `xml:"item"`
}

// rdfItem is one <item> in the feed.
type rdfItem struct {
	About       string `xml:"about,attr"`
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Date        string `xml:"date"`
}

// --- mapping ----------------------------------------------------------------

// listingIDRe extracts the numeric listing id Craigslist embeds at the end
// of a posting URL, e.g. ".../d/santa-rosa/1234567890.html" -> "1234567890".
var listingIDRe = regexp.MustCompile(`(\d+)\.html`)

// priceRe finds the first dollar amount in a title, e.g. "$45,000" or
// "$199,999.00". Fractional cents, if present, are always exactly 2 digits.
var priceRe = regexp.MustCompile(`\$[\d,]+(?:\.\d{2})?`)

// locationRe captures trailing parenthesized text in a title, Craigslist's
// convention for a neighborhood/area label, e.g.
// "5 Acres ... - $45,000 (Sonoma County)" -> "Sonoma County".
var locationRe = regexp.MustCompile(`\(([^()]+)\)\s*$`)

// mapItem converts one feed item into a listing.Raw. It returns ok=false
// when the item has neither a usable link nor rdf:about: without one there
// is no source-native key or URL to stamp as provenance, so the item cannot
// become a valid Raw and must be skipped rather than emitted broken.
func mapItem(it rdfItem, now time.Time) (listing.Raw, bool) {
	link := strings.TrimSpace(it.Link)
	about := strings.TrimSpace(it.About)

	sourceURL := link
	if sourceURL == "" {
		sourceURL = about
	}

	sourceKey := ""
	if m := listingIDRe.FindStringSubmatch(link); len(m) == 2 {
		sourceKey = m[1]
	} else if sourceURL != "" {
		// No numeric listing id parseable from the link: fall back to the
		// full URL (link, or rdf:about if link was empty) as the
		// source-native key.
		sourceKey = sourceURL
	}
	if sourceKey == "" {
		return listing.Raw{}, false
	}

	title := it.Title
	aspects := map[string]string{}
	if loc := parseLocation(title); loc != "" {
		aspects["location"] = loc
	}

	return listing.Raw{
		SourceID:     SourceID,
		SourceKey:    sourceKey,
		SourceURL:    sourceURL,
		Title:        title,
		Body:         it.Description, // UNTRUSTED free text; left as-is (HTML and all)
		PriceCents:   parsePriceCents(title),
		Currency:     "USD",
		ConditionRaw: "",
		Aspects:      aspects,
		SeenAt:       parseSeenAt(it.Date, now),
	}, true
}

// parsePriceCents finds the first dollar amount in title (e.g. "$45,000" or
// "$199,999.00") and converts it to integer cents. It returns 0 ("unknown")
// when no price is present in the title -- unpriced land listings are
// common on Craigslist and must still be emitted, not dropped.
func parsePriceCents(title string) int64 {
	m := priceRe.FindString(title)
	if m == "" {
		return 0
	}
	numStr := strings.ReplaceAll(strings.TrimPrefix(m, "$"), ",", "")

	whole, frac, hasFrac := strings.Cut(numStr, ".")
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0
	}
	var cents int64
	if hasFrac {
		c, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0
		}
		cents = c
	}
	return dollars*100 + cents
}

// parseLocation returns the trailing parenthesized text in title (e.g.
// "Sonoma County" from "... - $45,000 (Sonoma County)"), or "" if the title
// has no such trailing group.
func parseLocation(title string) string {
	m := locationRe.FindStringSubmatch(title)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// parseSeenAt parses dc:date as RFC3339. If dateStr is empty or
// unparseable, it falls back to now (the connector's observation time).
func parseSeenAt(dateStr string, now time.Time) time.Time {
	dateStr = strings.TrimSpace(dateStr)
	if dateStr == "" {
		return now
	}
	t, err := time.Parse(time.RFC3339, dateStr)
	if err != nil {
		return now
	}
	return t
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
