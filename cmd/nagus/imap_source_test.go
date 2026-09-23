package main

import (
	"strings"
	"testing"

	"github.com/leftathome/nagus/internal/store"
)

// No sender parser exists until one is written from a real captured email of
// that sender, so an imap source naming an unknown parser must fail at
// startup and say why -- never ingest nothing silently.
func TestIMAPSourceNeedsARealParser(t *testing.T) {
	t.Setenv("NAGUS_IMAP_HOST", "imap.forwardemail.net")
	t.Setenv("NAGUS_IMAP_USERNAME", "deals@totally.apocryph.al")
	t.Setenv("NAGUS_IMAP_PASSWORD", "x")
	src := SourceConfig{Name: "lastbottle", Category: "wine", Type: "imap",
		IMAPFrom: "offers@lastbottlewines.com", IMAPParser: "lastbottle",
		WineChannel: "retailer", Origin: "US-CA"}
	_, err := buildIngester(src, CategoryConfig{WineShipTo: "US-WA"}, store.NewMemoryStore(), categoryOpts{logf: t.Logf})
	if err == nil || !strings.Contains(err.Error(), "real captured email") {
		t.Fatalf("want a startup error naming the missing parser, got %v", err)
	}
}
