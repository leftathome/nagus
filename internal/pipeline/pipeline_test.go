package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/leftathome/nagus/internal/item"
	"github.com/leftathome/nagus/internal/listing"
)

// fakeConnector emits a fixed batch (or an error).
type fakeConnector struct {
	raws []listing.Raw
	err  error
}

func (f fakeConnector) SourceID() string { return "fake" }
func (f fakeConnector) Fetch(context.Context) ([]listing.Raw, error) {
	return f.raws, f.err
}

// fakeExtractor turns a Sanitized into an hdd item, copying a capacity aspect
// into Attributes. It errors when SourceKey is "bad" to exercise the skip path.
type fakeExtractor struct{}

func (fakeExtractor) Category() string { return "hdd" }
func (fakeExtractor) Extract(_ context.Context, s listing.Sanitized) (item.Item, error) {
	if s.SourceKey == "bad" {
		return item.Item{}, errors.New("unextractable")
	}
	attrs := map[string]string{}
	if c, ok := s.Aspects["capacity_tb"]; ok {
		attrs["capacity_tb"] = c
	}
	return item.Item{
		ID: s.SourceKey, Category: "hdd", Class: item.ClassDurable,
		Title: s.Title, PriceCents: s.PriceCents, Currency: s.Currency,
		Condition: s.ConditionRaw, SourceID: s.SourceID, SourceKey: s.SourceKey,
		SourceURL: s.SourceURL, SeenAt: s.SeenAt, Attributes: attrs,
	}, nil
}

func raw(key, title string, cents int64, capTB string) listing.Raw {
	return listing.Raw{
		SourceID: "fake", SourceKey: key, Title: title, PriceCents: cents,
		Currency: "USD", ConditionRaw: "refurb",
		Aspects: map[string]string{"capacity_tb": capTB}, SeenAt: time.Unix(1000, 0),
	}
}
