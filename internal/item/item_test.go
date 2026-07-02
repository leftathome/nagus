package item

import (
	"errors"
	"testing"
	"time"
)

func validItem() Item {
	return Item{
		ID:         "craigslist:reo:12345",
		Category:   "land",
		Class:      ClassDurable,
		Title:      "5 acres with old cabin",
		PriceCents: 4500000,
		Currency:   "USD",
		SourceID:   "craigslist",
		SourceKey:  "reo/12345",
		SourceURL:  "https://example.org/12345",
		SeenAt:     time.Unix(1_700_000_000, 0),
		Attributes: map[string]string{"acreage": "5"},
	}
}

func TestValidateAcceptsWellFormedItem(t *testing.T) {
	if err := validItem().Validate(); err != nil {
		t.Fatalf("valid item rejected: %v", err)
	}
}

func TestValidateRejectsMissingProvenance(t *testing.T) {
	cases := map[string]func(*Item){
		"no id":         func(i *Item) { i.ID = "" },
		"no category":   func(i *Item) { i.Category = "" },
		"no source_id":  func(i *Item) { i.SourceID = "" },
		"no source_key": func(i *Item) { i.SourceKey = "" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			it := validItem()
			mut(&it)
			if err := it.Validate(); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

func TestValidateRejectsBadClassAndNegativePrice(t *testing.T) {
	it := validItem()
	it.Class = "widget"
	if err := it.Validate(); err == nil {
		t.Fatal("expected error for invalid class")
	}
	it = validItem()
	it.PriceCents = -1
	var fe *FieldError
	err := it.Validate()
	if !errors.As(err, &fe) || fe.Field != "price_cents" {
		t.Fatalf("expected FieldError on price_cents, got %v", err)
	}
}
