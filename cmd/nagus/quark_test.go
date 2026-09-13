package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/enrich"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/quark"
)

func quarkEnv(t *testing.T, url, token, interval string) {
	t.Helper()
	t.Setenv("NAGUS_QUARK_URL", url)
	t.Setenv("NAGUS_QUARK_TOKEN", token)
	t.Setenv("NAGUS_QUARK_INTERVAL", interval)
}

func testIngesters() ([]*pipeline.Ingester, RunConfig) {
	ings := []*pipeline.Ingester{
		{Connector: &loopFakeConnector{id: "shopify:spd"}},
		{Connector: &loopFakeConnector{id: "shopify:smm"}},
	}
	cfg := RunConfig{Sources: []SourceConfig{
		{Name: "spd", Category: "hdd"},
		{Name: "smm"}, // offer-only: no category
	}}
	return ings, cfg
}

// Every reason the pass cannot run must turn it OFF with a stated reason, never
// fail startup: a missing quark credential must not become an ingest outage.
func TestBuildEnricherIsOffWithAReason(t *testing.T) {
	ings, cfg := testIngesters()
	offers := offer.NewMemoryStore()
	for _, tc := range []struct {
		name, url, token string
		offers           offer.Store
		why              string
	}{
		{"no url", "", "tok", offers, "NAGUS_QUARK_URL"},
		{"no offer layer", "http://quark", "tok", nil, "offer layer"},
		{"no token", "http://quark", "", offers, "NAGUS_QUARK_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quarkEnv(t, tc.url, tc.token, "")
			e, why := buildEnricher(tc.offers, cfg, ings, nil)
			if e != nil {
				t.Fatal("enricher built without its prerequisites")
			}
			if !strings.Contains(why, tc.why) {
				t.Fatalf("reason %q does not name %q", why, tc.why)
			}
		})
	}
}

func TestBuildEnricherMapsSourceToCategory(t *testing.T) {
	ings, cfg := testIngesters()
	quarkEnv(t, "http://quark.quark.svc.cluster.local:8080", "tok", "3m")
	e, why := buildEnricher(offer.NewMemoryStore(), cfg, ings, nil)
	if e == nil {
		t.Fatalf("enricher off: %s", why)
	}
	if got := e.CategoryFor("shopify:spd"); got != "hdd" {
		t.Errorf("category for shopify:spd = %q, want hdd", got)
	}
	if got := e.CategoryFor("shopify:smm"); got != "" {
		t.Errorf("offer-only source got category %q, want empty (quark refuses it)", got)
	}
	if e.Interval != 3*time.Minute {
		t.Errorf("interval = %s, want 3m from NAGUS_QUARK_INTERVAL", e.Interval)
	}
}

// The batch and timeout defaults are what keep a large backlog draining: a call
// that outlives the client timeout is applied by quark but discarded here, and
// is re-sent forever. See defaultQuarkBatch.
func TestBuildEnricherBatchAndTimeoutDefaultsAndOverrides(t *testing.T) {
	ings, cfg := testIngesters()
	timeoutOf := func(t *testing.T, e *enrich.Enricher) time.Duration {
		t.Helper()
		c, ok := e.Quark.(*quark.Client)
		if !ok || c.HTTP == nil {
			t.Fatalf("quark client = %T, want *quark.Client with an HTTP client", e.Quark)
		}
		return c.HTTP.Timeout
	}

	quarkEnv(t, "http://quark", "tok", "")
	e, why := buildEnricher(offer.NewMemoryStore(), cfg, ings, nil)
	if e == nil {
		t.Fatalf("enricher off: %s", why)
	}
	if e.BatchSize != defaultQuarkBatch || timeoutOf(t, e) != defaultQuarkTimeout {
		t.Errorf("defaults: batch %d timeout %s, want %d and %s",
			e.BatchSize, timeoutOf(t, e), defaultQuarkBatch, defaultQuarkTimeout)
	}
	// A batch this size at the measured per-hint latency must fit the timeout.
	if measured := time.Duration(defaultQuarkBatch) * 270 * time.Millisecond; measured*2 > defaultQuarkTimeout {
		t.Errorf("default batch at 270ms/hint = %s, under 2x headroom of the %s timeout", measured, defaultQuarkTimeout)
	}

	t.Setenv("NAGUS_QUARK_BATCH_SIZE", "25")
	t.Setenv("NAGUS_QUARK_TIMEOUT", "45s")
	e, _ = buildEnricher(offer.NewMemoryStore(), cfg, ings, nil)
	if e.BatchSize != 25 || timeoutOf(t, e) != 45*time.Second {
		t.Errorf("overrides: batch %d timeout %s, want 25 and 45s", e.BatchSize, timeoutOf(t, e))
	}
}

func TestEnrichMetricsAreValidPrometheusText(t *testing.T) {
	var buf bytes.Buffer
	writeEnrichMetrics(&buf, enrich.Stats{BatchesOK: 3, BatchesFailed: 1, Resolved: 10, Refused: 4, Generation: 2})
	out := buf.String()
	for _, want := range []string{
		`nagus_quark_batches_total{outcome="ok"} 3`,
		`nagus_quark_batches_total{outcome="failed"} 1`,
		`nagus_quark_offers_recorded_total{state="resolved"} 10`,
		`nagus_quark_catalog_generation 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// Each family's HELP/TYPE appears exactly once (the text format forbids repeats).
	if n := strings.Count(out, "# TYPE nagus_quark_batches_total"); n != 1 {
		t.Errorf("TYPE line for nagus_quark_batches_total appears %d times", n)
	}
}
