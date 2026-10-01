package main

// Deal submission by email (nagus.deal/v1, nagus-4uu; design:
// docs/design/2026-09-26-deal-submission-jsonl.md). The household's humans
// and agents email one JSON deal per line to the deals mailbox; each enabled
// category is one imap source with imapParser "deal-jsonl-v1", and all of them
// share one deal.Hub (the category routing, the principals and the status
// ledger). This file wires the sources, the schema route and the two
// read-only MCP tools.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/leftathome/go-service-kit/mcp"

	"github.com/leftathome/nagus/internal/connector/imapmail"
	"github.com/leftathome/nagus/internal/deal"
	"github.com/leftathome/nagus/internal/listing"
	"github.com/leftathome/nagus/internal/offer"
)

// isDealSource reports whether a source reads nagus.deal/v1 submissions.
func isDealSource(s SourceConfig) bool {
	return s.Type == "imap" && s.IMAPParser == deal.ParserName
}

// lowerSet is a sorted, lower-cased, de-duplicated copy of addresses, for
// comparing allowlists.
func lowerSet(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}

func aliasKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		keys = append(keys, strings.ToLower(strings.TrimSpace(k))+"="+strings.ToLower(strings.TrimSpace(v)))
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

// validateDealSource checks one deal source's own declaration.
func validateDealSource(s SourceConfig) error {
	switch {
	case s.Category != deal.CategoryWine && s.Category != deal.CategoryHDD:
		return fmt.Errorf("source %q: a %s source needs category wine or hdd (one source per category)", s.Name, deal.ParserName)
	case s.IMAPFrom != "":
		return fmt.Errorf("source %q: a %s source takes its senders from imapSenders, not imapFrom", s.Name, deal.ParserName)
	case len(lowerSet(s.IMAPSenders)) == 0:
		return fmt.Errorf("source %q: a %s source needs imapSenders (the allowlist, configured in gitops)", s.Name, deal.ParserName)
	case s.WineProducer != "" || s.ProducerFromBody:
		// Every line names its own producer (brand); a source-wide one would
		// overwrite it.
		return fmt.Errorf("source %q: a %s source takes each deal's producer from the deal; drop wineProducer/producerFromBody", s.Name, deal.ParserName)
	case s.Fixture != "":
		return fmt.Errorf("source %q: a %s source has no fixture mode", s.Name, deal.ParserName)
	case s.IMAPTrustAuthResults:
		// Only nagus's own DKIM verification may admit a deal (rv35 C1).
		return fmt.Errorf("source %q: a %s source never trusts Authentication-Results headers; drop imapTrustAuthResults", s.Name, deal.ParserName)
	}
	// A forward is credited to the forwarder (rv35 N1), so forwarders are
	// principals too and may have aliases.
	principals := append(lowerSet(s.IMAPSenders), lowerSet(s.IMAPForwarders)...)
	seenAlias := map[string]bool{}
	for addr, alias := range s.IMAPSenderAliases {
		if !slices.Contains(principals, strings.ToLower(strings.TrimSpace(addr))) {
			return fmt.Errorf("source %q: imapSenderAliases names %q, which is not in imapSenders or imapForwarders", s.Name, addr)
		}
		a := strings.ToLower(strings.TrimSpace(alias))
		if seenAlias[a] {
			return fmt.Errorf("source %q: alias %q is used twice", s.Name, alias)
		}
		seenAlias[a] = true
		if strings.TrimSpace(alias) == "" || strings.Contains(alias, "@") {
			return fmt.Errorf("source %q: the alias for %q must be a short name, not empty or an address", s.Name, addr)
		}
	}
	return nil
}

// buildDealHub builds the one Hub every deal source shares, or nil when no
// source reads deal submissions. Deal sources read the SAME messages and
// route each line to its category's source, so they must agree on who may
// submit and where: otherwise a line routed to a source that never reads its
// message would stay pending forever.
func buildDealHub(sources []SourceConfig, now func() time.Time) (*deal.Hub, error) {
	var first *SourceConfig
	routes := map[string]string{}
	mailbox := ""
	for i := range sources {
		s := sources[i]
		if !isDealSource(s) {
			continue
		}
		if err := validateDealSource(s); err != nil {
			return nil, err
		}
		if prev, dup := routes[s.Category]; dup {
			return nil, fmt.Errorf("source %q: category %s already has a deal source (%s); one per category", s.Name, s.Category, prev)
		}
		routes[s.Category] = imapmail.SourceID + ":" + s.Name
		if first == nil {
			first = &sources[i]
		} else if !slices.Equal(lowerSet(s.IMAPSenders), lowerSet(first.IMAPSenders)) ||
			!slices.Equal(lowerSet(s.IMAPForwarders), lowerSet(first.IMAPForwarders)) ||
			aliasKey(s.IMAPSenderAliases) != aliasKey(first.IMAPSenderAliases) ||
			s.IMAPMailbox != first.IMAPMailbox || s.IMAPLookbackDays != first.IMAPLookbackDays {
			return nil, fmt.Errorf("source %q: every %s source must have the same imapSenders, imapForwarders, imapSenderAliases, imapMailbox and imapLookbackDays as %q",
				s.Name, deal.ParserName, first.Name)
		}
		if s.DealSubmitTo != "" {
			if mailbox != "" && !strings.EqualFold(mailbox, s.DealSubmitTo) {
				return nil, fmt.Errorf("source %q: dealSubmitTo differs between deal sources", s.Name)
			}
			mailbox = s.DealSubmitTo
		}
	}
	if first == nil {
		return nil, nil
	}
	if mailbox == "" {
		mailbox = envOr("NAGUS_IMAP_USERNAME", "")
	}
	return deal.NewHub(routes, first.IMAPSenderAliases, strings.TrimSpace(mailbox), first.IMAPLookbackDays, now), nil
}

