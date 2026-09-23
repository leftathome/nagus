package category

import (
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
)

// sanitizerOr is the injected trust boundary, or the in-process Passthrough
// when none is configured. Dual-mode on purpose: deploying the glovebox gate
// changes nothing until its URL and token are provisioned, and a deployment
// without glovebox keeps working exactly as before (nagus-9ib).
func sanitizerOr(s listing.Sanitizer, category string) listing.Sanitizer {
	if s != nil {
		return s
	}
	return sanitize.Passthrough{Name: "sanitize.passthrough(" + category + ")"}
}
