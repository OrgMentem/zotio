// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean qfuq): drive resources/prompts through the MCP server's JSON-RPC
// dispatch (as an inspector/client would) and check payloads.

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zotio/internal/store"

	"github.com/mark3labs/mcp-go/server"
)

func qfuqServer(t *testing.T) *server.MCPServer {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // isolate dbPath() from the real store
	s := server.NewMCPServer("Zotero", "1.0.0",
		server.WithResourceCapabilities(false, true),
		server.WithPromptCapabilities(true),
	)
	RegisterResources(s)
	RegisterPrompts(s)
	return s
}

// rpc sends one JSON-RPC request through the server and returns the parsed
// result object, failing on any transport or JSON-RPC error.
func rpc(t *testing.T, s *server.MCPServer, method string, params any) map[string]any {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, _ := json.Marshal(req)
	resp := s.HandleMessage(context.Background(), raw)
	out, _ := json.Marshal(resp)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("%s: decode response: %v", method, err)
	}
	if e, ok := parsed["error"]; ok {
		t.Fatalf("%s: JSON-RPC error: %v", method, e)
	}
	result, ok := parsed["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result object in %s", method, out)
	}
	return result
}

func collectStrings(list any, field string) map[string]bool {
	set := map[string]bool{}
	items, _ := list.([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if s, ok := m[field].(string); ok {
				set[s] = true
			}
		}
	}
	return set
}

func TestMCPListResources(t *testing.T) {
	s := qfuqServer(t)

	uris := collectStrings(rpc(t, s, "resources/list", nil)["resources"], "uri")
	for _, want := range []string{"zotero://context", "zotero://agent-context", "zotero://status", "zotero://schema"} {
		if !uris[want] {
			t.Errorf("resources/list missing %q (got %v)", want, uris)
		}
	}

	tmpls := collectStrings(rpc(t, s, "resources/templates/list", nil)["resourceTemplates"], "uriTemplate")
	for _, want := range []string{"zotero://collections/{key}", "zotero://items/{key}"} {
		if !tmpls[want] {
			t.Errorf("templates/list missing %q (got %v)", want, tmpls)
		}
	}
}

func TestMCPListPrompts(t *testing.T) {
	s := qfuqServer(t)
	names := collectStrings(rpc(t, s, "prompts/list", nil)["prompts"], "name")
	for _, want := range []string{"inspect-library", "export-reading-notes", "prepare-citation-export", "synthesize"} {
		if !names[want] {
			t.Errorf("prompts/list missing %q (got %v)", want, names)
		}
	}
}

func TestMCPReadContextResource(t *testing.T) {
	s := qfuqServer(t)
	result := rpc(t, s, "resources/read", map[string]any{"uri": "zotero://context"})
	text := firstResourceText(t, result)
	var ctx map[string]any
	if err := json.Unmarshal([]byte(text), &ctx); err != nil {
		t.Fatalf("context payload not JSON: %v", err)
	}
	if ctx["api"] != "zotero" {
		t.Errorf("context api = %v, want zotero", ctx["api"])
	}
	// The resource payload must equal the context tool's payload (shared source).
	toolJSON, _ := json.Marshal(domainContext())
	var fromTool map[string]any
	_ = json.Unmarshal(toolJSON, &fromTool)
	if fromTool["tool_surface"] != ctx["tool_surface"] {
		t.Errorf("resource/tool context drift on tool_surface")
	}
}

func TestMCPReadAgentContextResource(t *testing.T) {
	s := qfuqServer(t)
	result := rpc(t, s, "resources/read", map[string]any{"uri": "zotero://agent-context"})
	text := firstResourceText(t, result)
	var ac map[string]any
	if err := json.Unmarshal([]byte(text), &ac); err != nil {
		t.Fatalf("agent-context payload not JSON: %v", err)
	}
	if ac["schema_version"] == nil {
		t.Errorf("agent-context missing schema_version: %v", ac)
	}
	if _, ok := ac["commands"]; !ok {
		t.Errorf("agent-context missing commands")
	}
}

func TestMCPReadStatusResourceNotSynced(t *testing.T) {
	s := qfuqServer(t) // temp HOME -> no local store
	result := rpc(t, s, "resources/read", map[string]any{"uri": "zotero://status"})
	text := firstResourceText(t, result)
	var status map[string]any
	if err := json.Unmarshal([]byte(text), &status); err != nil {
		t.Fatalf("status payload not JSON: %v", err)
	}
	if status["synced"] != false {
		t.Errorf("status synced = %v, want false for empty store", status["synced"])
	}
}

func TestMCPGetPrompt(t *testing.T) {
	s := qfuqServer(t)
	result := rpc(t, s, "prompts/get", map[string]any{"name": "inspect-library"})
	msgs, ok := result["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("inspect-library returned no messages: %v", result)
	}
}

func firstResourceText(t *testing.T, result map[string]any) string {
	t.Helper()
	contents, ok := result["contents"].([]any)
	if !ok || len(contents) == 0 {
		t.Fatalf("no resource contents: %v", result)
	}
	first, _ := contents[0].(map[string]any)
	text, _ := first["text"].(string)
	if text == "" {
		t.Fatalf("empty resource text: %v", first)
	}
	return text
}

// seedStore writes a small library into the canonical dbPath under the current
// (test-isolated) HOME so the template resource handlers have data to read.
func seedStore(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath()), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	items := []json.RawMessage{
		json.RawMessage(`{"key":"TOP1","version":1,"data":{"key":"TOP1","itemType":"journalArticle","title":"Paper One","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"book","title":"Another","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"TOP1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1","annotationText":"highlight"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Upsert("collections", "COL1", json.RawMessage(`{"key":"COL1","version":1,"data":{"key":"COL1","name":"Reading"}}`)); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
}

func TestMCPCollectionManifestResource(t *testing.T) {
	s := qfuqServer(t)
	seedStore(t)
	result := rpc(t, s, "resources/read", map[string]any{"uri": "zotero://collections/COL1"})
	var manifest map[string]any
	if err := json.Unmarshal([]byte(firstResourceText(t, result)), &manifest); err != nil {
		t.Fatalf("manifest not JSON: %v", err)
	}
	if manifest["item_count"].(float64) != 2 {
		t.Errorf("item_count = %v, want 2", manifest["item_count"])
	}
	keys := collectStrings(manifest["items"], "key")
	if !keys["TOP1"] || !keys["P2"] {
		t.Errorf("manifest items = %v, want TOP1 and P2", keys)
	}
}

func TestMCPItemBundleResource(t *testing.T) {
	s := qfuqServer(t)
	seedStore(t)
	result := rpc(t, s, "resources/read", map[string]any{"uri": "zotero://items/TOP1"})
	var bundle map[string]any
	if err := json.Unmarshal([]byte(firstResourceText(t, result)), &bundle); err != nil {
		t.Fatalf("bundle not JSON: %v", err)
	}
	if bundle["annotation_count"].(float64) != 1 {
		t.Errorf("annotation_count = %v, want 1 (AN1 via ATT1)", bundle["annotation_count"])
	}
	if bundle["item"] == nil {
		t.Errorf("bundle missing item payload")
	}
}
