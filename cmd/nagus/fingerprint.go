package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/leftathome/nagus/internal/fingerprint"
)

// runFingerprint classifies each domain on the command line (nagus-b0u) and
// prints one JSON line per domain: platform, the signals behind the call, and
// the endpoint a connector would ingest from.
func runFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	pause := fs.Duration("pause", 2*time.Second, "delay between requests (be polite to small winery hosts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: nagus fingerprint [-pause 2s] DOMAIN [DOMAIN ...]")
	}
	p := &fingerprint.Prober{Pause: *pause}
	enc := json.NewEncoder(os.Stdout)
	for _, d := range fs.Args() {
		if err := enc.Encode(p.Probe(context.Background(), d)); err != nil {
			return err
		}
	}
	return nil
}
