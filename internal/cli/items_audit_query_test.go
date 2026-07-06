// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean perf-audit m4ku/2qhf): cover the indexed-column missing-PDF query
// and the single-scan audit summary so the json_extract -> indexed-column and
// multi-scan -> conditional-aggregation rewrites stay behavior-preserving.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
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

	rows, err := queryMissingPDFItems(db, "", 0, "")
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
	books, err := queryMissingPDFItems(db, "book", 0, "")
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

func seedCitationStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		// C1: complete journalArticle (creators+title+date+publicationTitle).
		json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","itemType":"journalArticle","title":"Complete","creators":[{"lastName":"A","creatorType":"author"}],"date":"2020","publicationTitle":"J"}}`),
		// C2: journalArticle with only a title.
		json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","itemType":"journalArticle","title":"OnlyTitle"}}`),
		// C3: book with creators+title+date but no publisher.
		json.RawMessage(`{"key":"C3","version":1,"data":{"key":"C3","itemType":"book","title":"Bk","creators":[{"lastName":"B","creatorType":"author"}],"date":"2019"}}`),
		// C4: attachment (not citeable; excluded).
		json.RawMessage(`{"key":"C4","version":1,"data":{"key":"C4","itemType":"attachment","parentItem":"C1","contentType":"application/pdf"}}`),
		// C5: webpage with core fields; no type-specific venue requirement.
		json.RawMessage(`{"key":"C5","version":1,"data":{"key":"C5","itemType":"webpage","title":"W","creators":[{"lastName":"C","creatorType":"author"}],"date":"2021"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return localQueryStore{db}
}

func TestCitationAudit(t *testing.T) {
	db := seedCitationStore(t)

	rows, err := queryCitationIncompleteItems(db, 0)
	if err != nil {
		t.Fatalf("queryCitationIncompleteItems: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[sqlStringValue(r["key"])] = sqlStringValue(r["missing"])
	}
	for _, k := range []string{"C1", "C4", "C5"} {
		if m, bad := got[k]; bad {
			t.Errorf("%s should be citation-complete/excluded, got missing=%q", k, m)
		}
	}
	if m := got["C2"]; !strings.Contains(m, "creators") || !strings.Contains(m, "date") || !strings.Contains(m, "publicationTitle") {
		t.Errorf("C2 missing = %q, want creators+date+publicationTitle", m)
	}
	if m := got["C3"]; m != "publisher" {
		t.Errorf("C3 missing = %q, want 'publisher'", m)
	}
	if len(got) != 2 {
		t.Errorf("citation-incomplete count = %d, want 2 (%v)", len(got), got)
	}

	summary, err := queryItemsAuditSummary(db)
	if err != nil {
		t.Fatalf("queryItemsAuditSummary: %v", err)
	}
	if summary.MissingCitation != 2 {
		t.Errorf("summary.MissingCitation = %d, want 2", summary.MissingCitation)
	}
}

func TestQueryPDFAttachments(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(`{"key":"AT1","version":1,"data":{"key":"AT1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf","linkMode":"imported_file","filename":"a.pdf"}}`),
		json.RawMessage(`{"key":"AT2","version":1,"data":{"key":"AT2","itemType":"attachment","parentItem":"P1","contentType":"application/pdf","linkMode":"linked_url"}}`),
		json.RawMessage(`{"key":"AT3","version":1,"data":{"key":"AT3","itemType":"attachment","parentItem":"P2","contentType":"text/html","linkMode":"imported_url"}}`),
		json.RawMessage(`{"key":"AT4","version":1,"data":{"key":"AT4","itemType":"attachment","parentItem":"P2","contentType":"application/pdf","linkMode":"linked_file","filename":"b.pdf"}}`),
		json.RawMessage(`{"key":"IT1","version":1,"data":{"key":"IT1","itemType":"journalArticle","title":"T"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rows, err := queryPDFAttachments(localQueryStore{db}, 0)
	if err != nil {
		t.Fatalf("queryPDFAttachments: %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[sqlStringValue(r["key"])] = true
	}
	if !got["AT1"] || !got["AT4"] {
		t.Errorf("want AT1 (imported_file) and AT4 (linked_file), got %v", got)
	}
	if got["AT2"] || got["AT3"] || got["IT1"] {
		t.Errorf("excluded a linked_url/non-pdf/non-attachment incorrectly: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("count = %d, want 2 (%v)", len(got), got)
	}
}
