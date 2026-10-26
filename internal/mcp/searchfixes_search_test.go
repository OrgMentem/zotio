// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

func sliceSearchSeedMCPMirror(t *testing.T) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), mustDBPath(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	raw := make([]json.RawMessage, 0, 2)
	raw = append(raw,
		json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"journalArticle","title":"MCP Parent"}}`),
		json.RawMessage(`{"key":"ABCD2346","version":1,"data":{"key":"ABCD2346","itemType":"attachment","parentItem":"ABCD2345","contentType":"application/pdf"}}`),
	)
	if _, _, err := db.UpsertBatch("items", raw); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.SaveSyncStateContext(context.Background(), "items", "", -1); err != nil {
		_ = db.Close()
		t.Fatalf("seed items sync state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// An MCP full-text search over a mirror with synced items but no indexed PDF
// text must return a not-indexed tool error naming `zotio sync --fulltext`,
// never a successful empty envelope an agent would read as "no match".
func TestSliceSearchMCPFulltextWithoutIndexRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	sliceSearchSeedMCPMirror(t)
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "calibration", "fulltext": true}
	res, err := handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("handleSearch result = %+v, want tool error", res)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "sync --fulltext") {
		t.Fatalf("tool error = %q, want remediation naming `zotio sync --fulltext`", text)
	}
}

// The same MCP query over an indexed mirror answers normally: once rows
// exist, an empty answer is a genuine no-match, not a readiness failure.
func TestSliceSearchMCPFulltextIndexedEmptySucceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	sliceSearchSeedMCPMirror(t)
	db, err := store.OpenWithContext(context.Background(), mustDBPath(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"content": "some unrelated prose about methods"})
	if err != nil {
		_ = db.Close()
		t.Fatalf("encode full text: %v", err)
	}
	if _, err := db.UpsertKeyed("fulltext", []string{"ABCD2346"}, []json.RawMessage{payload}); err != nil {
		_ = db.Close()
		t.Fatalf("seed full text: %v", err)
	}
	if err := db.SaveSyncState("fulltext", "", 1); err != nil {
		_ = db.Close()
		t.Fatalf("seed fulltext completion: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "zzzznomatch", "fulltext": true}
	res, err := handleSearch(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearch protocol error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("handleSearch result = %+v, want success", res)
	}
	if !strings.Contains(toolResultText(t, res), `"items"`) {
		t.Fatalf("indexed empty search = %q, want library frame with items", toolResultText(t, res))
	}
}

// Whitespace-only MCP queries must be rejected like missing ones so scripts
// passing an unset variable get a contract error, not an FTS5 syntax failure.
func TestSliceSearchMCPWhitespaceQueryRejected(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "home"))
	for _, q := range []string{"", "   "} {
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = map[string]any{"query": q}
		res, err := handleSearch(context.Background(), req)
		if err != nil {
			t.Fatalf("handleSearch(%q) protocol error: %v", q, err)
		}
		if res == nil || !res.IsError {
			t.Fatalf("handleSearch(%q) result = %+v, want tool error", q, res)
		}
	}
}

// TestHandleSQLSurfacesIteratorErrorAfterRowLimit pins the keep-stepping loop
// in handleSQL: truncation must never mask a query that fails on a later row.
// The query returns sqlRowLimit+501 selectable rows whose later segment fails
// inside iteration (json() rejects malformed input, and CASE evaluates it
// lazily per row rather than at prepare time), so a correct implementation
// reports the failure while a break-on-limit implementation would report a
// successful truncated envelope. The failing rows sit past the limit so the
// guard is reachable: the first sqlRowLimit rows scan cleanly, then one
// poisons the iterator.
func TestHandleSQLSurfacesIteratorErrorAfterRowLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	db, err := store.OpenWithContext(context.Background(), mustDBPath(t))
	if err != nil {
		t.Fatalf("open writable db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writable db: %v", err)
	}
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query": "WITH RECURSIVE cnt(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM cnt) SELECT CASE WHEN x > 5000 THEN json('{{{not json') ELSE x END AS v FROM cnt LIMIT 5501",
	}
	res, err := handleSQL(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSQL protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("handleSQL result = %+v, want query-failed tool error", res)
	}
	if text := toolResultText(t, res); !strings.Contains(text, "query failed") {
		t.Fatalf("tool error = %q, want it to surface the late iterator failure", text)
	}
}
