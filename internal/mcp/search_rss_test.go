// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"zotio/internal/store"

	mcplib "github.com/mark3labs/mcp-go/mcp"
)

func TestRssSearchFulltextCollectionScopeBeforeLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	db, err := store.OpenWithContext(t.Context(), mustDBPath(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"IN","data":{"key":"IN","itemType":"journalArticle","title":"In-scope paper","collections":["COLL"]}}`),
		json.RawMessage(`{"key":"OUT","data":{"key":"OUT","itemType":"journalArticle","title":"Outside paper"}}`),
		json.RawMessage(`{"key":"AIN","data":{"key":"AIN","itemType":"attachment","parentItem":"IN"}}`),
		json.RawMessage(`{"key":"AOUT","data":{"key":"AOUT","itemType":"attachment","parentItem":"OUT"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if _, err := db.UpsertKeyed("fulltext", []string{"AIN", "AOUT"}, []json.RawMessage{
		json.RawMessage(`{"content":"A long methods section mentions calibration once among many unrelated words about sampling results discussion limitations and future work"}`),
		json.RawMessage(`{"content":"calibration calibration calibration"}`),
	}); err != nil {
		t.Fatalf("seed full text: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	for _, tc := range []struct {
		name  string
		scope string
		want  string
	}{
		{name: "unscoped stronger hit", want: "OUT"},
		{name: "collection weaker hit", scope: "collection:COLL", want: "IN"},
		{name: "empty cohort", scope: "collection:MISSING"},
		{name: "explicit whole library", scope: "library", want: "OUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			args := map[string]any{"query": "calibration", "fulltext": true, "limit": float64(1)}
			if tc.scope != "" {
				args["scope"] = tc.scope
			}
			req.Params.Arguments = args
			result, err := handleSearch(t.Context(), req)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("search result = %+v, error = %v", result, err)
			}
			var got struct {
				Items []struct {
					ItemKey string `json:"item_key"`
				} `json:"items"`
			}
			if err := json.Unmarshal([]byte(toolResultText(t, result)), &got); err != nil {
				t.Fatalf("decode search: %v", err)
			}
			if tc.want == "" {
				if len(got.Items) != 0 {
					t.Fatalf("empty cohort returns %+v", got.Items)
				}
				return
			}
			if len(got.Items) != 1 || got.Items[0].ItemKey != tc.want {
				t.Fatalf("scope %q limit 1 returns %+v, want parent %s", tc.scope, got.Items, tc.want)
			}
		})
	}
}

func TestRssSearchScopeRequiresFulltext(t *testing.T) {
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "calibration", "scope": "collection:COLL"}
	result, err := handleSearch(t.Context(), req)
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("search result = %+v, error = %v; want refusal", result, err)
	}
	if text := toolResultText(t, result); !strings.Contains(text, "scope requires fulltext") {
		t.Fatalf("refusal = %q, want fulltext requirement", text)
	}
}
