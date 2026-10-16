package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/promo"
	"github.com/leftathome/nagus/internal/store"
)

// Placeholder retailer-domain senders: real addresses live in gitops only.
func promoSource(name, profile, category, sender string) SourceConfig {
	s := SourceConfig{
		Name: name, Category: category, Type: "imap", IMAPParser: promo.ParserName,
		PromoProfile: profile, IMAPSenders: []string{sender}, IntervalMinutes: 60,
	}
	if category == "wine" {
		s.WineChannel, s.Origin = "retailer", "US-CA"
	}
	return s
}

func TestValidatePromoSource(t *testing.T) {
	good := promoSource("promo-newegg", "newegg", "hdd", "placeholder@newegg.com")
	if _, err := validatePromoSource(good); err != nil {
		t.Fatalf("good source refused: %v", err)
	}
	for name, mutate := range map[string]func(*SourceConfig){
		"unknown profile":   func(s *SourceConfig) { s.PromoProfile = "bestbuy" },
		"no profile":        func(s *SourceConfig) { s.PromoProfile = "" },
		"bad category":      func(s *SourceConfig) { s.Category = "land" },
		"no category":       func(s *SourceConfig) { s.Category = "" },
		"profile category":  func(s *SourceConfig) { s.Category, s.WineChannel, s.Origin = "wine", "retailer", "US-CA" },
		"no senders":        func(s *SourceConfig) { s.IMAPSenders = nil },
		"imapFrom":          func(s *SourceConfig) { s.IMAPFrom = "placeholder@newegg.com" },
		"foreign sender":    func(s *SourceConfig) { s.IMAPSenders = []string{"placeholder@newegg.com", "someone@example.com"} },
		"lookalike domain":  func(s *SourceConfig) { s.IMAPSenders = []string{"placeholder@newegg.com.example.com"} },
		"forwarders":        func(s *SourceConfig) { s.IMAPForwarders = []string{"household@example.com"} },
		"trust A-R":         func(s *SourceConfig) { s.IMAPTrustAuthResults = true },
		"aliases":           func(s *SourceConfig) { s.IMAPSenderAliases = map[string]string{"placeholder@newegg.com": "newegg"} },
		"fixture":           func(s *SourceConfig) { s.Fixture = "x.json" },
		"producer":          func(s *SourceConfig) { s.WineProducer = "X" },
		"negative max":      func(s *SourceConfig) { s.PromoMaxImages = -1 },
		"negative per poll": func(s *SourceConfig) { s.PromoCallsPerPoll = -1 },
	} {
		s := good
		mutate(&s)
		if _, err := validatePromoSource(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuildPromoIngesters(t *testing.T) {
	t.Setenv("NAGUS_IMAP_HOST", "imap.example.net")
	t.Setenv("NAGUS_IMAP_USERNAME", "deals@example.net")
	t.Setenv("NAGUS_IMAP_PASSWORD", "x")
	t.Setenv(envVisionBaseURL, "http://vision.example.net/v1")
	t.Setenv(envVisionModel, "test-model")
	t.Setenv(envVisionAPIKey, "sk-test-not-real")
	var logs bytes.Buffer
	logf := func(f string, a ...any) { logs.WriteString(strings.TrimSpace(fmt.Sprintf(f, a...)) + "\n") }
	rt := promoRuntimeFromEnv(logf)
	if !rt.vision.Enabled() || strings.Contains(logs.String(), "sk-test-not-real") || !strings.Contains(logs.String(), "promo vision enabled") {
		t.Fatalf("runtime: enabled %v, logs %q", rt.vision.Enabled(), logs.String())
	}
	o := categoryOpts{logf: t.Logf, promo: rt, lwinStamp: true, offers: offer.NewMemoryStore()}
	for _, s := range []SourceConfig{
		promoSource("promo-newegg", "newegg", "hdd", "placeholder@newegg.com"),
		promoSource("promo-winecom", "winecom", "wine", "placeholder@e.wine.com"),
		promoSource("promo-totalwine", "totalwine", "wine", "placeholder@email-totalwine.com"),
	} {
		ing, err := buildIngester(s, CategoryConfig{WineShipTo: "US-WA"}, store.NewMemoryStore(), o)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		// The wine bundle wraps the connector; completeness must survive it.
		if rc, ok := ing.Connector.(interface{ FetchComplete() bool }); !ok || rc.FetchComplete() {
			t.Fatalf("%s: connector %T must report the (not yet run) promo fetch incomplete", s.Name, ing.Connector)
		}
		if ing.AfterIngest == nil || ing.StaleAfter < 24*time.Hour {
			t.Errorf("%s: AfterIngest %v StaleAfter %s", s.Name, ing.AfterIngest != nil, ing.StaleAfter)
		}
		if s.Category == "wine" && ing.NameHintProducer != "wine_producer" {
			t.Errorf("%s: wine promo source must send name hints", s.Name)
		}
		if s.Category == "hdd" && !ing.HintsNeedGate {
			t.Errorf("%s: hdd promo hints must wait for the gate", s.Name)
		}
	}
	// Vision unset: the runtime is off and says which settings are missing.
	t.Setenv(envVisionAPIKey, "")
	logs.Reset()
	if off := promoRuntimeFromEnv(logf); off.vision.Enabled() || !strings.Contains(logs.String(), envVisionAPIKey) {
		t.Fatalf("off runtime: %q", logs.String())
	}
}

// Rows of a promo source say where their price came from.
func TestPromoRowsCarryOrigin(t *testing.T) {
	src := promoSource("promo-newegg", "newegg", "hdd", "placeholder@newegg.com")
	ing := &pipeline.Ingester{Connector: &fakeMail{id: "imap:promo-newegg"}}
	pols := buildSourcePolicies([]SourceConfig{src}, []*pipeline.Ingester{ing})
	srv := &server{sourcePolicies: pols}
	now := time.Now().UTC()
	got := srv.timingFor(offer.Summary{SourceID: "imap:promo-newegg", Status: offer.StatusActive, FirstSeen: now, LastSeen: now, EndsAt: now.Add(48 * time.Hour)})
	if got.OfferOrigin != promo.OriginEmailImage || got.SubmittedBy != "retailer:newegg" || got.OfferOriginNote != promo.OriginNote || got.ExpiresBasis != basisEmailWindow {
		t.Fatalf("timing %+v", got)
	}
	b, _ := json.Marshal(got)
	for _, k := range []string{`"offer_origin":"email_image"`, `"submitted_by":"retailer:newegg"`, `"offer_origin_note":`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("row JSON missing %s: %s", k, b)
		}
	}
	// Expired rows still say so.
	if exp := srv.timingFor(offer.Summary{SourceID: "imap:promo-newegg", Status: offer.StatusExpired}); exp.OfferOrigin != promo.OriginEmailImage {
		t.Fatal("expired promo row lost its origin")
	}
	// Other sources' rows are unchanged.
	other := srv.timingFor(offer.Summary{SourceID: "shopify:x", Status: offer.StatusActive, FirstSeen: now, LastSeen: now})
	if other.OfferOrigin != "" || other.SubmittedBy != "" || other.OfferOriginNote != "" {
		t.Fatalf("non-promo row %+v", other)
	}
}

func TestPromoMetricsRoute(t *testing.T) {
	srv := newTestServer(t)
	b := promo.NewBudget(10, nil)
	srv.promo = promo.NewMetrics(b, false)
	srv.promo.Register("imap:promo-newegg")
	body := do(t, srv, http.MethodGet, "/metrics").Body.String()
	for _, want := range []string{
		"nagus_promo_vision_enabled 0\n",
		`nagus_promo_messages_total{source="imap:promo-newegg",outcome="disabled"} 0` + "\n",
		"nagus_promo_vision_calls_per_day_limit 10\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
	if strings.Contains(do(t, newTestServer(t), http.MethodGet, "/metrics").Body.String(), "nagus_promo_") {
		t.Fatal("promo metrics without promo sources")
	}
}

// wineShipsTo: an operator declaration per wine source, validated at startup.
func TestWineShipsToConfig(t *testing.T) {
	t.Setenv("NAGUS_IMAP_HOST", "imap.example.net")
	t.Setenv("NAGUS_IMAP_USERNAME", "deals@example.net")
	t.Setenv("NAGUS_IMAP_PASSWORD", "x")
	o := categoryOpts{logf: t.Logf, promo: promoRuntimeFromEnv(nil)}
	base := promoSource("promo-winecom", "winecom", "wine", "placeholder@e.wine.com")
	us := []string{"US"}
	ok := base
	ok.WineShipsTo = &us
	if _, err := buildIngester(ok, CategoryConfig{WineShipTo: "US-WA"}, store.NewMemoryStore(), o); err != nil {
		t.Fatalf("US declaration refused: %v", err)
	}
	for name, codes := range map[string][]string{"empty": {}, "bad code": {"USA"}, "unmodeled": {"US-ZZ"}} {
		s := base
		c := codes
		s.WineShipsTo = &c
		if _, err := buildIngester(s, CategoryConfig{WineShipTo: "US-WA"}, store.NewMemoryStore(), o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	hdd := promoSource("promo-newegg", "newegg", "hdd", "placeholder@newegg.com")
	hdd.WineShipsTo = &us
	if _, err := buildIngester(hdd, CategoryConfig{}, store.NewMemoryStore(), o); err == nil {
		t.Error("wineShipsTo accepted on an hdd source")
	}
	var cfg SourceConfig
	if err := json.Unmarshal([]byte(`{"name":"x","wineShipsTo":[]}`), &cfg); err != nil || cfg.WineShipsTo == nil || len(*cfg.WineShipsTo) != 0 {
		t.Fatal("an empty wineShipsTo must stay distinguishable from an absent one")
	}
}
