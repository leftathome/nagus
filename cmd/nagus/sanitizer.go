package main

import (
	"fmt"
	"io"
	"net/http"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
)

// sanitizerFromEnv builds the glovebox sanitize gate from
// NAGUS_GLOVEBOX_SANITIZE_URL and the token, given either as
// NAGUS_GLOVEBOX_TOKEN_FILE (a mounted Secret, re-read so a late sync or a
// rotation heals without a restart -- nagus-4g3) or NAGUS_GLOVEBOX_TOKEN.
//
//   - Nothing set: nil, and every category keeps the in-process Passthrough --
//     the behavior before the gate existed.
//   - URL and a token (or token file): the gate, fail closed.
//   - Only one side: a deployment that asked to be gated but cannot be. It is
//     NEVER allowed to run ungated, and it does not refuse to start either:
//     the Deployment is Recreate, so a pod that will not start is an outage
//     of every surface. It starts, serves what it already holds, and drops
//     every NEW listing (sanitize.Closed), with the reason logged.
func sanitizerFromEnv(hc *http.Client, logf func(string, ...any)) (listing.Sanitizer, error) {
	url := envOr("NAGUS_GLOVEBOX_SANITIZE_URL", "")
	token := envOr("NAGUS_GLOVEBOX_TOKEN", "")
	file := envOr("NAGUS_GLOVEBOX_TOKEN_FILE", "")
	haveToken := token != "" || file != ""
	switch {
	case url == "" && !haveToken:
		return nil, nil
	case url == "" || !haveToken:
		reason := "no NAGUS_GLOVEBOX_TOKEN or NAGUS_GLOVEBOX_TOKEN_FILE"
		if url == "" {
			reason = "NAGUS_GLOVEBOX_SANITIZE_URL is empty"
		}
		if logf != nil {
			logf("sanitize: GATE HALF-CONFIGURED -- %s; every new listing will be DROPPED (fail closed) until both are set", reason)
		}
		return &sanitize.Closed{Reason: reason}, nil
	}
	g, err := sanitize.NewGate(url, token, file, hc, logf)
	if err != nil {
		return nil, err
	}
	if logf != nil {
		src := "env"
		if file != "" {
			src = "file " + file
		}
		logf("sanitize: glovebox gate at %s, token from %s (fail closed)", url, src)
	}
	return g, nil
}

// writeSanitizeMetrics renders the gate's outcome counters. Every outcome but
// pass is a DROPPED listing, so the drop ratio and the token alerts are all
// derived from this one family.
func writeSanitizeMetrics(w io.Writer, s listing.Sanitizer) {
	var st sanitize.Stats
	switch g := s.(type) {
	case *sanitize.Gate:
		st = g.Snapshot()
	case *sanitize.Closed:
		st = sanitize.Stats{Misconfigured: g.Dropped()}
	default:
		return // Passthrough: no gate, nothing to report
	}
	fmt.Fprintf(w, "# HELP nagus_sanitize_total Listings through the glovebox sanitize gate, by outcome; every outcome but pass is a dropped listing.\n")
	fmt.Fprintf(w, "# TYPE nagus_sanitize_total counter\n")
	for _, o := range []struct {
		name string
		n    int64
	}{
		{"pass", st.Pass}, {"quarantine", st.Quarantine}, {"error", st.Error},
		{"unauthorized", st.Unauthorized}, {"tripped", st.Tripped}, {"no_token", st.NoToken},
		{"misconfigured", st.Misconfigured},
	} {
		fmt.Fprintf(w, "nagus_sanitize_total{outcome=%q} %d\n", o.name, o.n)
	}
}
