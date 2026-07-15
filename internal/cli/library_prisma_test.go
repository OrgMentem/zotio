// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func seedPrismaStore(t *testing.T, items ...json.RawMessage) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	return localQueryStore{Store: db}
}

func prismaReportForTest(t *testing.T, db localQueryStore, scopeExpr, by string) libraryPrismaReport {
	t.Helper()
	scope := scopeResult{All: true, Expr: "library"}
	if scopeExpr != "library" {
		spec, err := parseScopeSpec(scopeExpr)
		if err != nil {
			t.Fatalf("parse scope %q: %v", scopeExpr, err)
		}
		scope, err = resolveScope(db, spec)
		if err != nil {
			t.Fatalf("resolve scope %q: %v", scopeExpr, err)
		}
	}
	report, err := assembleLibraryPrismaReport(db, scope, by)
	if err != nil {
		t.Fatalf("assemble PRISMA report: %v", err)
	}
	assertPrismaArithmetic(t, report)
	return report
}

func assertPrismaArithmetic(t *testing.T, report libraryPrismaReport) {
	t.Helper()
	if got, want := report.RecordsAfterDeduplication, report.Identified.Total-report.DuplicateRecordsRemoved; got != want {
		t.Errorf("records after deduplication = %d, want identified %d - removed %d = %d", got, report.Identified.Total, report.DuplicateRecordsRemoved, want)
	}
	if report.Prisma.RecordsIdentified != report.Identified.Total || report.Prisma.DuplicateRecordsRemoved != report.DuplicateRecordsRemoved || report.Prisma.RecordsScreenedInput != report.RecordsAfterDeduplication {
		t.Errorf("PRISMA flow payload = %+v, want report totals", report.Prisma)
	}
}

func TestLibraryPrismaCorpusExcludesNonCiteableItemsAndCountsSources(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"One","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"book","title":"Two","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"P3","version":1,"data":{"key":"P3","itemType":"report","title":"Three"}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","title":"Attachment","libraryCatalog":"Scopus"}}`),
		json.RawMessage(`{"key":"N1","version":1,"data":{"key":"N1","itemType":"note","title":"Note","libraryCatalog":"Scopus"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","title":"Annotation","libraryCatalog":"Scopus"}}`),
	)

	report := prismaReportForTest(t, db, "library", "all")
	if report.Identified.Total != 3 {
		t.Errorf("identified total = %d, want 3", report.Identified.Total)
	}
	if got := report.Identified.BySource["PubMed"]; got != 2 {
		t.Errorf("PubMed count = %d, want 2", got)
	}
	if got := report.Identified.BySource["unspecified"]; got != 1 {
		t.Errorf("unspecified count = %d, want 1", got)
	}
	if len(report.Identified.BySource) != 2 {
		t.Errorf("source breakdown = %#v, want PubMed and unspecified only", report.Identified.BySource)
	}
}

func TestLibraryPrismaMergesOverlappingDuplicateDetectors(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Same paper","DOI":"10/example","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Same paper","DOI":"10/example","libraryCatalog":"PubMed"}}`),
	)

	report := prismaReportForTest(t, db, "library", "all")
	if report.DuplicateClusters != 1 || report.DuplicateRecordsRemoved != 1 {
		t.Errorf("duplicate summary = %d clusters, %d removed; want 1, 1", report.DuplicateClusters, report.DuplicateRecordsRemoved)
	}
}

// Regression: a chain where the DOI detector links P1-P2 and the title
// detector links P2-P3 must merge into ONE cluster of three (2 removals) —
// re-rooting P2 while processing the title group used to sever the DOI link.
func TestLibraryPrismaMergesPartiallyOverlappingClusters(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Distinct one","DOI":"10/chain","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Chain title","DOI":"10/chain","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"P3","version":1,"data":{"key":"P3","itemType":"journalArticle","title":"Chain title","DOI":"10/other","libraryCatalog":"Scopus"}}`),
	)

	report := prismaReportForTest(t, db, "library", "all")
	if report.DuplicateClusters != 1 || report.DuplicateRecordsRemoved != 2 {
		t.Errorf("duplicate summary = %d clusters, %d removed; want 1, 2", report.DuplicateClusters, report.DuplicateRecordsRemoved)
	}
	if report.RecordsAfterDeduplication != report.Identified.Total-report.DuplicateRecordsRemoved {
		t.Errorf("after-dedupe invariant violated: %d != %d - %d", report.RecordsAfterDeduplication, report.Identified.Total, report.DuplicateRecordsRemoved)
	}
}

func TestLibraryPrismaSumsDisjointDuplicateClusters(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"D1","version":1,"data":{"key":"D1","itemType":"journalArticle","title":"DOI one","DOI":"10/doi","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"D2","version":1,"data":{"key":"D2","itemType":"journalArticle","title":"DOI two","DOI":"10/doi","libraryCatalog":"PubMed"}}`),
		json.RawMessage(`{"key":"T1","version":1,"data":{"key":"T1","itemType":"journalArticle","title":"Title pair","DOI":"10/title-one","libraryCatalog":"Scopus"}}`),
		json.RawMessage(`{"key":"T2","version":1,"data":{"key":"T2","itemType":"journalArticle","title":"Title pair","DOI":"10/title-two","libraryCatalog":"Scopus"}}`),
	)

	report := prismaReportForTest(t, db, "library", "all")
	if report.DuplicateClusters != 2 || report.DuplicateRecordsRemoved != 2 {
		t.Errorf("duplicate summary = %d clusters, %d removed; want 2, 2", report.DuplicateClusters, report.DuplicateRecordsRemoved)
	}
}

