// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func seedItemsRelatedStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
}

func runItemsRelatedJSON(t *testing.T, key string) itemRelatedReport {
	t.Helper()
	flags := &rootFlags{asJSON: true}
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"related", key})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items related %s: %v; stdout=%s", key, err, out.String())
	}
	var report itemRelatedReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode related JSON %q: %v", out.String(), err)
	}
	return report
}

func TestItemsRelatedJSONReportsOutgoingExternalAndIncomingEdges(t *testing.T) {
	const externalURI = "https://zotero.org/users/123/items/EXT999"
	seedItemsRelatedStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K0","version":1,"data":{"key":"K0","itemType":"journalArticle","title":"Source Paper","relations":{"dc:relation":"https://zotero.org/users/123/items/K1"}}}`),
		json.RawMessage(`{"key":"K1","version":2,"data":{"key":"K1","itemType":"journalArticle","title":"Central Paper","relations":{"dc:relation":"https://zotero.org/users/123/items/K2","owl:sameAs":"` + externalURI + `"}}}`),
		json.RawMessage(`{"key":"K2","version":3,"data":{"key":"K2","itemType":"journalArticle","title":"Target Paper"}}`),
	})

	report := runItemsRelatedJSON(t, "K1")
	if report.Key != "K1" || report.Truncated {
		t.Fatalf("report key/truncated = %q/%v, want K1/false", report.Key, report.Truncated)
	}
	if len(report.Related) != 3 {
		t.Fatalf("related edge count = %d, want 3: %#v", len(report.Related), report.Related)
	}

	outgoingK2 := report.Related[0]
	if outgoingK2.Direction != "outgoing" || outgoingK2.Predicate != "dc:relation" || outgoingK2.SourceKey != "K1" {
		t.Fatalf("first edge = %#v, want outgoing dc:relation from K1", outgoingK2)
	}
	if outgoingK2.TargetKey != "K2" || !outgoingK2.TargetPresent || outgoingK2.Title != "Target Paper" || outgoingK2.ItemType != "journalArticle" {
		t.Fatalf("outgoing K2 hydration = %#v, want present K2 with title/type", outgoingK2)
	}

	external := report.Related[1]
	if external.Direction != "outgoing" || external.Predicate != "owl:sameAs" || external.SourceKey != "K1" {
		t.Fatalf("second edge = %#v, want outgoing owl:sameAs from K1", external)
	}
	if external.TargetPresent || external.TargetKey != "" || external.TargetURI != externalURI || external.Title != "" {
		t.Fatalf("external edge = %#v, want absent target preserving URI %q", external, externalURI)
	}

	incoming := report.Related[2]
	if incoming.Direction != "incoming" || incoming.Predicate != "dc:relation" || incoming.SourceKey != "K0" {
		t.Fatalf("third edge = %#v, want incoming dc:relation from K0", incoming)
	}
	if incoming.TargetKey != "K1" || !incoming.TargetPresent || incoming.TargetURI != "https://zotero.org/users/123/items/K1" {
		t.Fatalf("incoming target = %#v, want present K1 target", incoming)
	}
	if incoming.Title != "Source Paper" || incoming.ItemType != "journalArticle" {
		t.Fatalf("incoming source summary = title %q type %q, want Source Paper/journalArticle", incoming.Title, incoming.ItemType)
	}

	mcpJSON, err := ItemRelatedJSON(context.Background(), "K1")
	if err != nil {
		t.Fatalf("ItemRelatedJSON: %v", err)
	}
	var mcpReport itemRelatedReport
	if err := json.Unmarshal(mcpJSON, &mcpReport); err != nil {
		t.Fatalf("decode MCP related JSON %q: %v", string(mcpJSON), err)
	}
	if len(mcpReport.Related) != len(report.Related) || mcpReport.Related[0].TargetKey != "K2" || mcpReport.Related[2].SourceKey != "K0" {
		t.Fatalf("MCP related report = %#v, want same related graph as CLI", mcpReport)
	}
}

