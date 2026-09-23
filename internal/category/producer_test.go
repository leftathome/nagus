package category

import (
	"testing"

	"github.com/leftathome/nagus/internal/listing"
)

// A retailer sells many producers, so one declared producer cannot serve --
// but stores that publish it in a structured description can be read, per
// source. Real Bottle Barn descriptions (2026-09-22).
func TestProducerFromBody(t *testing.T) {
	for body, want := range map[string]string{
		"Producer: Domaine Bouchard Region: Volnay , Cote de Beaune , Burgundy , France Varietal: Pinot Noir": "Domaine Bouchard",
		"Producer: Ca' La Bionda Region: Valpolicella , Veneto , Italy Blend: 70% Corvina":                    "Ca' La Bionda",
		"Producer: Freire Lobo Region: Dao, Portugal":                                                         "Freire Lobo",
		"producer: Domaine Tempier":                      "Domaine Tempier",
		"A lovely wine from a great producer in Bandol.": "",
		"": "",
	} {
		if got := producerFromBody(body); got != want {
			t.Errorf("producerFromBody(%.40q) = %q, want %q", body, got, want)
		}
	}
}

func TestProducerFromBodyIsPerSourceOptIn(t *testing.T) {
	raw := listing.Raw{Title: "2024 Domaine Tempier Bandol", Body: "Producer: Domaine Tempier Region: Bandol"}
	if got := (&channelTagger{}).producerFor(raw); got != "" {
		t.Fatalf("without the opt-in the body must not be read: %q", got)
	}
	if got := (&channelTagger{producerFromBody: true}).producerFor(raw); got != "Domaine Tempier" {
		t.Fatalf("with the opt-in: %q", got)
	}
	if got := (&channelTagger{producer: "Declared", producerFromBody: true}).producerFor(raw); got != "Declared" {
		t.Fatalf("declared producer must win: %q", got)
	}
}