func TestLibraryPrismaScopeRestrictsCorpusAndDuplicates(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"S1","version":1,"data":{"key":"S1","itemType":"journalArticle","title":"Scoped one","DOI":"10/scoped","collections":["C1"]}}`),
		json.RawMessage(`{"key":"S2","version":1,"data":{"key":"S2","itemType":"journalArticle","title":"Scoped two","DOI":"10/scoped","collections":["C1"]}}`),
		json.RawMessage(`{"key":"O1","version":1,"data":{"key":"O1","itemType":"journalArticle","title":"Outside pair","DOI":"10/outside"}}`),
		json.RawMessage(`{"key":"O2","version":1,"data":{"key":"O2","itemType":"journalArticle","title":"Outside pair","DOI":"10/outside"}}`),
	)

	report := prismaReportForTest(t, db, "collection:C1", "all")
	if report.Scope != "collection:C1" || report.Identified.Total != 2 {
		t.Errorf("scoped report = scope %q, %d identified; want collection:C1, 2", report.Scope, report.Identified.Total)
	}
	if report.DuplicateClusters != 1 || report.DuplicateRecordsRemoved != 1 {
		t.Errorf("scoped duplicate summary = %d clusters, %d removed; want 1, 1", report.DuplicateClusters, report.DuplicateRecordsRemoved)
	}
}

func TestLibraryPrismaZeroItemScopeIsAValidReport(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Outside scope"}}`),
	)

	report := prismaReportForTest(t, db, "collection:empty", "all")
	if report.Identified.Total != 0 || report.DuplicateClusters != 0 || report.DuplicateRecordsRemoved != 0 || report.RecordsAfterDeduplication != 0 {
		t.Errorf("zero-item scope report = %+v, want zero counts", report)
	}
	if len(report.Identified.BySource) != 0 {
		t.Errorf("zero-item scope sources = %#v, want empty", report.Identified.BySource)
	}
}

func TestLibraryPrismaByDOIIgnoresTitleGroups(t *testing.T) {
	db := seedPrismaStore(t,
		json.RawMessage(`{"key":"D1","version":1,"data":{"key":"D1","itemType":"journalArticle","title":"DOI one","DOI":"10/doi"}}`),
		json.RawMessage(`{"key":"D2","version":1,"data":{"key":"D2","itemType":"journalArticle","title":"DOI two","DOI":"10/doi"}}`),
		json.RawMessage(`{"key":"T1","version":1,"data":{"key":"T1","itemType":"journalArticle","title":"Title pair","DOI":"10/title-one"}}`),
		json.RawMessage(`{"key":"T2","version":1,"data":{"key":"T2","itemType":"journalArticle","title":"Title pair","DOI":"10/title-two"}}`),
	)

	report := prismaReportForTest(t, db, "library", "doi")
	if report.By != "doi" || report.DuplicateClusters != 1 || report.DuplicateRecordsRemoved != 1 {
		t.Errorf("DOI-only duplicate summary = by %q, %d clusters, %d removed; want doi, 1, 1", report.By, report.DuplicateClusters, report.DuplicateRecordsRemoved)
	}
}

func TestLibraryPrismaRejectsInvalidByAsUsageError(t *testing.T) {
	cmd := newLibraryPrismaCmd(&rootFlags{})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--by", "isbn"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if code := ExitCode(err); code != 2 {
		t.Fatalf("invalid --by exit = %d (%v), want usage exit 2", code, err)
	}
}
