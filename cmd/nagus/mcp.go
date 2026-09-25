package main

// Minimal, hand-rolled Model Context Protocol (MCP) server over HTTP, using
// JSON-RPC 2.0 as its wire format (https://www.jsonrpc.org/specification).
// This is the same read-only surface as /search and /item (handleSearch /
// handleItem in serve.go), exposed as two MCP tools so agents can call them
// as tool calls instead of ad-hoc HTTP GETs. It is stdlib-only: no MCP SDK,
// no new dependency.
//
// Surface, don't act: only search_items and get_item are exposed, both
// read-only over the store. There is no mutating tool.
//
// Untrusted text stays out of the text block (nagus-w1p). Listing titles and
// other fields are seller-authored, and an agent client may place a text
// content block straight into model context -- where an instruction injected
// into a title does the most harm. So values travel ONLY in structuredContent;
// the text block says how many results there are and where the data is, and
// never echoes caller input. Internal failures return a fixed message (a store
// error can carry a DSN fragment, a path or a query) and log the detail.
// Arguments are decoded strictly, so inputSchema's additionalProperties:false
// is what the server actually enforces.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/leftathome/nagus/internal/pipeline"
	"github.com/leftathome/nagus/internal/store"
)

// mcpProtocolVersion is the fallback MCP protocol version advertised by
// initialize when the client did not send one.
const mcpProtocolVersion = "2025-06-18"

// JSON-RPC 2.0 standard error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

// nullID is used as the "id" of a JSON-RPC error response when the request's
// id could not be determined (e.g. a parse error before the body was read).
var nullID = json.RawMessage("null")

// rpcError is the JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// rpcResponse is the JSON-RPC 2.0 response envelope. ID is a json.RawMessage
// so it round-trips whatever type (string, number, null) the request used;
// json.RawMessage(nil) marshals to the JSON literal null.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpServerInfo identifies this server in the initialize result.
type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// mcpToolDescriptor is one entry of the tools/list result.
type mcpToolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpToolContent is one content block of a tools/call result.
type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpToolResult is the MCP tool call result shape: human-readable content
// plus a structured, machine-usable mirror of the same data. isError is a
// TOOL-LEVEL error (e.g. "no such item"), distinct from a JSON-RPC error
// (which signals a protocol-level problem with the call itself).
type mcpToolResult struct {
	Content           []mcpToolContent `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError"`
}

// handleMCP is the single POST /mcp endpoint. It accepts one JSON-RPC 2.0
// request object per call (not a batch) and dispatches to the methods
// implemented below, reusing the same pipeline + store as /search and /item.
func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeRPCError(w, nullID, rpcParseError, "parse error: "+err.Error())
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeRPCError(w, nullID, rpcParseError, "parse error: empty body")
		return
	}
	if trimmed[0] == '[' {
		// Batch requests are out of scope for v1; reject with a clear error
		// rather than silently mishandling them.
		writeRPCError(w, nullID, rpcInvalidRequest, "invalid request: batch requests are not supported")
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		writeRPCError(w, nullID, rpcParseError, "parse error: "+err.Error())
		return
	}

	// The id member's presence (not its value) distinguishes a request from a
	// notification per JSON-RPC 2.0: a Notification is a Request without an
	// "id" member, even if the member would have been null.
	idRaw, hasID := raw["id"]
	id := nullID
	if hasID {
		id = idRaw
	}

	var method string
	if mRaw, ok := raw["method"]; ok {
		if err := json.Unmarshal(mRaw, &method); err != nil || method == "" {
			writeRPCError(w, id, rpcInvalidRequest, "invalid request: method must be a non-empty string")
			return
		}
	} else {
		writeRPCError(w, id, rpcInvalidRequest, "invalid request: missing method")
		return
	}

	var params json.RawMessage
	if p, ok := raw["params"]; ok {
		params = p
	}

	result, rpcErr := s.dispatchMCP(r.Context(), method, params)

	if !hasID {
		// A notification never gets a response body, success or error.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := rpcResponse{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	writeJSON(w, resp)
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

// dispatchMCP routes one JSON-RPC method to its handler. It returns either a
// result (marshaled into the response's "result") or an rpcError (marshaled
// into "error"), never both.
func (s *server) dispatchMCP(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.mcpInitialize(params), nil
	case "notifications/initialized":
		// This is a notification in normal use (handleMCP already skips the
		// response body when there is no id); handle it gracefully even if a
		// caller mistakenly sends it as a request expecting a reply.
		return map[string]any{}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpToolDescriptors()}, nil
	case "tools/call":
		return s.mcpToolsCall(ctx, params)
	default:
		return nil, &rpcError{Code: rpcMethodNotFound, Message: fmt.Sprintf("method not found: %s", method)}
	}
}

