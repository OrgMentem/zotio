// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func mjItem(key, itemType, parent, title, dateAdded string) json.RawMessage {
	parentField := ""
	if parent != "" {
		parentField = fmt.Sprintf(`,"parentItem":%q`, parent)
	}
	contentType := ""
	if itemType == "attachment" {
		contentType = `,"contentType":"application/pdf"`
	}
	return json.RawMessage(fmt.Sprintf(
		`{"key":%q,"version":1,"data":{"key":%q,"itemType":%q,"title":%q,"dateAdded":%q%s%s}}`,
		key, key, itemType, title, dateAdded, parentField, contentType))
}

// Annotations hang off attachments. The pick must rank top-level items by
// the total across all their PDFs, not by the single busiest attachment.
func TestMjWrappedMostAnnotatedRollsUpToItem(t *testing.T) {
	const in, before = "2026-03-01T10:00:00Z", "2025-03-01T10:00:00Z"
	db := seedStoreWithItems(t, []json.RawMessage{
		mjItem("P1", "journalArticle", "", "Two PDFs", in),
		mjItem("A1", "attachment", "P1", "a1.pdf", in),
		mjItem("A2", "attachment", "P1", "a2.pdf", in),
		mjItem("N1", "annotation", "A1", "", in),
		mjItem("N2", "annotation", "A1", "", in),
		mjItem("N3", "annotation", "A2", "", in),
		mjItem("N4", "annotation", "A2", "", in),
		mjItem("P2", "journalArticle", "", "One PDF", in),
		mjItem("B1", "attachment", "P2", "b1.pdf", in),
		mjItem("N5", "annotation", "B1", "", in),
		mjItem("N6", "annotation", "B1", "", in),
		mjItem("N7", "annotation", "B1", "", in),
		// Out-of-year annotations must not tip the ranking to P2.
		mjItem("N8", "annotation", "B1", "", before),
		mjItem("N9", "annotation", "B1", "", before),
	})
	pick, err := queryLibraryWrappedMostAnnotated(db, 2026)
	if err != nil {
		t.Fatalf("queryLibraryWrappedMostAnnotated: %v", err)
	}
	if pick == nil || pick.Key != "P1" || pick.Title != "Two PDFs" || pick.Count != 4 {
		t.Fatalf("pick = %+v, want P1 \"Two PDFs\" with 4 annotations", pick)
	}
}

func TestMjWrappedMostAnnotatedStandaloneAttachment(t *testing.T) {
	const in = "2026-03-01T10:00:00Z"
	db := seedStoreWithItems(t, []json.RawMessage{
		mjItem("SOLO", "attachment", "", "solo.pdf", in),
		mjItem("N1", "annotation", "SOLO", "", in),
		mjItem("N2", "annotation", "SOLO", "", in),
	})
	pick, err := queryLibraryWrappedMostAnnotated(db, 2026)
	if err != nil {
		t.Fatalf("queryLibraryWrappedMostAnnotated: %v", err)
	}
	if pick == nil || pick.Key != "SOLO" || pick.Title != "solo.pdf" || pick.Count != 2 {
		t.Fatalf("pick = %+v, want SOLO \"solo.pdf\" with 2 annotations", pick)
	}
}

func mjRender(t *testing.T, render func(cmd *cobra.Command) error) string {
	t.Helper()
	t.Cleanup(snapshotCLIGlobals())
	noColor, humanFriendly = true, false
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := render(cmd); err != nil {
		t.Fatalf("render: %v", err)
	}
	return out.String()
}

func TestMjPrintLibraryStatsAlignsScalesAndSanitizes(t *testing.T) {
	stats := libraryStats{
		ItemsByType: []libraryTypeCount{{ItemType: "journalArticle", Count: 48}, {ItemType: "book", Count: 2}},
		// AddedBy unset: the added section must be skipped even with rows.
		ItemsAdded:  []libraryAddedCount{{Period: "2026-01", Count: 9}},
		TopVenues:   []libraryVenueCount{{Venue: "Evil\x1b[31mVenue", Count: 3}},
		PDFCoverage: libraryPDFCoverage{TotalItems: 2, ItemsWithPDF: 1, Pct: 50},
	}
	got := mjRender(t, func(cmd *cobra.Command) error { return printLibraryStats(cmd, stats, 5) })

	if strings.Contains(got, "\x1b") {
		t.Errorf("output carries a raw escape byte:\n%q", got)
	}
	if !strings.Contains(got, "Evil\uFFFD[31mVenue") {
		t.Errorf("venue label not sanitized:\n%s", got)
	}
	// Labels pad to the widest label, counts right-align, and bars scale to
	// the section maximum (24 cells) with at least one cell for non-zero.
	for _, want := range []string{
		"journalArticle  48  " + strings.Repeat("▆", 24) + "\n",
		"book" + strings.Repeat(" ", 10) + "   2  ▆\n",
		"PDF Coverage  1/2 (50%)  " + strings.Repeat("▆", 12) + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"Items Added", "Items by Year"} {
		if strings.Contains(got, absent) {
			t.Errorf("output has section %q that should be skipped:\n%s", absent, got)
		}
	}
}

// --added-by selects the period heading; the year window labels its section.
func TestMjPrintLibraryStatsAddedByHeading(t *testing.T) {
	for _, tc := range []struct{ addedBy, want, notWant string }{
		{"month", "Items Added by Month\n", "Items Added by Year"},
		{"year", "Items Added by Year\n", "Items Added by Month"},
	} {
		stats := libraryStats{
			ItemsByYear: []libraryYearCount{{Year: "2026", Count: 1}},
			AddedBy:     tc.addedBy,
			ItemsAdded:  []libraryAddedCount{{Period: "2026", Count: 1}},
		}
		got := mjRender(t, func(cmd *cobra.Command) error { return printLibraryStats(cmd, stats, 3) })
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.notWant) {
			t.Errorf("added-by %s: want heading %q only:\n%s", tc.addedBy, tc.want, got)
		}
		if !strings.Contains(got, "Items by Year (last 3 years)\n") {
			t.Errorf("added-by %s: year section label missing window:\n%s", tc.addedBy, got)
		}
	}
}

func TestMjPrintLibraryPrismaOrdersAndSanitizesSources(t *testing.T) {
	report := libraryPrismaReport{
		By: "all",
		Identified: prismaCorpus{Total: 6, BySource: map[string]int{
			"zeta": 2, "alpha": 2, "unspecified": 1, "Evil\x1b]0;pwn\x07": 1,
		}},
		DuplicateClusters:         1,
		DuplicateRecordsRemoved:   1,
		RecordsAfterDeduplication: 5,
	}
	got := mjRender(t, func(cmd *cobra.Command) error { return printLibraryPrisma(cmd, report) })

	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("output carries raw control bytes:\n%q", got)
	}
	// Sources sort by count descending, then name, independent of map order.
	order := []string{"  alpha: 2\n", "  zeta: 2\n", "  Evil\uFFFD]0;pwn\uFFFD: 1\n", "  unspecified: 1\n"}
	last := -1
	for _, line := range order {
		idx := strings.Index(got, line)
		if idx <= last {
			t.Fatalf("source line %q missing or out of order:\n%s", line, got)
		}
		last = idx
	}
	if !strings.Contains(got, "by doi+title") {
		t.Errorf("--by all should render as doi+title:\n%s", got)
	}
}
