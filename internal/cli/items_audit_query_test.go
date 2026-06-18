// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean perf-audit m4ku/2qhf): cover the indexed-column missing-PDF query
// and the single-scan audit summary so the json_extract -> indexed-column and
// multi-scan -> conditional-aggregation rewrites stay behavior-preserving.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotero-pp-cli/internal/store"
)

func seedAuditStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// P1: journalArticle, no PDF, no DOI, no abstract, no tags.
	// P2: journalArticle WITH a PDF attachment, DOI, abstract, and a tag.
	// P3: book, no PDF, no abstract, no tags (a missing-PDF candidate type).
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"P2","DOI":"10/x","abstractNote":"abs","tags":[{"tag":"t"}]}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"P3","version":1,"data":{"key":"P3","itemType":"book","title":"P3"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return localQueryStore{db}
}

func TestQueryMissingPDFItems_IndexedColumns(t *testing.T) {
	db := seedAuditStore(t)

	rows, err := queryMissingPDFItems(db, "", 0)
	if err != nil {
		t.Fatalf("queryMissingPDFItems: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[sqlStringValue(r["key"])] = true
	}
	// P1 and P3 lack a PDF child; P2 has one and must be excluded.
	if !got["P1"] || !got["P3"] {
		t.Errorf("missing-PDF keys = %v, want P1 and P3", got)
	}
	if got["P2"] {
		t.Error("P2 has a PDF attachment and must not be reported")
	}
	if len(got) != 2 {
		t.Errorf("missing-PDF count = %d, want 2 (%v)", len(got), got)
	}

	count, err := queryMissingPDFCount(db)
	if err != nil {
		t.Fatalf("queryMissingPDFCount: %v", err)
	}
	if count != 2 {
		t.Errorf("queryMissingPDFCount = %d, want 2", count)
	}

	// The --type filter narrows to a single item type via the indexed column.
	books, err := queryMissingPDFItems(db, "book", 0)
	if err != nil {
		t.Fatalf("queryMissingPDFItems(book): %v", err)
	}
	if len(books) != 1 || sqlStringValue(books[0]["key"]) != "P3" {
		t.Errorf("missing-PDF books = %v, want [P3]", books)
	}
}

func TestQueryItemsAuditSummary_SingleScan(t *testing.T) {
	db := seedAuditStore(t)

	summary, err := queryItemsAuditSummary(db)
	if err != nil {
		t.Fatalf("queryItemsAuditSummary: %v", err)
	}
	// P1 + P3 lack a PDF (P2 has one).
	if summary.MissingPDF != 2 {
		t.Errorf("MissingPDF = %d, want 2", summary.MissingPDF)
	}
	// P1, A2, P3 lack an abstract; P2 has one.
	if summary.MissingAbstract != 3 {
		t.Errorf("MissingAbstract = %d, want 3", summary.MissingAbstract)
	}
	// Only P1 is a DOI-bearing type without a DOI (P2 has one; book/attachment excluded).
	if summary.MissingDOI != 1 {
		t.Errorf("MissingDOI = %d, want 1", summary.MissingDOI)
	}
	// P1, A2, P3 have no tags; P2 has one.
	if summary.MissingTags != 3 {
		t.Errorf("MissingTags = %d, want 3", summary.MissingTags)
	}
}
