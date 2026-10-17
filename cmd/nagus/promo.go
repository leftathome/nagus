package main

// Retailer promo mail read by a vision model (promo-vision-v1, nagus-3ua;
// design docs/design/2026-10-16-promo-vision.md). One imap source per
// retailer, imapParser "promo-vision-v1", promoProfile naming a built-in
// profile. Every promo source shares one promoRuntime: the vision client,
// the daily call budget (the GPU host is shared), the HTTP client for images
// and links, and the metrics.

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/promo"
)

// Vision settings (env). The feature is OFF unless base URL, key and model
// are all set.
const (
	envVisionBaseURL = "NAGUS_VISION_BASE_URL"
	envVisionAPIKey  = "NAGUS_VISION_API_KEY"
	envVisionModel   = "NAGUS_VISION_MODEL"
	envVisionPerPoll = "NAGUS_VISION_MAX_CALLS_PER_POLL"
	envVisionPerDay  = "NAGUS_VISION_MAX_CALLS_PER_DAY"
	envVisionTimeout = "NAGUS_VISION_TIMEOUT"
)

// promoUserAgent identifies nagus on retailer image hosts and redirectors.
const promoUserAgent = "nagus-promo-vision/1 (+https://github.com/leftathome/nagus)"

// promoRuntime is the state every promo source shares.
type promoRuntime struct {
	vision   *promo.Vision
	budget   *promo.Budget
	metrics  *promo.Metrics
	fetcher  *promo.Fetcher
	resolver *promo.Resolver
	perPoll  int
	// state persists processed messages, readings and the daily usage in
	// the offers database (nagus-p7x); nil without the offer layer.
	state offer.MailState
}

// useState persists the runtime's state in store when it keeps mail state.
func (rt *promoRuntime) useState(store offer.Store) {
	if ms, ok := store.(offer.MailState); ok && rt.state == nil {
		rt.state = ms
		rt.budget.State = ms
	}
}

// promoStateWindow is how long a promo source's persisted state is kept:
// its mail is re-read for the lookback window, plus two days of slack.
func promoStateWindow(s SourceConfig) time.Duration {
	days := s.IMAPLookbackDays
	if days <= 0 {
		days = imapmail.DefaultLookbackDays
	}
	return time.Duration(days+2) * 24 * time.Hour
}

// isPromoSource reports whether a source reads retailer promo mail.
func isPromoSource(s SourceConfig) bool {
	return s.Type == "imap" && s.IMAPParser == promo.ParserName
}

// isMailDealSource is a deal submission or a promo source: offers read from
// the deals mailbox, listed while their mail is inside the lookback window,
// whose hints reach quark only after the gate passed them.
func isMailDealSource(s SourceConfig) bool { return isDealSource(s) || isPromoSource(s) }

// validatePromoSource checks one promo source's declaration.
func validatePromoSource(s SourceConfig) (*promo.Profile, error) {
	p, err := promo.Lookup(strings.TrimSpace(s.PromoProfile))
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", s.Name, err)
	}
	switch {
	case s.Category != deal.CategoryWine && s.Category != deal.CategoryHDD:
		return nil, fmt.Errorf("source %q: a %s source needs category wine or hdd (profile %s is %s)", s.Name, promo.ParserName, p.Name, p.Category)
	case s.Category != p.Category:
		return nil, fmt.Errorf("source %q: profile %s is category %s, not %s", s.Name, p.Name, p.Category, s.Category)
	case s.IMAPFrom != "":
		return nil, fmt.Errorf("source %q: a %s source takes its senders from imapSenders, not imapFrom", s.Name, promo.ParserName)
	case len(lowerSet(s.IMAPSenders)) == 0:
		return nil, fmt.Errorf("source %q: a %s source needs imapSenders (the retailer's newsletter addresses, configured in gitops)", s.Name, promo.ParserName)
	case len(s.IMAPForwarders) > 0:
		// A forward is credited to the forwarder, while the offer would claim
		// the retailer: the two cannot both be true.
		return nil, fmt.Errorf("source %q: a %s source does not read forwards; drop imapForwarders", s.Name, promo.ParserName)
	case s.IMAPTrustAuthResults:
		return nil, fmt.Errorf("source %q: a %s source never trusts Authentication-Results headers; drop imapTrustAuthResults", s.Name, promo.ParserName)
	case len(s.IMAPSenderAliases) > 0 || s.DealSubmitTo != "":
		return nil, fmt.Errorf("source %q: a %s source has no aliases or submit address (its principal is retailer:%s)", s.Name, promo.ParserName, p.Name)
	case s.WineProducer != "" || s.ProducerFromBody:
		return nil, fmt.Errorf("source %q: a %s source takes each product's producer from the image; drop wineProducer/producerFromBody", s.Name, promo.ParserName)
	case s.Fixture != "":
		return nil, fmt.Errorf("source %q: a %s source has no fixture mode", s.Name, promo.ParserName)
	case s.PromoMaxImages < 0 || s.PromoCallsPerPoll < 0:
		return nil, fmt.Errorf("source %q: promoMaxImages and promoCallsPerPoll must not be negative", s.Name)
	}
	for _, a := range lowerSet(s.IMAPSenders) {
		if !p.SenderAllowed(a) {
			return nil, fmt.Errorf("source %q: sender %q is not on the %s profile's sender domains %v", s.Name, a, p.Name, p.SenderDomains)
		}
	}
	return p, nil
}