// buildDealConnector builds a deal source's imap connector: the sender
// allowlist, ignored mail counted, and the fetch boundary the ledger needs.
func buildDealConnector(s SourceConfig, o categoryOpts) (listing.Connector, error) {
	hub := o.deals
	if hub == nil {
		// A single source built outside serve (tests, ingest): its own hub.
		var err error
		if hub, err = buildDealHub([]SourceConfig{s}, nil); err != nil {
			return nil, err
		}
	} else if err := validateDealSource(s); err != nil {
		return nil, err
	}
	p, err := deal.NewParser(hub, s.Category)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", s.Name, err)
	}
	conn, err := imapmail.NewConnector(imapmail.Config{
		Name: s.Name, Senders: s.IMAPSenders, Forwarders: s.IMAPForwarders, Mailbox: s.IMAPMailbox,
		LookbackDays: s.IMAPLookbackDays, Parser: p, CountIgnored: true, Observe: hub.Observer(), Logf: o.logf,
		Host: envOr("NAGUS_IMAP_HOST", ""), Port: envOr("NAGUS_IMAP_PORT", ""), TLS: envOr("NAGUS_IMAP_TLS", ""),
		Username: envOr("NAGUS_IMAP_USERNAME", ""), Password: envOr("NAGUS_IMAP_PASSWORD", ""),
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", s.Name, err)
	}
	return &deal.Source{Inner: conn, Hub: hub}, nil
}

// dealHubOf returns the hub a deal connector reports to.
func dealHubOf(c listing.Connector) *deal.Hub {
	if ds, ok := c.(*deal.Source); ok {
		return ds.Hub
	}
	return nil
}

// handleDealSchema serves the committed nagus.deal/v1 JSON Schema, read-only.
func handleDealSchema(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(deal.SchemaFile)
}

// --- MCP ----------------------------------------------------------------------

// dealSpecArgs is deal_submission_spec's (empty) argument object.
type dealSpecArgs struct{}

// dealStatusArgs is deal_submission_status's argument object: exactly one of
// message_id and principal.
type dealStatusArgs struct {
	MessageID string `json:"message_id"`
	Principal string `json:"principal"`
	Limit     *int   `json:"limit"`
}

const (
	dealStatusDefaultLimit = 5
	dealStatusMaxLimit     = 20
)

var dealWhenToUse = []string{
	"A deal nagus cannot see for itself: an in-store or tasting-room price, a price a person saw or was told about.",
	"A retailer newsletter or flyer whose products are images only, so nagus's own mail parsers find no text.",
	"A tip: a deal someone mentioned, with a link.",
}

var dealWhenNotToUse = []string{
	"Items already on a store nagus watches: nagus polls those itself (search_items shows them); submitting them only adds a duplicate.",
	"Anything that is not a wine or hard-drive offer with a price and an https link.",
	"Private or personal information: the note field is stored and is visible to other household agents.",
}

func (s *server) newDealTools() []mcp.Tool {
	spec := mcp.NewTool(mcp.ToolSpec{
		Name: "deal_submission_spec",
		Description: "READ-ONLY. How to submit a deal nagus cannot see itself (in-store, image-only newsletter, a tip) by email: " +
			"the nagus.deal/v1 JSON Schema, one wine and one hdd example, the mailbox address, when to use it and when not to, " +
			"and the limits. Call this before sending; then check deal_submission_status.",
		Noun:   "spec",
		Access: mcp.ReadOnly,
	}, s.mcpDealSpec)

	status := mcp.NewTool(mcp.ToolSpec{
		Name: "deal_submission_status",
		Description: "READ-ONLY per-line results for a deal submission email: accepted (with offer_id, usable with get_item, and quark's product_id once resolved), " +
			"rejected (a reason code), or pending. Give message_id (the Message-ID you sent) for full detail, OR principal (your sender NAME, never an address) " +
			"for your latest messages' outcomes and reason codes only. Never returns line content.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message_id": map[string]any{"type": "string", "description": "The submission's Message-ID, with or without angle brackets."},
				"principal":  map[string]any{"type": "string", "description": "Your sender name (alias, not an address); returns outcomes and reason codes only."},
				"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": dealStatusMaxLimit},
			},
			"additionalProperties": false,
		},
		Noun:   "message(s)",
		Access: mcp.ReadOnly,
	}, s.mcpDealStatus)
	return []mcp.Tool{spec, status}
}

