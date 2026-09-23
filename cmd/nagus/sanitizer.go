package main

import (
	"fmt"
	"net/http"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
)

// sanitizerFromEnv builds the glovebox sanitize gate from
// NAGUS_GLOVEBOX_SANITIZE_URL and NAGUS_GLOVEBOX_TOKEN (nagus-9ib).
//
// Neither set: nil, and every category keeps the in-process Passthrough --
// the behavior before the gate existed. Both set: the gate, fail closed. Only
// one set is a startup ERROR, never a silent fallback: a deployment that means
// to be gated must not quietly run ungated because its token failed to sync.
func sanitizerFromEnv(hc *http.Client, logf func(string, ...any)) (listing.Sanitizer, error) {
	url := envOr("NAGUS_GLOVEBOX_SANITIZE_URL", "")
	token := envOr("NAGUS_GLOVEBOX_TOKEN", "")
	switch {
	case url == "" && token == "":
		return nil, nil
	case url == "" || token == "":
		return nil, fmt.Errorf("glovebox sanitize gate half-configured: set both NAGUS_GLOVEBOX_SANITIZE_URL and NAGUS_GLOVEBOX_TOKEN, or neither")
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