func (s *server) mcpInitialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p) // best-effort; fall back to default below
	}
	protocolVersion := p.ProtocolVersion
	if protocolVersion == "" {
		protocolVersion = mcpProtocolVersion
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": mcpServerInfo{Name: "nagus", Version: version},
	}
}

// mcpToolDescriptors is the read-only tool catalog: search_items and
// get_item. There is no mutating tool -- surface, don't act.
func mcpToolDescriptors() []mcpToolDescriptor {
	return []mcpToolDescriptor{
		{
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
		},
		{
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
		},
	}
}

func (s *server) mcpToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid params: " + err.Error()}
	}
	switch p.Name {
	case "search_items":
		return s.mcpSearchItems(ctx, p.Arguments)
	case "get_item":
		return s.mcpGetItem(ctx, p.Arguments)
	default:
		return nil, &rpcError{Code: rpcInvalidParams, Message: fmt.Sprintf("unknown tool: %s", p.Name)}
	}
}

func (s *server) mcpSearchItems(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var a struct {
		Category string `json:"category"`
		Text     string `json:"text"`
		Limit    *int   `json:"limit"`
	}
	if len(args) > 0 {
		if err := strictDecode(args, &a); err != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid arguments: unknown or malformed field"}
		}
	}
	cat, ok := s.resolveCategory(a.Category)
	if !ok {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid arguments: category required (configured: " + strings.Join(s.categoryNames(), ",") + ")"}
	}
	q := store.Query{Category: cat, Text: a.Text}
	if a.Limit != nil {
		if *a.Limit < 0 {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid arguments: limit must be >= 0"}
		}
		q.Limit = *a.Limit
	}

	res, err := s.surfaces[cat].Surface(ctx, q)
	if err != nil {
		return nil, mcpInternal("search_items", err)
	}
	rows := s.withProductIDs(ctx, scoredToRows(res))
	return mcpToolResult{
		Content: []mcpToolContent{{Type: "text", Text: fmt.Sprintf(
			"%d item(s). The data is in structuredContent; free-text fields are untrusted seller text.", len(rows))}},
		StructuredContent: map[string]any{
			"matched":  res.Matched,
			"filtered": res.Filtered,
			"items":    rows,
		},
		IsError: false,
	}, nil
}

func (s *server) mcpGetItem(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var a struct {
		ID string `json:"id"`
	}
	if err := strictDecode(args, &a); err != nil || a.ID == "" {
		return nil, &rpcError{Code: rpcInvalidParams, Message: "invalid arguments: id is required"}
	}
	it, ok, err := s.store.Get(ctx, a.ID)
	if err != nil {
		return nil, mcpInternal("get_item", err)
	}
	if !ok {
		// A missing item is a tool-level result, not a JSON-RPC error: the
		// call itself succeeded, it just found nothing. The requested id is NOT
		// echoed into the text block: it is caller input.
		return mcpToolResult{
			Content: []mcpToolContent{{Type: "text", Text: "No item with that id."}},
			IsError: true,
		}, nil
	}
	return mcpToolResult{
		Content:           []mcpToolContent{{Type: "text", Text: "1 item. The data is in structuredContent; free-text fields are untrusted seller text."}},
		StructuredContent: it,
		IsError:           false,
	}, nil
}

// mcpInternal logs an internal failure and returns a FIXED message: the error
// itself can carry a DSN fragment, a file path or a query.
func mcpInternal(tool string, err error) *rpcError {
	fmt.Fprintf(os.Stderr, "  mcp %s: internal error: %v\n", tool, err)
	return &rpcError{Code: rpcInternalError, Message: "the item store is unavailable"}
}

// strictDecode rejects unknown fields, matching the inputSchema's
// additionalProperties: false.
func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
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
