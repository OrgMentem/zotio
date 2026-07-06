// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean hhup): cover dependent-resource columns and the annotation/
// fulltext query helpers added for dependent-resource sync.

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestDependentResourceColumnsAndQueries(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A Zotero hierarchy: top item -> PDF attachment -> annotation.
	items := []json.RawMessage{
		json.RawMessage(`{"key":"TOP1","version":1,"data":{"key":"TOP1","itemType":"journalArticle","title":"Paper"}}`),
		json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"TOP1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1","annotationColor":"#ff0","annotationText":"highlight"}}`),
	}
	stored, _, err := s.UpsertBatch("items", items)
	if err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if stored != 3 {
		t.Fatalf("stored = %d, want 3", stored)
	}

	// Indexed columns are populated from the nested data sub-object.
	var parentKey, itemType string
	if err := s.DB().QueryRow(`SELECT parent_key, item_type FROM resources WHERE id = ?`, "AN1").Scan(&parentKey, &itemType); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if parentKey != "ATT1" {
		t.Errorf("AN1 parent_key = %q, want ATT1", parentKey)
	}
	if itemType != "annotation" {
		t.Errorf("AN1 item_type = %q, want annotation", itemType)
	}

	// ItemsByType filters on the indexed column.
	annotations, err := s.ItemsByType("annotation", 0)
	if err != nil {
		t.Fatalf("ItemsByType: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("ItemsByType(annotation) = %d rows, want 1", len(annotations))
	}

	// AnnotationsForItem joins annotation -> attachment -> top item.
	forTop, err := s.AnnotationsForItem("TOP1")
	if err != nil {
		t.Fatalf("AnnotationsForItem: %v", err)
	}
	if len(forTop) != 1 {
		t.Fatalf("AnnotationsForItem(TOP1) = %d rows, want 1", len(forTop))
	}
	var got map[string]any
	if err := json.Unmarshal(forTop[0], &got); err != nil {
		t.Fatalf("decode annotation: %v", err)
	}
	if got["key"] != "AN1" {
		t.Errorf("AnnotationsForItem returned key %v, want AN1", got["key"])
	}

	// An unrelated top item resolves no annotations.
	none, err := s.AnnotationsForItem("OTHER")
	if err != nil {
		t.Fatalf("AnnotationsForItem(OTHER): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("AnnotationsForItem(OTHER) = %d rows, want 0", len(none))
	}
}

func TestFulltextRoundTrip(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Upsert("fulltext", "ATT1", json.RawMessage(`{"content":"hello world","indexedChars":11}`)); err != nil {
		t.Fatalf("Upsert fulltext: %v", err)
	}
	data, ok, err := s.Fulltext("ATT1")
	if err != nil {
		t.Fatalf("Fulltext: %v", err)
	}
	if !ok {
		t.Fatal("Fulltext(ATT1) not found")
	}
	if len(data) == 0 {
		t.Fatal("Fulltext(ATT1) returned empty data")
	}
	if _, ok, _ := s.Fulltext("MISSING"); ok {
		t.Error("Fulltext(MISSING) reported found")
	}
}

// PATCH(glean perf-audit x5lh): UpsertKeyed persists caller-keyed payloads (no
// id in the body) in one transaction and round-trips through Fulltext.
func TestUpsertKeyed(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ids := []string{"ATT1", "ATT2"}
	data := []json.RawMessage{
		json.RawMessage(`{"content":"alpha","indexedChars":5}`),
		json.RawMessage(`{"content":"beta","indexedChars":4}`),
	}
	if err := s.UpsertKeyed("fulltext", ids, data); err != nil {
		t.Fatalf("UpsertKeyed: %v", err)
	}
	for _, id := range ids {
		got, ok, err := s.Fulltext(id)
		if err != nil {
			t.Fatalf("Fulltext(%s): %v", id, err)
		}
		if !ok || len(got) == 0 {
			t.Fatalf("Fulltext(%s) not stored", id)
		}
	}

	// Re-keying the same id replaces rather than duplicates.
	if err := s.UpsertKeyed("fulltext", []string{"ATT1"}, []json.RawMessage{json.RawMessage(`{"content":"alpha2"}`)}); err != nil {
		t.Fatalf("UpsertKeyed replace: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE resource_type='fulltext'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("fulltext rows = %d, want 2 (replace, not insert)", count)
	}

	// Length mismatch is rejected; empty input is a no-op.
	if err := s.UpsertKeyed("fulltext", []string{"X"}, nil); err == nil {
		t.Error("UpsertKeyed mismatch: want error, got nil")
	}
	if err := s.UpsertKeyed("fulltext", nil, nil); err != nil {
		t.Errorf("UpsertKeyed empty: %v", err)
	}
}

// PATCH(glean perf-audit rj6r): AnnotationsForItems returns the same rows as
// per-item AnnotationsForItem but grouped, in a single query.
func TestAnnotationsForItems(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	items := []json.RawMessage{
		json.RawMessage(`{"key":"TOP1","version":1,"data":{"key":"TOP1","itemType":"journalArticle","title":"A"}}`),
		json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"TOP1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1"}}`),
		json.RawMessage(`{"key":"TOP2","version":1,"data":{"key":"TOP2","itemType":"journalArticle","title":"B"}}`),
		json.RawMessage(`{"key":"ATT2","version":1,"data":{"key":"ATT2","itemType":"attachment","parentItem":"TOP2","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN2","version":1,"data":{"key":"AN2","itemType":"annotation","parentItem":"ATT2"}}`),
		json.RawMessage(`{"key":"AN3","version":1,"data":{"key":"AN3","itemType":"annotation","parentItem":"ATT2"}}`),
	}
	if _, _, err := s.UpsertBatch("items", items); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}

	grouped, err := s.AnnotationsForItems([]string{"TOP1", "TOP2", "OTHER"})
	if err != nil {
		t.Fatalf("AnnotationsForItems: %v", err)
	}
	if len(grouped["TOP1"]) != 1 {
		t.Errorf("TOP1 annotations = %d, want 1", len(grouped["TOP1"]))
	}
	if len(grouped["TOP2"]) != 2 {
		t.Errorf("TOP2 annotations = %d, want 2", len(grouped["TOP2"]))
	}
	if _, ok := grouped["OTHER"]; ok {
		t.Errorf("OTHER should be absent, got %d", len(grouped["OTHER"]))
	}

	// Empty input returns an empty (non-nil) map.
	empty, err := s.AnnotationsForItems(nil)
	if err != nil {
		t.Fatalf("AnnotationsForItems(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("AnnotationsForItems(nil) = %v, want empty map", empty)
	}
}
