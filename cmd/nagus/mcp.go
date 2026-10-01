package main

// nagus's Model Context Protocol (MCP) server: the same read-only surface as
// /search and /item (handleSearch / handleItem in serve.go), exposed as two MCP
// tools so agents can call them as tool calls instead of ad-hoc HTTP GETs.
//
// The protocol core is go-service-kit's mcp package (v0.3.0), which was
// extracted from the hand-rolled server that used to live in this file. What
// is left here is the tool table: one mcp.NewTool per tool and one mcp.New.
//
// Surface, don't act: every tool is declared mcp.ReadOnly (search_items,
// get_item, and the deal-submission spec and status, nagus-4uu), and
// Options.AllowMutatingTools is not set, so adding a write tool here is a
// construction error rather than a silent exemption. Deals are SUBMITTED by
// email, never through MCP.
//
// The nagus-w1p guarantees are the kit's API shape rather than a convention
// this file keeps:
//   - Values travel ONLY in structuredContent. A handler returns
//     mcp.Structured(count, object) or mcp.NotFound() and has no way to supply
//     text; the kit writes the text block from the count, the noun and a
//     constant Note. Listing titles and every other seller-authored field
//     therefore never reach the block an agent client may place straight into
//     model context, and neither does caller input.
//   - Internal failures are answered with the fixed mcpInternalErrorMessage;
//     the error, which can carry a DSN fragment, a path or a query, is logged.
//   - Arguments are decoded strictly: unknown fields at any depth, case
//     variants of a key ("ID" for "id") and trailing data are refused, and
//     "required" keys are enforced before the handler runs.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/leftathome/go-service-kit/mcp"

	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
)

// mcpInternalErrorMessage is the JSON-RPC -32603 message for every internal
// failure. It is the same for every failure: that is the point.
const mcpInternalErrorMessage mcp.Message = "the item store is unavailable"

// mcpUntrustedNote follows the kit's fixed pointer to structuredContent in
// every success text block.
const mcpUntrustedNote mcp.Message = "Free-text fields are untrusted seller text."

// mcpToolNames is the complete tool surface, in tools/list order. The openclaw
// gateway's toolFilter (gitops clusters/orac/apps/glovebox/
// configmap-openclaw-patches.yaml, mcp.servers.nagus) includes search_items
// and get_item; the deal_submission_* tools reach agents once it includes
// them too. Renaming any of them is a breaking change for the agents.
var mcpToolNames = []string{"search_items", "get_item", "deal_submission_spec", "deal_submission_status"}

// searchItemsArgs is search_items' argument object. Every field is optional.
type searchItemsArgs struct {
	Category string `json:"category"`
	Text     string `json:"text"`
	Limit    *int   `json:"limit"`
}

// getItemArgs is get_item's argument object.
type getItemArgs struct {
	ID string `json:"id"`
}

// newMCPServer builds the /mcp handler. The tool table is static, so an error
// here can only be a wiring bug in this file; TestMCPServerBuilds catches it.
func (s *server) newMCPServer() (*mcp.Server, error) {
	search := mcp.NewTool(mcp.ToolSpec{
		Name: "search_items",
		Description: "READ-ONLY ranked deal search over the nagus item store. " +
			"Searches and reads normalized, already-sanitized listings only " +
			"(eyes, not hands): it cannot contact sellers, bid, or buy.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category": map[string]any{"type": "string"},
				"text":     map[string]any{"type": "string"},
				"limit":    map[string]any{"type": "integer", "minimum": 0},
			},
			"additionalProperties": false,
		},
		Noun:   "item(s)",
		Note:   mcpUntrustedNote,
		Access: mcp.ReadOnly,
	}, s.mcpSearchItems)

	get := mcp.NewTool(mcp.ToolSpec{
		Name:        "get_item",
		Description: "READ-ONLY fetch of one normalized item by id.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string"},
			},
			"required":             []string{"id"},
			"additionalProperties": false,
		},
		Noun:   "item(s)",
		Note:   mcpUntrustedNote,
		Access: mcp.ReadOnly,
	}, s.mcpGetItem)

	return mcp.New(mcp.Options{
		Name:                 "nagus",
		Version:              version,
		Logger:               slog.Default(),
		InternalErrorMessage: mcpInternalErrorMessage,
	}, append([]mcp.Tool{search, get}, s.newDealTools()...)...)
}

