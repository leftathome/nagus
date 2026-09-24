package main

import (
	"context"
	"strings"
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
	t.Run("url and token file", func(t *testing.T) {
		t.Setenv("NAGUS_GLOVEBOX_SANITIZE_URL", "http://glovebox:9093")
		t.Setenv("NAGUS_GLOVEBOX_TOKEN", "")
		t.Setenv("NAGUS_GLOVEBOX_TOKEN_FILE", "/run/secrets/nagus-glovebox/NAGUS_GLOVEBOX_TOKEN")
		s, err := sanitizerFromEnv(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if g, ok := s.(*sanitize.Gate); !ok || g.TokenFile == "" {
			t.Fatalf("a token file must configure the gate, got %T", s)
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
			if _, ok := s.(*sanitize.Closed); !ok {
				t.Fatalf("half-configured must be Closed, got %T", s)
			}
			if _, err := s.Sanitize(context.Background(), listing.Raw{Title: "x"}); err == nil {
				t.Fatal("half-configured must drop, never pass")
			}
		})
	}
}

// The gate's outcomes are one metric family; the alerts are derived from it.
func TestSanitizeMetrics(t *testing.T) {
	c := &sanitize.Closed{Reason: "x"}
	_, _ = c.Sanitize(context.Background(), listing.Raw{})
	_, _ = c.Sanitize(context.Background(), listing.Raw{})
	var b strings.Builder
	writeSanitizeMetrics(&b, c)
	out := b.String()
	for _, want := range []string{
		"# TYPE nagus_sanitize_total counter",
		`nagus_sanitize_total{outcome="misconfigured"} 2`,
		`nagus_sanitize_total{outcome="pass"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	writeSanitizeMetrics(&b, sanitize.Passthrough{})
	if b.Len() != 0 {
		t.Fatal("Passthrough has no gate metrics")
	}
}
