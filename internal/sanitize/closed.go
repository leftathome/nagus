package sanitize

import (
	"context"
	"fmt"

	"github.com/leftathome/nagus/internal/listing"
)

// Closed rejects every listing. It is what a HALF-configured gate becomes:
// the deployment asked to be gated (a URL or a token is set) but cannot be,
// so nothing new may pass unchecked. The service still starts and serves what
// it already holds -- the Deployment is Recreate, so refusing to start would be
// an outage of every surface -- and the reason is in every drop.
type Closed struct {
	Reason string
}

var _ listing.Sanitizer = Closed{}

// Sanitize always drops.
func (c Closed) Sanitize(context.Context, listing.Raw) (listing.Sanitized, error) {
	return listing.Sanitized{}, fmt.Errorf("sanitize: gate misconfigured (%s), dropping (fail closed)", c.Reason)
}