// promoRuntimeFromEnv builds the shared promo state. It is built whether or
// not vision is configured: unconfigured, promo sources poll but read
// nothing, and the metrics say so.
func promoRuntimeFromEnv(logf func(string, ...any)) *promoRuntime {
	v := &promo.Vision{
		BaseURL: strings.TrimSpace(envOr(envVisionBaseURL, "")),
		APIKey:  strings.TrimSpace(envOr(envVisionAPIKey, "")),
		Model:   strings.TrimSpace(envOr(envVisionModel, "")),
		Timeout: envDuration(envVisionTimeout, 90*time.Second),
		// The vision endpoint is in-cluster: an ordinary client, no
		// public-address rule.
		Client: &http.Client{Timeout: envDuration(envVisionTimeout, 90*time.Second) + 5*time.Second},
	}
	budget := promo.NewBudget(int(envInt64(envVisionPerDay, promo.DefaultCallsPerDay)), nil)
	budget.Logf = logf
	rt := &promoRuntime{
		vision:  v,
		budget:  budget,
		metrics: promo.NewMetrics(budget, v.Enabled()),
		perPoll: int(envInt64(envVisionPerPoll, promo.DefaultCallsPerPoll)),
	}
	client := promo.NewPublicClient(promo.DefaultFetchTimeout)
	rt.fetcher = &promo.Fetcher{Client: client, Retries: promo.DefaultFetchRetries, UserAgent: promoUserAgent}
	rt.resolver = &promo.Resolver{Client: client, UserAgent: promoUserAgent}
	if logf != nil {
		if v.Enabled() {
			// Never the key.
			logf("promo vision enabled (model=%s, %d calls/poll, %d calls/day)", v.Model, rt.perPoll, budget.PerDay)
		} else {
			var missing []string
			for k, val := range map[string]string{envVisionBaseURL: v.BaseURL, envVisionAPIKey: v.APIKey, envVisionModel: v.Model} {
				if val == "" {
					missing = append(missing, k)
				}
			}
			slices.Sort(missing)
			logf("promo vision off: %s unset (promo-vision-v1 sources read nothing)", strings.Join(missing, ", "))
		}
	}
	return rt
}

// buildPromoConnector builds a promo source's imap connector and wrapper.
func buildPromoConnector(s SourceConfig, o categoryOpts) (listing.Connector, error) {
	prof, err := validatePromoSource(s)
	if err != nil {
		return nil, err
	}
	rt := o.promo
	if rt == nil {
		rt = promoRuntimeFromEnv(o.logf)
	}
	rt.useState(o.offers)
	if s.PromoMaxImages > 0 && s.PromoMaxImages < prof.MaxImages {
		cp := *prof
		cp.MaxImages = s.PromoMaxImages
		prof = &cp
	}
	perPoll := rt.perPoll
	if s.PromoCallsPerPoll > 0 {
		perPoll = s.PromoCallsPerPoll
	}
	p, err := promo.NewParser(promo.Config{
		Profile: prof, Category: s.Category, SourceID: imapmail.SourceID + ":" + s.Name,
		Vision: rt.vision, Fetcher: rt.fetcher, Resolver: rt.resolver, Budget: rt.budget, Metrics: rt.metrics,
		PerPoll: perPoll, LookbackDays: s.IMAPLookbackDays, Logf: o.logf, State: rt.state,
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", s.Name, err)
	}
	conn, err := imapmail.NewConnector(imapmail.Config{
		Name: s.Name, Senders: s.IMAPSenders, Mailbox: s.IMAPMailbox, LookbackDays: s.IMAPLookbackDays,
		Parser: p, Logf: o.logf, NewestFirst: true,
		Host: envOr("NAGUS_IMAP_HOST", ""), Port: envOr("NAGUS_IMAP_PORT", ""), TLS: envOr("NAGUS_IMAP_TLS", ""),
		Username: envOr("NAGUS_IMAP_USERNAME", ""), Password: envOr("NAGUS_IMAP_PASSWORD", ""),
		TrustARCSealers: s.IMAPTrustARCSealers, TrustARCAuthServ: s.IMAPTrustARCAuthservIDs,
		TrustARCRecipients: s.IMAPTrustARCRecipients, ARCMaxAge: time.Duration(s.IMAPTrustARCMaxAgeMinutes) * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", s.Name, err)
	}
	o.registerIMAP(conn)
	return &promo.Source{Inner: conn, Parser: p}, nil
}

// promoMetricsOf returns the metrics a promo connector reports to.
func promoMetricsOf(c listing.Connector) *promo.Metrics {
	if ps, ok := c.(*promo.Source); ok {
		return ps.Metrics()
	}
	return nil
}

// writeIMAPAuthMetrics renders how the imap sources authenticated mail (direct
// DKIM, a trusted Authentication-Results header, or a trusted ARC seal), when
// any imap source is configured.
func writeIMAPAuthMetrics(w io.Writer, ingesters []*pipeline.Ingester) {
	has := false
	for _, ing := range ingesters {
		if strings.HasPrefix(ing.SourceID(), imapmail.SourceID+":") {
			has = true
		}
	}
	if !has {
		return
	}
	var b strings.Builder
	b.WriteString("# HELP nagus_imap_authenticated_total Mail verifications by the path that authenticated them (every poll re-verifies the lookback window).\n")
	b.WriteString("# TYPE nagus_imap_authenticated_total counter\n")
	st := imapmail.AuthStats()
	for _, p := range imapmail.AuthPaths {
		fmt.Fprintf(&b, "nagus_imap_authenticated_total{path=%q} %d\n", p, st[p])
	}
	_, _ = io.WriteString(w, b.String())
}

// arcTrustDeclared reports whether a source sets any ARC trust key.
func arcTrustDeclared(s SourceConfig) bool {
	return len(s.IMAPTrustARCSealers) > 0 || len(s.IMAPTrustARCAuthservIDs) > 0 || len(s.IMAPTrustARCRecipients) > 0 ||
		s.IMAPTrustARCMaxAgeMinutes != 0
}
