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

// TestQueryCollectionTopVenuesFallbackAndOrdering pins the whole observable
// contract of queryCollectionTopVenues, which no other test names: the
// publicationTitle -> bookTitle -> publisher COALESCE precedence, the HAVING
// that drops null and empty venues, the json_each collection-membership test,
// the shared eligibility predicate, ORDER BY count DESC, and LIMIT.
//
// Every counted venue gets a distinct count (4, 3, 2, 1) so the descending
// order is fully determined. Each losing fallback field carries an "Ignored…"
// value, so reversing the COALESCE arms renames the venues and fails here.
func TestQueryCollectionTopVenuesFallbackAndOrdering(t *testing.T) {
	items := []json.RawMessage{
		// publicationTitle must win over the competing publisher: 4 items.
		json.RawMessage(`{"key":"VJ1","version":1,"data":{"key":"VJ1","itemType":"journalArticle","title":"VJ1","collections":["COL1"],"publicationTitle":"Nature","publisher":"Ignored Springer"}}`),
		json.RawMessage(`{"key":"VJ2","version":1,"data":{"key":"VJ2","itemType":"journalArticle","title":"VJ2","collections":["COL1"],"publicationTitle":"Nature","publisher":"Ignored Springer"}}`),
		json.RawMessage(`{"key":"VJ3","version":1,"data":{"key":"VJ3","itemType":"journalArticle","title":"VJ3","collections":["COL1"],"publicationTitle":"Nature","publisher":"Ignored Springer"}}`),
		json.RawMessage(`{"key":"VJ4","version":1,"data":{"key":"VJ4","itemType":"journalArticle","title":"VJ4","collections":["COL1"],"publicationTitle":"Nature","publisher":"Ignored Springer"}}`),
		// No publicationTitle: bookTitle must win over the publisher. 3 items.
		json.RawMessage(`{"key":"VB1","version":1,"data":{"key":"VB1","itemType":"bookSection","title":"VB1","collections":["COL1"],"bookTitle":"Handbook of Optics","publisher":"Ignored Elsevier"}}`),
		json.RawMessage(`{"key":"VB2","version":1,"data":{"key":"VB2","itemType":"bookSection","title":"VB2","collections":["COL1"],"bookTitle":"Handbook of Optics","publisher":"Ignored Elsevier"}}`),
		json.RawMessage(`{"key":"VB3","version":1,"data":{"key":"VB3","itemType":"bookSection","title":"VB3","collections":["COL1"],"bookTitle":"Handbook of Optics","publisher":"Ignored Elsevier"}}`),
		// Publisher is the last resort. 2 items.
		json.RawMessage(`{"key":"VP1","version":1,"data":{"key":"VP1","itemType":"book","title":"VP1","collections":["COL1"],"publisher":"MIT Press"}}`),
		json.RawMessage(`{"key":"VP2","version":1,"data":{"key":"VP2","itemType":"book","title":"VP2","collections":["COL1"],"publisher":"MIT Press"}}`),
		// All three fields present: precedence must select publicationTitle.
		json.RawMessage(`{"key":"VM1","version":1,"data":{"key":"VM1","itemType":"journalArticle","title":"VM1","collections":["COL1"],"publicationTitle":"Science","bookTitle":"Ignored Book","publisher":"Ignored Press"}}`),

		// Venueless items in COL1: empty, whitespace-only, and absent. All
		// three collapse into the NULL group that HAVING must drop.
		json.RawMessage(`{"key":"VZ1","version":1,"data":{"key":"VZ1","itemType":"journalArticle","title":"VZ1","collections":["COL1"],"publicationTitle":""}}`),
		json.RawMessage(`{"key":"VZ2","version":1,"data":{"key":"VZ2","itemType":"journalArticle","title":"VZ2","collections":["COL1"],"publicationTitle":"   ","bookTitle":"  ","publisher":" "}}`),
		json.RawMessage(`{"key":"VZ3","version":1,"data":{"key":"VZ3","itemType":"journalArticle","title":"VZ3","collections":["COL1"]}}`),

		// A venue outside the target collection: VO1-VO4 carry it, and VO5
		// belongs to no collection at all. A broken collection-membership
		// test is caught by the full-result length assertion, which expects
		// exactly the four in-collection venues. Rank alone would not catch
		// it: four copies tie with Nature, and a tie has no defined order.
		json.RawMessage(`{"key":"VO1","version":1,"data":{"key":"VO1","itemType":"journalArticle","title":"VO1","collections":["COL2"],"publicationTitle":"Other Library Journal"}}`),
		json.RawMessage(`{"key":"VO2","version":1,"data":{"key":"VO2","itemType":"journalArticle","title":"VO2","collections":["COL2"],"publicationTitle":"Other Library Journal"}}`),
		json.RawMessage(`{"key":"VO3","version":1,"data":{"key":"VO3","itemType":"journalArticle","title":"VO3","collections":["COL2"],"publicationTitle":"Other Library Journal"}}`),
		json.RawMessage(`{"key":"VO4","version":1,"data":{"key":"VO4","itemType":"journalArticle","title":"VO4","collections":["COL2"],"publicationTitle":"Other Library Journal"}}`),
		json.RawMessage(`{"key":"VO5","version":1,"data":{"key":"VO5","itemType":"journalArticle","title":"VO5","collections":[]}}`),

		// Ineligible item types in COL1: collectionStatsEligibleItemPredicate
		// rejects item_type IN ('attachment','note','annotation'). Five notes,
		// so a broken predicate would rank "Notes Digest" first.
		json.RawMessage(`{"key":"VN1","version":1,"data":{"key":"VN1","itemType":"note","note":"n1","collections":["COL1"],"publicationTitle":"Notes Digest"}}`),
		json.RawMessage(`{"key":"VN2","version":1,"data":{"key":"VN2","itemType":"note","note":"n2","collections":["COL1"],"publicationTitle":"Notes Digest"}}`),
		json.RawMessage(`{"key":"VN3","version":1,"data":{"key":"VN3","itemType":"note","note":"n3","collections":["COL1"],"publicationTitle":"Notes Digest"}}`),
		json.RawMessage(`{"key":"VN4","version":1,"data":{"key":"VN4","itemType":"note","note":"n4","collections":["COL1"],"publicationTitle":"Notes Digest"}}`),
		json.RawMessage(`{"key":"VN5","version":1,"data":{"key":"VN5","itemType":"note","note":"n5","collections":["COL1"],"publicationTitle":"Notes Digest"}}`),
		json.RawMessage(`{"key":"VA1","version":1,"data":{"key":"VA1","itemType":"attachment","parentItem":"VJ1","contentType":"application/pdf","collections":["COL1"],"publicationTitle":"Attachment Venue"}}`),
		json.RawMessage(`{"key":"VAN1","version":1,"data":{"key":"VAN1","itemType":"annotation","parentItem":"VA1","annotationText":"a1","collections":["COL1"],"publicationTitle":"Annotation Venue"}}`),
	}
	db := seedStoreWithItems(t, items)

	type venueCount struct {
		venue string
		count int64
	}
	read := func(t *testing.T, top int) []venueCount {
		t.Helper()
		rows, err := queryCollectionTopVenues(db, "COL1", top)
		if err != nil {
			t.Fatalf("queryCollectionTopVenues(top=%d): %v", top, err)
		}
		out := make([]venueCount, 0, len(rows))
		for i, row := range rows {
			venue, ok := row["venue"].(string)
			if !ok {
				t.Fatalf("row %d venue = %#v, want a string; HAVING venue IS NOT NULL must drop venueless items", i, row["venue"])
			}
			if venue == "" {
				t.Fatalf("row %d venue is empty; HAVING venue != '' must drop empty and whitespace-only venues", i)
			}
			n, err := toInt64(row["count"])
			if err != nil {
				t.Fatalf("row %d count %#v: %v", i, row["count"], err)
			}
			out = append(out, venueCount{venue: venue, count: n})
		}
		return out
	}

	got := read(t, 10)
	want := []venueCount{
		{venue: "Nature", count: 4},             // publicationTitle beats publisher
		{venue: "Handbook of Optics", count: 3}, // bookTitle beats publisher
		{venue: "MIT Press", count: 2},          // publisher is the last arm
		{venue: "Science", count: 1},            // publicationTitle beats both others
	}
	if len(got) != len(want) {
		t.Fatalf("queryCollectionTopVenues returned %d venues %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("venue %d = %+v, want %+v (COALESCE precedence and ORDER BY count DESC); full result %+v", i, got[i], want[i], got)
		}
	}
	for _, forbidden := range []string{
		"Ignored Springer", "Ignored Elsevier", "Ignored Book", "Ignored Press",
		"Other Library Journal", "Notes Digest", "Attachment Venue", "Annotation Venue",
	} {
		for _, vc := range got {
			if vc.venue == forbidden {
				t.Fatalf("venue %q must never be selected; full result %+v", forbidden, got)
			}
		}
	}

	// LIMIT truncates at a strict count boundary, so the retained venues are
	// deterministic.
	truncated := read(t, 2)
	wantTruncated := []venueCount{
		{venue: "Nature", count: 4},
		{venue: "Handbook of Optics", count: 3},
	}
	if len(truncated) != len(wantTruncated) {
		t.Fatalf("top=2 returned %d venues %+v, want %d %+v", len(truncated), truncated, len(wantTruncated), wantTruncated)
	}
	for i := range wantTruncated {
		if truncated[i] != wantTruncated[i] {
			t.Fatalf("top=2 venue %d = %+v, want %+v (LIMIT must keep the highest counts); full result %+v", i, truncated[i], wantTruncated[i], truncated)
		}
	}
}
