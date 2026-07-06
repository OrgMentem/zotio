// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zotio/internal/store"
)

func TestLibraryWrappedYearScopedReportAndCard(t *testing.T) {
	home := wrappedIsolatedHome(t)
	seedWrappedStore(t)
	cardPath := filepath.Join(home, "wrapped", "card.svg")

	flags := &rootFlags{asJSON: true}
	cmd := newLibraryCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"wrapped", "--year", "2026", "--card", cardPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("library wrapped: %v", err)
	}

	var report libraryWrappedReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode wrapped JSON %q: %v", out.String(), err)
	}
	if report.Year != 2026 {
		t.Fatalf("year = %d, want 2026", report.Year)
	}
	if report.Items.Total != 8 {
		t.Fatalf("items total = %d, want only the eight top-level 2026 items", report.Items.Total)
	}
	if got := wrappedMonthCounts(report.Items.ByMonth); !reflect.DeepEqual(got, map[int]int{1: 2, 2: 2, 3: 1, 4: 1, 5: 1, 6: 1}) {
		t.Fatalf("month counts = %#v, want Jan=2 Feb=2 Mar-Jun=1 and no other 2026 months", got)
	}
	if got, want := wrappedRankNames(report.Items.ByItemType), []string{"journalArticle", "book"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("item type ranking = %#v, want %#v", got, want)
	}
	if got, want := wrappedRankCounts(report.Items.ByItemType), []int{7, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("item type counts = %#v, want %#v", got, want)
	}

	wantVenues := []string{"Alpha & Beta <Journal>", "Journal B", "Delta", "Epsilon", "Gamma"}
	if got := wrappedRankNames(report.TopVenues); !reflect.DeepEqual(got, wantVenues) {
		t.Fatalf("top venue ranking = %#v, want count-desc/name-asc top five %#v", got, wantVenues)
	}
	if got, want := wrappedRankCounts(report.TopVenues), []int{2, 2, 1, 1, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("top venue counts = %#v, want %#v", got, want)
	}
	wantAuthors := []string{"Clark", "Adams", "Baker", "Zed"}
	if got := wrappedRankNames(report.TopAuthors); !reflect.DeepEqual(got, wantAuthors) {
		t.Fatalf("top author ranking = %#v, want count-desc/name-asc %#v", got, wantAuthors)
	}
	if got, want := wrappedRankCounts(report.TopAuthors), []int{3, 2, 2, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("top author counts = %#v, want %#v", got, want)
	}

	if report.PDFCoverage == nil {
		t.Fatal("PDFCoverage is nil, want coverage for 2026 top-level items")
	}
	if got := *report.PDFCoverage; got != (libraryWrappedPDFCoverage{WithAttachment: 2, Total: 8, Percent: 25}) {
		t.Fatalf("PDF coverage = %+v, want 2/8 = 25%%", got)
	}
	if report.Annotations == nil || report.Annotations.Count != 3 || report.Annotations.BusiestMonth == nil || report.Annotations.BusiestMonth.Month != 1 || report.Annotations.BusiestMonth.Count != 2 {
		t.Fatalf("annotations = %+v, want three 2026 annotations with January busiest at two", report.Annotations)
	}
	if report.CardPath != cardPath {
		t.Fatalf("card_path = %q, want %q", report.CardPath, cardPath)
	}

	card, err := os.ReadFile(cardPath)
	if err != nil {
		t.Fatalf("read generated card: %v", err)
	}
	var svg struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal(card, &svg); err != nil {
		t.Fatalf("generated card is not well-formed XML: %v\n%s", err, string(card))
	}
	cardText := string(card)
	if !strings.Contains(cardText, "Alpha &amp; Beta &lt;Journal&gt; (2)") {
		t.Fatalf("card did not XML-escape the top venue text; card = %s", cardText)
	}
	if strings.Contains(cardText, "Alpha & Beta <Journal>") {
		t.Fatalf("card contains unescaped top venue text: %s", cardText)
	}
}

func TestLibraryWrappedZeroItemYearPrintsMessage(t *testing.T) {
	wrappedIsolatedHome(t)
	seedWrappedStore(t)

	flags := &rootFlags{}
	cmd := newLibraryCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"wrapped", "--year", "2024"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("library wrapped zero year: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "No items were added in 2024.") {
		t.Fatalf("zero-item output = %q, want plain no-items message", got)
	}
}

