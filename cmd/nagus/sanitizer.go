package main

import (
	"net/http"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
)

// sanitizerFromEnv builds the glovebox sanitize gate from
// NAGUS_GLOVEBOX_SANITIZE_URL and NAGUS_GLOVEBOX_TOKEN (nagus-9ib).
//
//   - Neither set: nil, and every category keeps the in-process Passthrough --
//     the behavior before the gate existed.
//   - Both set: the gate, fail closed.
//   - Only one set: a deployment that asked to be gated but cannot be. It is
//     NEVER allowed to run ungated, and it does not refuse to start either:
//     the Deployment is Recreate, so a pod that will not start is an outage
//     of every surface. It starts, serves what it already holds, and drops
//     every NEW listing (sanitize.Closed) -- the same outcome as glovebox
//     rejecting the token -- with the reason logged at startup and in each
//     drop. The token's env ref is optional in the chart for the same reason.
func sanitizerFromEnv(hc *http.Client, logf func(string, ...any)) (listing.Sanitizer, error) {
	url := envOr("NAGUS_GLOVEBOX_SANITIZE_URL", "")
	token := envOr("NAGUS_GLOVEBOX_TOKEN", "")
	switch {
	case url == "" && token == "":
		return nil, nil
	case url == "" || token == "":
		reason := "NAGUS_GLOVEBOX_TOKEN is empty (has its secret synced?)"
		if url == "" {
			reason = "NAGUS_GLOVEBOX_SANITIZE_URL is empty"
		}
		if logf != nil {
			logf("sanitize: GATE HALF-CONFIGURED -- %s; every new listing will be DROPPED (fail closed) until both are set", reason)
		}
		return sanitize.Closed{Reason: reason}, nil
	}
	g, err := sanitize.NewGate(url, token, hc, logf)
	if err != nil {
		return nil, err
	}
	if logf != nil {
		logf("sanitize: glovebox gate at %s (fail closed)", url)
	}
	return g, nil
}