func TestParseZoteroItemURIKeyAndPredicatePreservation(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{name: "https user item", uri: "https://zotero.org/users/123/items/USERKEY", want: "USERKEY"},
		{name: "http group item", uri: "http://zotero.org/groups/987/items/GROUPKEY", want: "GROUPKEY"},
		{name: "trailing slash", uri: "https://zotero.org/users/123/items/TRAIL/", want: "TRAIL"},
		{name: "non zotero host", uri: "https://example.org/users/123/items/NOPE", want: ""},
		{name: "missing item segment", uri: "https://zotero.org/users/123/collections/COLL", want: ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseZoteroItemURIKey(tt.uri); got != tt.want {
				t.Fatalf("parseZoteroItemURIKey(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}

	const predicate = "dc:relation:with:colons"
	const nonZoteroURI = "https://example.org/users/123/items/NOPE"
	edges, err := outgoingRelationEdges(json.RawMessage(`{"key":"SRC","data":{"relations":{"`+predicate+`":"`+nonZoteroURI+`"}}}`), "SRC")
	if err != nil {
		t.Fatalf("outgoingRelationEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1: %#v", len(edges), edges)
	}
	if edges[0].Predicate != predicate || edges[0].TargetURI != nonZoteroURI || edges[0].parsedTargetKey != "" {
		t.Fatalf("edge = %#v, want predicate key with colons and preserved non-Zotero URI without parsed key", edges[0])
	}
}

func TestItemsRelatedTruncatesIncomingEdgesAtGraphNodeCap(t *testing.T) {
	items := make([]json.RawMessage, 0, graphNodeCap+2)
	items = append(items, json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","itemType":"journalArticle","title":"Central"}}`))
	for i := range graphNodeCap + 1 {
		items = append(items, json.RawMessage(fmt.Sprintf(`{"key":"S%03d","version":1,"data":{"key":"S%03d","itemType":"journalArticle","title":"Source %03d","relations":{"dc:relation":"https://zotero.org/users/1/items/K1"}}}`, i, i, i)))
	}
	seedItemsRelatedStore(t, items)

	report := runItemsRelatedJSON(t, "K1")
	if len(report.Related) != graphNodeCap {
		t.Fatalf("related count = %d, want graphNodeCap %d", len(report.Related), graphNodeCap)
	}
	if !report.Truncated {
		t.Fatal("truncated = false, want true after graphNodeCap+1 incoming edges")
	}
	for i, edge := range report.Related {
		if edge.Direction != "incoming" || edge.TargetKey != "K1" || !edge.TargetPresent {
			t.Fatalf("edge[%d] = %#v, want incoming present K1 edge", i, edge)
		}
	}
}

func TestItemsRelatedNotFoundJSONUsesGraphConvention(t *testing.T) {
	seedItemsRelatedStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","itemType":"journalArticle","title":"Present"}}`),
	})

	flags := &rootFlags{asJSON: true}
	cmd := newItemsCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"related", "MISSING"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items related missing: %v; stdout=%s", err, out.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode not-found JSON %q: %v", out.String(), err)
	}
	if got["error"] != "not found" || got["key"] != "MISSING" {
		t.Fatalf("not-found payload = %#v, want error=not found key=MISSING", got)
	}

	mcpJSON, err := ItemRelatedJSON(context.Background(), "MISSING")
	if err != nil {
		t.Fatalf("ItemRelatedJSON missing: %v", err)
	}
	var mcpGot map[string]string
	if err := json.Unmarshal(mcpJSON, &mcpGot); err != nil {
		t.Fatalf("decode MCP not-found JSON %q: %v", string(mcpJSON), err)
	}
	if mcpGot["error"] != "not found" || mcpGot["key"] != "MISSING" {
		t.Fatalf("MCP not-found payload = %#v, want graph not-found convention", mcpGot)
	}
}

func TestItemsRelatedRelationArrayProducesOneEdgePerURI(t *testing.T) {
	seedItemsRelatedStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"ARR","version":1,"data":{"key":"ARR","itemType":"journalArticle","title":"Array Source","relations":{"dc:relation":["https://zotero.org/users/123/items/K2","https://zotero.org/groups/456/items/K3"]}}}`),
		json.RawMessage(`{"key":"K2","version":2,"data":{"key":"K2","itemType":"journalArticle","title":"Target Two"}}`),
		json.RawMessage(`{"key":"K3","version":3,"data":{"key":"K3","itemType":"book","title":"Target Three"}}`),
	})

	report := runItemsRelatedJSON(t, "ARR")
	if len(report.Related) != 2 {
		t.Fatalf("related edge count = %d, want one edge per array URI: %#v", len(report.Related), report.Related)
	}
	want := map[string]string{"K2": "Target Two", "K3": "Target Three"}
	for _, edge := range report.Related {
		if edge.Direction != "outgoing" || edge.Predicate != "dc:relation" || edge.SourceKey != "ARR" || !edge.TargetPresent {
			t.Fatalf("array edge = %#v, want outgoing present dc:relation from ARR", edge)
		}
		if want[edge.TargetKey] != edge.Title {
			t.Fatalf("array edge = %#v, want hydrated target/title in %v", edge, want)
		}
		delete(want, edge.TargetKey)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected array target(s): %v", want)
	}
}