func seedWrappedStore(t *testing.T) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Escaping <Title> & Friends","dateAdded":"2026-02-03T10:00:00Z","publicationTitle":"Alpha & Beta <Journal>","creators":[{"lastName":"Zed"}]}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"P2","dateAdded":"2026-02-04T10:00:00Z","publicationTitle":"Journal B","creators":[{"lastName":"Adams"}]}}`),
		json.RawMessage(`{"key":"P3","version":1,"data":{"key":"P3","itemType":"book","title":"P3","dateAdded":"2026-03-05T10:00:00Z","publicationTitle":"Journal B","creators":[{"lastName":"Adams"}]}}`),
		json.RawMessage(`{"key":"P4","version":1,"data":{"key":"P4","itemType":"journalArticle","title":"P4","dateAdded":"2026-01-05T10:00:00Z","publicationTitle":"Alpha & Beta <Journal>","creators":[{"lastName":"Baker"}]}}`),
		json.RawMessage(`{"key":"P5","version":1,"data":{"key":"P5","itemType":"journalArticle","title":"P5","dateAdded":"2026-01-06T10:00:00Z","publicationTitle":"Gamma","creators":[{"lastName":"Baker"}]}}`),
		json.RawMessage(`{"key":"P6","version":1,"data":{"key":"P6","itemType":"journalArticle","title":"P6","dateAdded":"2026-04-06T10:00:00Z","publicationTitle":"Delta","creators":[{"lastName":"Clark"}]}}`),
		json.RawMessage(`{"key":"P7","version":1,"data":{"key":"P7","itemType":"journalArticle","title":"P7","dateAdded":"2026-05-06T10:00:00Z","publicationTitle":"Epsilon","creators":[{"lastName":"Clark"}]}}`),
		json.RawMessage(`{"key":"P8","version":1,"data":{"key":"P8","itemType":"journalArticle","title":"P8","dateAdded":"2026-06-06T10:00:00Z","publicationTitle":"Zeta","creators":[{"lastName":"Clark"}]}}`),
		json.RawMessage(`{"key":"A1","version":1,"data":{"key":"A1","itemType":"attachment","parentItem":"P1","contentType":"application/pdf","dateAdded":"2026-02-03T11:00:00Z"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","contentType":"application/pdf","dateAdded":"2026-02-04T11:00:00Z"}}`),
		json.RawMessage(`{"key":"N1","version":1,"data":{"key":"N1","itemType":"annotation","parentItem":"A1","annotationType":"highlight","dateAdded":"2026-01-07T10:00:00Z"}}`),
		json.RawMessage(`{"key":"N2","version":1,"data":{"key":"N2","itemType":"annotation","parentItem":"A1","annotationType":"note","dateAdded":"2026-01-08T10:00:00Z"}}`),
		json.RawMessage(`{"key":"N3","version":1,"data":{"key":"N3","itemType":"annotation","parentItem":"A2","annotationType":"highlight","dateAdded":"2026-03-08T10:00:00Z"}}`),
		json.RawMessage(`{"key":"OLD","version":1,"data":{"key":"OLD","itemType":"journalArticle","title":"Old","dateAdded":"2025-02-03T10:00:00Z","publicationTitle":"Old Journal","creators":[{"lastName":"Old"}]}}`),
		json.RawMessage(`{"key":"OLDPDF","version":1,"data":{"key":"OLDPDF","itemType":"attachment","parentItem":"OLD","contentType":"application/pdf","dateAdded":"2025-02-03T11:00:00Z"}}`),
		json.RawMessage(`{"key":"OLDANN","version":1,"data":{"key":"OLDANN","itemType":"annotation","parentItem":"OLDPDF","annotationType":"highlight","dateAdded":"2025-02-03T12:00:00Z"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed wrapped items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close wrapped store: %v", err)
	}
}

func wrappedIsolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	return home
}

func wrappedMonthCounts(months []libraryWrappedMonthCount) map[int]int {
	out := map[int]int{}
	for _, month := range months {
		if month.Count > 0 {
			out[month.Month] = month.Count
		}
	}
	return out
}

func wrappedRankNames(rows []libraryWrappedRankedCount) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Name)
	}
	return out
}

func wrappedRankCounts(rows []libraryWrappedRankedCount) []int {
	out := make([]int, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Count)
	}
	return out
}
