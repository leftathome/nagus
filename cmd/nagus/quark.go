package main

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/leftathome/nagus/internal/enrich"
	"github.com/leftathome/nagus/internal/offer"
	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/quark"
)

// defaultQuarkInterval is how often the resolution pass runs. Ingest runs every
// 30-60 minutes per source, so 10 minutes means a new offer carries a product
// id well before the next ingest of its store, without polling quark hard.
const defaultQuarkInterval = 10 * time.Minute

// defaultQuarkBatch and defaultQuarkTimeout are sized from MEASURED production
// latency, not from quark's schema cap. quark commits each hint on replicated
// block storage; on orac it served ~270ms per hint (2026-09-13, 105 hints over
// four calls, 42s). At the schema cap of 500 a call takes minutes, so with the
// old 30s client timeout every large call failed -- AFTER quark had applied it
// -- and the same offers were re-sent every pass without ever being recorded.
// 100 hints at 270ms is ~27s, inside a 2m timeout with 4x headroom. Both are
// env-tunable so a slower or faster store is a values edit, not a release.
const (
	defaultQuarkBatch   = 100
	defaultQuarkTimeout = 2 * time.Minute
)

// buildEnricher wires the asynchronous quark resolution pass from env, or
// returns nil with the reason it is off.
//
// It is OFF, not an error, in every case where it cannot run -- no quark URL
// (local runs, and every deployment before quark existed), no offer layer, or
// no token. A missing credential must never become an ingest or surface outage;
// offers simply stay unattempted and the reason is logged at startup.
func buildEnricher(offers offer.Store, cfg RunConfig, ingesters []*pipeline.Ingester, logf func(string, ...any)) (*enrich.Enricher, string) {
	url := envOr("NAGUS_QUARK_URL", "")
	switch {
	case url == "":
		return nil, "NAGUS_QUARK_URL unset"
	case offers == nil:
		return nil, "the offer layer is disabled (set NAGUS_OFFERS)"
	}
	token := envOr("NAGUS_QUARK_TOKEN", "")
	if token == "" {
		return nil, "NAGUS_QUARK_TOKEN unset"
	}

	// Offers carry a source id, not a category; quark needs the category. The
	// map is keyed by the ingester's SourceID, the same value stored on offers.
	cats := make(map[string]string, len(ingesters))
	for i, ing := range ingesters {
		if i < len(cfg.Sources) {
			cats[ing.SourceID()] = cfg.Sources[i].Category
		}
	}
	return &enrich.Enricher{
		Offers:      offers,
		Quark:       &quark.Client{BaseURL: url, Token: token, HTTP: &http.Client{Timeout: envDuration("NAGUS_QUARK_TIMEOUT", defaultQuarkTimeout)}},
		Interval:    envDuration("NAGUS_QUARK_INTERVAL", defaultQuarkInterval),
		BatchSize:   int(envInt64("NAGUS_QUARK_BATCH_SIZE", defaultQuarkBatch)),
		CategoryFor: func(src string) string { return cats[src] },
		Logf:        logf,
	}, ""
}

// writeEnrichMetrics renders the resolution pass counters in Prometheus text.
func writeEnrichMetrics(w io.Writer, s enrich.Stats) {
	fmt.Fprintf(w, "# HELP nagus_quark_batches_total quark resolve calls, by outcome.\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_batches_total counter\n")
	fmt.Fprintf(w, "nagus_quark_batches_total{outcome=\"ok\"} %d\n", s.BatchesOK)
	fmt.Fprintf(w, "nagus_quark_batches_total{outcome=\"failed\"} %d\n", s.BatchesFailed)
	fmt.Fprintf(w, "# HELP nagus_quark_unauthorized_total quark calls rejected 401 (a token problem, not a transient one).\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_unauthorized_total counter\n")
	fmt.Fprintf(w, "nagus_quark_unauthorized_total %d\n", s.Unauthorized)
	fmt.Fprintf(w, "# HELP nagus_quark_offers_recorded_total Offer resolutions recorded, by resulting state.\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_offers_recorded_total counter\n")
	fmt.Fprintf(w, "nagus_quark_offers_recorded_total{state=\"resolved\"} %d\n", s.Resolved)
	fmt.Fprintf(w, "nagus_quark_offers_recorded_total{state=\"refused\"} %d\n", s.Refused)
	fmt.Fprintf(w, "nagus_quark_offers_recorded_total{state=\"quarantined\"} %d\n", s.Quarantined)
	fmt.Fprintf(w, "nagus_quark_offers_recorded_total{state=\"unidentifiable\"} %d\n", s.Unidentifiable)
	fmt.Fprintf(w, "# HELP nagus_quark_answers_discarded_total Answers dropped because the offer's hint changed while quark was answering.\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_answers_discarded_total counter\n")
	fmt.Fprintf(w, "nagus_quark_answers_discarded_total %d\n", s.Discarded)
	fmt.Fprintf(w, "# HELP nagus_quark_retries_total Refused offers re-offered after quark's catalog generation advanced.\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_retries_total counter\n")
	fmt.Fprintf(w, "nagus_quark_retries_total %d\n", s.Retries)
	fmt.Fprintf(w, "# HELP nagus_quark_catalog_generation Highest catalog generation quark has reported.\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_catalog_generation gauge\n")
	fmt.Fprintf(w, "nagus_quark_catalog_generation %d\n", s.Generation)
	fmt.Fprintf(w, "# HELP nagus_quark_last_clean_pass_timestamp_seconds When a resolution pass last finished without error (0 = never since start).\n")
	fmt.Fprintf(w, "# TYPE nagus_quark_last_clean_pass_timestamp_seconds gauge\n")
	fmt.Fprintf(w, "nagus_quark_last_clean_pass_timestamp_seconds %d\n", s.LastCleanPass)
}