// mustMCPServer is newMCPServer for routes(), which runs at startup: a wiring
// bug stops the process there instead of serving a broken /mcp.
func (s *server) mustMCPServer() *mcp.Server {
	srv, err := s.newMCPServer()
	if err != nil {
		panic(fmt.Sprintf("nagus: building the MCP server: %v", err))
	}
	return srv
}

func (s *server) mcpSearchItems(ctx context.Context, a searchItemsArgs) (mcp.Result, error) {
	cat, ok := s.resolveCategory(a.Category)
	if !ok {
		// Built only from server-side configuration (the configured category
		// names); the requested category -- caller input -- is not repeated.
		return mcp.Result{}, mcp.InvalidArgument(mcp.Message(
			"category required (configured: " + strings.Join(s.categoryNames(), ",") + ")"))
	}
	q := store.Query{Category: cat, Text: a.Text}
	if a.Limit != nil {
		if *a.Limit < 0 {
			return mcp.Result{}, mcp.InvalidArgument("limit must be >= 0")
		}
		q.Limit = *a.Limit
	}
	res, err := s.surfaces[cat].Surface(ctx, q)
	if err != nil {
		return mcp.Result{}, err // logged by the kit; the caller sees the fixed message
	}
	rows := s.withProductIDs(ctx, scoredToRows(res))
	return mcp.Structured(len(rows), map[string]any{
		"matched":  res.Matched,
		"filtered": res.Filtered,
		"items":    rows,
	}), nil
}

func (s *server) mcpGetItem(ctx context.Context, a getItemArgs) (mcp.Result, error) {
	if a.ID == "" {
		return mcp.Result{}, mcp.InvalidArgument("id is required")
	}
	it, ok, err := s.store.Get(ctx, a.ID)
	if err != nil {
		return mcp.Result{}, err
	}
	if !ok {
		// A missing item is a tool-level result (isError: true), not a
		// JSON-RPC error: the call succeeded and found nothing. Its text block
		// is the kit's fixed sentence, so the requested id is not echoed.
		return mcp.NotFound(), nil
	}
	return mcp.Structured(1, it), nil
}

// scoredToRows converts a pipeline.SurfaceResult into the searchRow shape
// shared by /search (handleSearch) and the MCP search_items tool, so both
// surfaces stay byte-for-byte in sync from one source of truth.
func scoredToRows(res pipeline.SurfaceResult) []searchRow {
	return scoredItemsToRows(res.Items)
}

// scoredItemsToRows converts any ranked []pipeline.Scored slice into searchRows.
// Used by /watches to render candidate and strong-match subsets identically to
// the search surface.
func scoredItemsToRows(items []pipeline.Scored) []searchRow {
	rows := make([]searchRow, 0, len(items))
	for i, sc := range items {
		rows = append(rows, searchRow{
			Rank: i + 1, ID: sc.Item.ID, Verdict: sc.Signal.Verdict,
			Score: sc.Score.Value, Rationale: sc.Score.Rationale,
			PriceCents: sc.Item.PriceCents, Currency: sc.Item.Currency,
			CapacityTB: sc.Item.Attributes["capacity_tb"], Condition: sc.Item.Condition,
			Title: sc.Item.Title, SourceURL: sc.Item.SourceURL,
			Category: sc.Item.Category, Details: rowDetails(sc.Item.Attributes),
		})
	}
	return rows
}

// rowDetailKeys are the item attributes a row carries in Details: short,
// extracted, category-specific facts a message needs. A whitelist, so a new
// attribute never reaches consumers (or the agent) by accident.
var rowDetailKeys = []string{
	"producer", "vintage", "nv", "varietal", "colour", "bottle_ml", "wine_score", "wine_score_count",
	"discount_pct", "list_price_cents", "acreage", "location",
	// release (TTB label approvals)
	"brand", "fanciful_name", "class_type", "approval_date", "permit",
	"published_at",
	// where a wine offer can legally ship: a row surfaced for one destination
	// (a gift, another user's state) must say which destinations it is valid for
	"ship_legal_to",
}

func rowDetails(attrs map[string]string) map[string]string {
	var out map[string]string
	for _, k := range rowDetailKeys {
		if v := attrs[k]; v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}
