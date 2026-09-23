package main

import (
	"context"
	"testing"

	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/sanitize"
)

// The gate is dual-mode: unconfigured keeps Passthrough; fully configured is
// the gate; half-configured starts but drops every new listing -- never a
// silent ungated run, and never a pod that refuses to start (Recreate).
func TestSanitizerFromEnv(t *testing.T) {
	t.Run("neither", func(t *testing.T) {
		t.Setenv("NAGUS_GLOVEBOX_SANITIZE_URL", "")
		t.Setenv("NAGUS_GLOVEBOX_TOKEN", "")
		s, err := sanitizerFromEnv(nil, nil)
		if err != nil || s != nil {
			t.Fatalf("unconfigured must keep Passthrough (nil): %v %v", s, err)
		}
	})
	t.Run("both", func(t *testing.T) {
		t.Setenv("NAGUS_GLOVEBOX_SANITIZE_URL", "http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9093")
		t.Setenv("NAGUS_GLOVEBOX_TOKEN", "tok")
		s, err := sanitizerFromEnv(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := s.(*sanitize.Gate); !ok {
			t.Fatalf("configured must be the glovebox gate, got %T", s)
		}
	})
	for _, tc := range []struct{ name, url, token string }{
		{"url-only", "http://glovebox:9093", ""},
		{"token-only", "", "tok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NAGUS_GLOVEBOX_SANITIZE_URL", tc.url)
			t.Setenv("NAGUS_GLOVEBOX_TOKEN", tc.token)
			s, err := sanitizerFromEnv(nil, nil)
			if err != nil {
				t.Fatalf("half-configured must still start: %v", err)
			}
			if _, ok := s.(sanitize.Closed); !ok {
				t.Fatalf("half-configured must be Closed, got %T", s)
			}
			if _, err := s.Sanitize(context.Background(), listing.Raw{Title: "x"}); err == nil {
				t.Fatal("half-configured must drop, never pass")
			}
		})
	}
}