func (s *server) mcpDealSpec(_ context.Context, _ dealSpecArgs) (mcp.Result, error) {
	enabled := s.deals != nil
	mailbox := ""
	categories := []string{}
	lookback := imapmail.DefaultLookbackDays
	if enabled {
		mailbox = s.deals.Mailbox
		categories = s.deals.EnabledCategories()
		lookback = s.deals.LookbackDays
	}
	reasons := map[string]string{}
	for _, lr := range deal.LineReasons {
		reasons[string(lr.Code)] = lr.Meaning
	}
	return mcp.Structured(1, map[string]any{
		"schema_id":          deal.SchemaID,
		"schema_url":         deal.SchemaPath,
		"schema":             deal.SchemaObject(),
		"enabled":            enabled && mailbox != "",
		"mailbox":            mailbox,
		"enabled_categories": categories,
		"format": "Send a NEW plain-text email (text/plain; HTML-only mail is refused) to the mailbox from your allowlisted address. " +
			"Never send a reply: a message with In-Reply-To or References is refused whole (reply_not_accepted). " +
			"Put ONE JSON object per line; every line that starts with { is read as a deal, everything else is ignored. " +
			"Do not hard-wrap lines. Reading stops at a signature (-- ), a forwarded or original-message marker, " +
			"an Outlook reply header, or an 'On ... wrote:' line. " +
			"Each line is judged on its own. A resent message is a new submission.",
		"examples":        deal.Examples(),
		"example_body":    deal.ExampleBody,
		"when_to_use":     dealWhenToUse,
		"when_not_to_use": dealWhenNotToUse,
		"limits": map[string]any{
			"max_deal_lines_per_message": deal.MaxLinesPerMessage,
			"max_line_bytes":             deal.MaxLineBytes,
			"max_body_bytes_scanned":     deal.MaxScanBytes,
			"max_url_chars":              deal.MaxURLLen,
			"max_message_bytes":          imapmail.DefaultMaxBytes,
			"status_kept_days":           lookback,
			"processed_within":           "the next poll of the deal sources (their configured interval)",
		},
		"status_tool":      "deal_submission_status",
		"reason_codes":     reasons,
		"message_outcomes": deal.MessageOutcomeMeanings,
	}), nil
}

func (s *server) mcpDealStatus(ctx context.Context, a dealStatusArgs) (mcp.Result, error) {
	if s.deals == nil {
		return mcp.Result{}, mcp.InvalidArgument("deal submission is not enabled on this nagus")
	}
	byID, byPrincipal := strings.TrimSpace(a.MessageID) != "", strings.TrimSpace(a.Principal) != ""
	if byID == byPrincipal {
		return mcp.Result{}, mcp.InvalidArgument("give exactly one of message_id and principal")
	}
	limit := dealStatusDefaultLimit
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > dealStatusMaxLimit {
			return mcp.Result{}, mcp.InvalidArgument("limit must be between 1 and 20")
		}
		limit = *a.Limit
	}
	if byPrincipal {
		msgs := s.deals.Recent(a.Principal, limit)
		if msgs == nil {
			msgs = []deal.MessageView{}
		}
		return mcp.Structured(len(msgs), map[string]any{"messages": msgs}), nil
	}
	// Every verified sender's message with that id: ids are sender-chosen,
	// so two allowlisted senders may share one, and each keeps its own entry
	// (rv35 I5). Unknown and unverified ids both answer not-found (N3).
	msgs := s.deals.Ledger.LookupAll(a.MessageID)
	if len(msgs) == 0 {
		return mcp.NotFound(), nil
	}
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}
	for i := range msgs {
		s.withResolutions(ctx, msgs[i].Lines)
	}
	return mcp.Structured(len(msgs), map[string]any{"messages": msgs}), nil
}

// withResolutions stamps quark's answer onto accepted lines from the offer
// store: the product id once resolved. Enrichment only; a read failure leaves
// the line as it is.
func (s *server) withResolutions(ctx context.Context, lines []deal.LineView) {
	if s.offers == nil {
		return
	}
	for i := range lines {
		if lines[i].OfferID == "" {
			continue
		}
		o, ok, err := s.offers.Get(ctx, lines[i].OfferID)
		if err != nil || !ok {
			continue
		}
		lines[i].Resolution = string(o.Resolution.State)
		if o.Resolution.State == offer.ResolutionResolved {
			lines[i].ProductID = o.Resolution.ProductID
		}
	}
}
