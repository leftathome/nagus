package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// doMCP posts a raw JSON-RPC body to /mcp and returns the recorder.
func doMCP(t *testing.T, srv *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	srv.routes().ServeHTTP(rec, req)
	return rec
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeRPC(t *testing.T, rec *httptest.ResponseRecorder) rpcEnvelope {
	t.Helper()
	var env rpcEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode rpc envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env
}

func TestMCPInitialize(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	env := decodeRPC(t, rec)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ProtocolVersion != "2025-06-18" {
		t.Fatalf("protocolVersion = %q", result.ProtocolVersion)
	}
	if result.Capabilities.Tools == nil {
		t.Fatal("expected capabilities.tools to be present")
	}
	if result.ServerInfo.Name != "nagus" {
		t.Fatalf("serverInfo.name = %q, want nagus", result.ServerInfo.Name)
	}
}

func TestMCPNotificationsInitializedHasNoBody(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("expected empty body for a notification, got %q", rec.Body.String())
	}
}

func TestMCPToolsList(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	env := decodeRPC(t, rec)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(result.Tools))
	}
	byName := map[string]bool{}
	for _, tool := range result.Tools {
		byName[tool.Name] = true
		if tool.Name == "get_item" {
			props, _ := tool.InputSchema["properties"].(map[string]any)
			if _, ok := props["id"]; !ok {
				t.Fatal("get_item inputSchema missing 'id' property")
			}
			req, ok := tool.InputSchema["required"].([]any)
			if !ok || len(req) != 1 || req[0] != "id" {
				t.Fatalf("get_item inputSchema required = %v, want [\"id\"]", tool.InputSchema["required"])
			}
		}
	}
	if !byName["search_items"] || !byName["get_item"] {
		t.Fatalf("tools = %v, want search_items and get_item", byName)
	}
}

func TestMCPToolsCallSearchItems(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_items","arguments":{}}}`)
	env := decodeRPC(t, rec)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent struct {
			Matched  int         `json:"matched"`
			Filtered int         `json:"filtered"`
			Items    []searchRow `json:"items"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.IsError {
		t.Fatal("expected isError=false")
	}
	if len(result.Content) == 0 || result.Content[0].Type != "text" {
		t.Fatalf("content = %+v", result.Content)
	}
	if len(result.StructuredContent.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(result.StructuredContent.Items))
	}
	if result.StructuredContent.Items[0].Verdict != "great" || result.StructuredContent.Items[0].Condition != "used" {
		t.Fatalf("top item = %+v, want verdict=great condition=used", result.StructuredContent.Items[0])
	}
}

func TestMCPToolsCallSearchItemsLimit(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_items","arguments":{"limit":1}}}`)
	env := decodeRPC(t, rec)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result struct {
		StructuredContent struct {
			Items []searchRow `json:"items"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.StructuredContent.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(result.StructuredContent.Items))
	}
}

func TestMCPToolsCallGetItem(t *testing.T) {
	srv := newTestServer(t)

	// Get a real id via search_items first.
	searchRec := doMCP(t, srv, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search_items","arguments":{"limit":1}}}`)
	searchEnv := decodeRPC(t, searchRec)
	var searchResult struct {
		StructuredContent struct {
			Items []searchRow `json:"items"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(searchEnv.Result, &searchResult); err != nil || len(searchResult.StructuredContent.Items) != 1 {
		t.Fatalf("seed search failed: err=%v items=%+v", err, searchResult.StructuredContent.Items)
	}
	id := searchResult.StructuredContent.Items[0].ID

	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"`+id+`"}}}`)
	env := decodeRPC(t, rec)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.IsError {
		t.Fatal("expected isError=false for a real id")
	}
	if result.StructuredContent["id"] != id {
		t.Fatalf("structuredContent id = %v, want %s", result.StructuredContent["id"], id)
	}

	// Bogus id -> tool-level error (isError:true), NOT a JSON-RPC error.
	badRec := doMCP(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"get_item","arguments":{"id":"does-not-exist"}}}`)
	badEnv := decodeRPC(t, badRec)
	if badEnv.Error != nil {
		t.Fatalf("expected no JSON-RPC error for a missing item, got %+v", badEnv.Error)
	}
	var badResult struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(badEnv.Result, &badResult); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !badResult.IsError {
		t.Fatal("expected isError=true for a missing item")
	}
}

func TestMCPToolsCallUnknownTool(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"delete_everything","arguments":{}}}`)
	env := decodeRPC(t, rec)
	if env.Error == nil {
		t.Fatal("expected a JSON-RPC error for an unknown tool")
	}
	if env.Error.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", env.Error.Code)
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":9,"method":"does_not_exist"}`)
	env := decodeRPC(t, rec)
	if env.Error == nil {
		t.Fatal("expected a JSON-RPC error for an unknown method")
	}
	if env.Error.Code != -32601 {
		t.Fatalf("error code = %d, want -32601", env.Error.Code)
	}
}

func TestMCPMalformedJSON(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{not valid json`)
	env := decodeRPC(t, rec)
	if env.Error == nil {
		t.Fatal("expected a JSON-RPC error for malformed JSON")
	}
	if env.Error.Code != -32700 {
		t.Fatalf("error code = %d, want -32700", env.Error.Code)
	}
}

func TestMCPPing(t *testing.T) {
	srv := newTestServer(t)
	rec := doMCP(t, srv, `{"jsonrpc":"2.0","id":10,"method":"ping"}`)
	env := decodeRPC(t, rec)
	if env.Error != nil {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if string(env.Result) != "{}" {
		t.Fatalf("ping result = %s, want {}", env.Result)
	}
}
