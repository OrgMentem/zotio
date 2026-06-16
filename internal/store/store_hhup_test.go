// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean hhup): cover dependent-resource columns and the annotation/
// fulltext query helpers added for dependent-resource sync.

package store

import (
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestDependentResourceColumnsAndQueries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
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
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
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
