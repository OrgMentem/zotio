// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase1-followup): cover the duplicate-title attachment
// exclusion so attachments sharing a generic name ("PDF", "Snapshot") are never
// reported as bibliographic duplicates.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func TestQueryDuplicateTitlesExcludesAttachments(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	items := []json.RawMessage{
		// Two real articles sharing a title — a genuine duplicate group.
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Shared Title"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Shared Title"}}`),
		// Two attachments named "PDF" — must NOT be flagged as duplicates.
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","title":"PDF","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","title":"PDF","contentType":"application/pdf"}}`),
	}
	qs := localQueryStore{db}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := queryDuplicateTitles(qs)
	if err != nil {
		t.Fatalf("queryDuplicateTitles: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicate-title groups = %d, want 1 (only the article pair): %v", len(rows), rows)
	}
	if got := sqlStringValue(rows[0]["value"]); got != "Shared Title" {
		t.Errorf("duplicate group value = %q, want \"Shared Title\"", got)
	}
}
