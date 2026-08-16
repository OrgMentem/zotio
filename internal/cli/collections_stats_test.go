// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func TestToInt64ParsesNumericTypes(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"int64", int64(42), 42},
		{"float64", float64(7), 7},
		{"int", int(5), 5},
		{"numeric string", "123", 123},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toInt64(tc.in)
			if err != nil {
				t.Fatalf("toInt64(%v) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("toInt64(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestToInt64NonNumericStringReturnsError(t *testing.T) {
	got, err := toInt64("not-a-number")
	if err == nil {
		t.Fatalf("toInt64(%q) = %d, nil; want parse error instead of silent 0", "not-a-number", got)
	}
	if got != 0 {
		t.Fatalf("toInt64(%q) value = %d, want 0 on error", "not-a-number", got)
	}
}

// seedStoreWithItems opens an isolated store and upserts the given item
// payloads. Caller must close via t.Cleanup; this mirrors seedAuditStore.
func seedStoreWithItems(t *testing.T, items []json.RawMessage) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed UpsertBatch: %v", err)
	}
	return localQueryStore{db}
}

func TestCollectionStatsPDFCountsDistinctParents(t *testing.T) {
	// One collection item P1 with TWO PDF attachments A1, A2. The numerator
	// must count distinct parents (1), not attachment rows (2), otherwise
	// pdf_pct would be 200%.
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
	}
	db := seedStoreWithItems(t, items)

	// Replicate the fixed collection stats PDF query verbatim.
	pdfRows, err := db.QueryRaw(`
SELECT COUNT(DISTINCT a.parent_key) AS items_with_pdf
FROM resources a
WHERE a.resource_type='items'
  AND a.item_type='attachment'
  AND json_extract(a.data,'$.data.contentType')='application/pdf'
  AND EXISTS (
    SELECT 1 FROM resources i
    WHERE i.resource_type='items'
      AND i.id = a.parent_key
      AND EXISTS (
        SELECT 1 FROM json_each(json_extract(i.data,'$.data.collections')) c
        WHERE c.value = ?
      )
  )`, "COL1")
	if err != nil {
		t.Fatalf("QueryRaw pdf count: %v", err)
	}
	if len(pdfRows) == 0 {
		t.Fatalf("no rows from pdf count query")
	}
	got := sqlIntValue(pdfRows[0]["items_with_pdf"])
	if got != 1 {
		t.Fatalf("items_with_pdf = %d, want 1 (distinct parent, not attachment rows)", got)
	}

	// Verify total and derived percentage as the command computes it.
	summaryRows, err := db.QueryRaw(`
SELECT COUNT(*) AS total
FROM resources
WHERE resource_type='items'
  AND item_type NOT IN ('attachment','note','annotation')
  AND EXISTS (
    SELECT 1 FROM json_each(json_extract(data,'$.data.collections')) c
    WHERE c.value = ?
  )`, "COL1")
	if err != nil {
		t.Fatalf("QueryRaw total: %v", err)
	}
	total := sqlIntValue(summaryRows[0]["total"])
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	var pdfPct float64
	if total > 0 {
		pdfPct = float64(got) / float64(total) * 100
	}
	if pdfPct != 100 {
		t.Fatalf("pdfPct = %v, want 100 (clamping not used; DISTINCT fix is the cause)", pdfPct)
	}
}

func TestLibraryPDFCoverageCountsDistinctParents(t *testing.T) {
	// One qualifying item P1 with TWO PDF attachments must contribute 1 to
	// items_with_pdf, not 2. Verifies COUNT(DISTINCT CASE WHEN a.id IS NOT NULL THEN i.id END).
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1"}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
	}
	db := seedStoreWithItems(t, items)
	coverage, err := queryLibraryPDFCoverage(db)
	if err != nil {
		t.Fatalf("queryLibraryPDFCoverage: %v", err)
	}
	if coverage.TotalItems != 1 {
		t.Fatalf("TotalItems = %d, want 1", coverage.TotalItems)
	}
	if coverage.ItemsWithPDF != 1 {
		t.Fatalf("ItemsWithPDF = %d, want 1 (distinct parent via CASE guard, not distinct attachments)", coverage.ItemsWithPDF)
	}
	if coverage.Pct != 100 {
		t.Fatalf("Pct = %d, want 100", coverage.Pct)
	}
}

func TestLibraryPDFCoverageExcludesZeroPDFItems(t *testing.T) {
	// P1 has a PDF, P2 has none. LEFT JOIN CASE guard must exclude P2
	// from the numerator; a bare COUNT(DISTINCT i.id) would incorrectly
	// count both.
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"P2"}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
	}
	db := seedStoreWithItems(t, items)
	coverage, err := queryLibraryPDFCoverage(db)
	if err != nil {
		t.Fatalf("queryLibraryPDFCoverage: %v", err)
	}
	if coverage.TotalItems != 2 {
		t.Fatalf("TotalItems = %d, want 2", coverage.TotalItems)
	}
	if coverage.ItemsWithPDF != 1 {
		t.Fatalf("ItemsWithPDF = %d, want 1 (P2 with no PDF must be excluded)", coverage.ItemsWithPDF)
	}
	if coverage.Pct != 50 {
		t.Fatalf("Pct = %d, want 50", coverage.Pct)
	}
}
