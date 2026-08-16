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
	// One eligible collection item P1 with TWO PDF attachments A1, A2.
	// Numerator must count distinct parents (1), not attachment rows (2),
	// otherwise pct would be 200. Exercises the production seam
	// queryCollectionPDFCount / queryCollectionStatsSummary so a reversion
	// to COUNT(*) causes failure (negative control a).
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
	}
	db := seedStoreWithItems(t, items)

	got, err := queryCollectionPDFCount(db, "COL1")
	if err != nil {
		t.Fatalf("queryCollectionPDFCount: %v", err)
	}
	if got != 1 {
		t.Fatalf("items_with_pdf = %d, want 1 (distinct parent, not attachment rows)", got)
	}

	total, _, _, err := queryCollectionStatsSummary(db, "COL1")
	if err != nil {
		t.Fatalf("queryCollectionStatsSummary: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	var pdfPct float64
	if total > 0 {
		pdfPct = float64(got) / float64(total) * 100
	}
	if pdfPct != 100 {
		t.Fatalf("pdfPct = %v, want 100 (DISTINCT bounds it; must not be 200)", pdfPct)
	}
	if pdfPct > 100 {
		t.Fatalf("pdfPct = %v, must be bounded by 100", pdfPct)
	}
}

func TestCollectionStatsPDFExcludesIneligibleParents(t *testing.T) {
	// One eligible item + a note and an annotation in the same collection,
	// each with a PDF child. Numerator must count only the eligible parent
	// (negative control b: removing the eligibility predicate must fail this).
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"N1","version":1,"data":{"key":"N1","itemType":"note","note":"n","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","annotationText":"a","collections":["COL1"]}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"N1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"A3","version":1,"data":{"key":"A3","itemType":"attachment","parentItem":"AN1","contentType":"application/pdf"}}`),
	}
	db := seedStoreWithItems(t, items)

	total, _, _, err := queryCollectionStatsSummary(db, "COL1")
	if err != nil {
		t.Fatalf("queryCollectionStatsSummary: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1 (only eligible parent counted)", total)
	}

	got, err := queryCollectionPDFCount(db, "COL1")
	if err != nil {
		t.Fatalf("queryCollectionPDFCount: %v", err)
	}
	if got != 1 {
		t.Fatalf("items_with_pdf = %d, want 1 (only eligible parent in collection should count)", got)
	}

	var pdfPct float64
	if total > 0 {
		pdfPct = float64(got) / float64(total) * 100
	}
	if pdfPct > 100 {
		t.Fatalf("pdfPct = %v, want <= 100 (ineligible parents must not inflate numerator)", pdfPct)
	}
	if pdfPct != 100 {
		t.Fatalf("pdfPct = %v, want 100", pdfPct)
	}
}

func TestCollectionStatsEligibleItemPredicateIsShared(t *testing.T) {
	want := "item_type NOT IN ('attachment','note','annotation')"
	if collectionStatsEligibleItemPredicate != want {
		t.Fatalf("predicate = %q, want %q", collectionStatsEligibleItemPredicate, want)
	}
}

func TestLibraryPDFCoverageCountsDistinctParents(t *testing.T) {
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
