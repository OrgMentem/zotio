// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(marketing-heroes-2): add a local-only year-in-review command for library marketing heroes.

package cli

import (
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type libraryWrappedReport struct {
	Year        int                         `json:"year"`
	Items       libraryWrappedItemSummary   `json:"items"`
	TopVenues   []libraryWrappedRankedCount `json:"top_venues,omitempty"`
	TopAuthors  []libraryWrappedRankedCount `json:"top_authors,omitempty"`
	Annotations *libraryWrappedAnnotations  `json:"annotations,omitempty"`
	PDFCoverage *libraryWrappedPDFCoverage  `json:"pdf_coverage,omitempty"`
	CardPath    string                      `json:"card_path,omitempty"`
}

type libraryWrappedItemSummary struct {
	Total      int                         `json:"total"`
	ByMonth    []libraryWrappedMonthCount  `json:"by_month,omitempty"`
	ByItemType []libraryWrappedRankedCount `json:"by_item_type,omitempty"`
}

type libraryWrappedMonthCount struct {
	Month int    `json:"month"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type libraryWrappedRankedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type libraryWrappedAnnotations struct {
	Count        int                       `json:"count"`
	BusiestMonth *libraryWrappedMonthCount `json:"busiest_month,omitempty"`
}

type libraryWrappedPDFCoverage struct {
	WithAttachment int `json:"with_attachment"`
	Total          int `json:"total"`
	Percent        int `json:"percent"`
}

func newLibraryWrappedCmd(flags *rootFlags) *cobra.Command {
	flagYear := time.Now().Year()
	var flagCard string

	cmd := &cobra.Command{
		Use:         "wrapped",
		Short:       "Show a local year-in-review for your Zotero library",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagYear < 1 || flagYear > 9999 {
				return usageErr(fmt.Errorf("--year must be between 1 and 9999"))
			}
			if strings.TrimSpace(flagCard) == "" && cmd.Flags().Changed("card") {
				return usageErr(fmt.Errorf("--card requires a non-empty path"))
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			totalStored, err := queryLibraryWrappedStoredItemCount(db)
			if err != nil {
				return fmt.Errorf("checking local library: %w", err)
			}
			if totalStored == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}

			report, err := queryLibraryWrappedReport(db, flagYear)
			if err != nil {
				return fmt.Errorf("querying wrapped report: %w", err)
			}
			if flagCard != "" {
				if err := writeLibraryWrappedCard(flagCard, report); err != nil {
					return err
				}
				report.CardPath = flagCard
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			return printLibraryWrapped(cmd, report)
		},
	}
	cmd.Flags().IntVar(&flagYear, "year", flagYear, "Year to summarize")
	cmd.Flags().StringVar(&flagCard, "card", "", "Write an 800x418 SVG share card to this path")
	return cmd
}

// PATCH(marketing-heroes-2): keep all year-in-review reads local and scoped to top-level Zotero items.
func queryLibraryWrappedReport(db localQueryStore, year int) (libraryWrappedReport, error) {
	items, err := queryLibraryWrappedItems(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report := libraryWrappedReport{Year: year, Items: items}
	if items.Total == 0 {
		return report, nil
	}

	venues, err := queryLibraryWrappedTopVenues(db, year, 5)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.TopVenues = venues

	authors, err := queryLibraryWrappedTopAuthors(db, year, 5)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.TopAuthors = authors

	annotations, err := queryLibraryWrappedAnnotations(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	report.Annotations = annotations

	coverage, err := queryLibraryWrappedPDFCoverage(db, year)
	if err != nil {
		return libraryWrappedReport{}, err
	}
	if coverage.Total > 0 {
		report.PDFCoverage = &coverage
	}
	return report, nil
}

func queryLibraryWrappedStoredItemCount(db localQueryStore) (int, error) {
	rows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')`)
	if err != nil {
		return 0, err
	}
	return firstCount(rows), nil
}

func queryLibraryWrappedItems(db localQueryStore, year int) (libraryWrappedItemSummary, error) {
	monthRows, err := db.QueryRaw(`
SELECT CAST(SUBSTR(json_extract(data,'$.data.dateAdded'), 6, 2) AS INTEGER) AS month, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
GROUP BY month
ORDER BY month`, fmt.Sprintf("%04d", year))
	if err != nil {
		return libraryWrappedItemSummary{}, err
	}

	byMonth := make([]libraryWrappedMonthCount, 12)
	total := 0
	for i := range byMonth {
		byMonth[i] = libraryWrappedMonthCount{Month: i + 1, Name: shortMonthName(i + 1)}
	}
	for _, row := range monthRows {
		month := sqlIntValue(row["month"])
		if month < 1 || month > 12 {
			continue
		}
		count := sqlIntValue(row["count"])
		byMonth[month-1].Count = count
		total += count
	}

	typeRows, err := db.QueryRaw(`
SELECT COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), 'unknown') AS name, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
GROUP BY name
ORDER BY count DESC, name ASC`, fmt.Sprintf("%04d", year))
	if err != nil {
		return libraryWrappedItemSummary{}, err
	}

	return libraryWrappedItemSummary{
		Total:      total,
		ByMonth:    byMonth,
		ByItemType: rankedRows(typeRows),
	}, nil
}

func queryLibraryWrappedTopVenues(db localQueryStore, year, limit int) ([]libraryWrappedRankedCount, error) {
	rows, err := db.QueryRaw(`
SELECT TRIM(json_extract(data,'$.data.publicationTitle')) AS name, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
	AND NULLIF(TRIM(json_extract(data,'$.data.publicationTitle')), '') IS NOT NULL
GROUP BY name
ORDER BY count DESC, name ASC
LIMIT ?`, fmt.Sprintf("%04d", year), limit)
	if err != nil {
		return nil, err
	}
	return rankedRows(rows), nil
}

func queryLibraryWrappedTopAuthors(db localQueryStore, year, limit int) ([]libraryWrappedRankedCount, error) {
	rows, err := db.QueryRaw(`
SELECT COALESCE(
	NULLIF(TRIM(json_extract(data,'$.data.creators[0].lastName')), ''),
	NULLIF(TRIM(json_extract(data,'$.data.creators[0].name')), '')
) AS name, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
	AND name IS NOT NULL
GROUP BY name
ORDER BY count DESC, name ASC
LIMIT ?`, fmt.Sprintf("%04d", year), limit)
	if err != nil {
		return nil, err
	}
	return rankedRows(rows), nil
}

func queryLibraryWrappedAnnotations(db localQueryStore, year int) (*libraryWrappedAnnotations, error) {
	allRows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') = 'annotation'`)
	if err != nil {
		return nil, err
	}
	if firstCount(allRows) == 0 {
		return nil, nil
	}

	rows, err := db.QueryRaw(`
SELECT CAST(SUBSTR(json_extract(data,'$.data.dateAdded'), 6, 2) AS INTEGER) AS month, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND COALESCE(NULLIF(item_type,''), json_extract(data,'$.data.itemType'), '') = 'annotation'
	AND SUBSTR(COALESCE(json_extract(data,'$.data.dateAdded'),''), 1, 4) = ?
GROUP BY month
ORDER BY count DESC, month ASC`, fmt.Sprintf("%04d", year))
	if err != nil {
		return nil, err
	}

	out := &libraryWrappedAnnotations{}
	for _, row := range rows {
		month := sqlIntValue(row["month"])
		if month < 1 || month > 12 {
			continue
		}
		count := sqlIntValue(row["count"])
		out.Count += count
		if out.BusiestMonth == nil || count > out.BusiestMonth.Count {
			out.BusiestMonth = &libraryWrappedMonthCount{Month: month, Name: shortMonthName(month), Count: count}
		}
	}
	return out, nil
}

func queryLibraryWrappedPDFCoverage(db localQueryStore, year int) (libraryWrappedPDFCoverage, error) {
	rows, err := db.QueryRaw(`
SELECT
	COUNT(DISTINCT i.id) AS total_items,
	COUNT(DISTINCT CASE WHEN a.id IS NOT NULL THEN i.id END) AS items_with_pdf
FROM resources i
LEFT JOIN resources a ON
	a.resource_type='items'
	AND COALESCE(NULLIF(a.item_type,''), json_extract(a.data,'$.data.itemType'), '') = 'attachment'
	AND json_extract(a.data,'$.data.contentType') = 'application/pdf'
	AND (a.parent_key = i.id OR json_extract(a.data,'$.data.parentItem') = i.id)
WHERE i.resource_type='items'
	AND COALESCE(NULLIF(i.item_type,''), json_extract(i.data,'$.data.itemType'), '') NOT IN ('attachment','note','annotation')
	AND SUBSTR(COALESCE(json_extract(i.data,'$.data.dateAdded'),''), 1, 4) = ?`, fmt.Sprintf("%04d", year))
	if err != nil {
		return libraryWrappedPDFCoverage{}, err
	}
	if len(rows) == 0 {
		return libraryWrappedPDFCoverage{}, nil
	}
	coverage := libraryWrappedPDFCoverage{
		WithAttachment: sqlIntValue(rows[0]["items_with_pdf"]),
		Total:          sqlIntValue(rows[0]["total_items"]),
	}
	if coverage.Total > 0 {
		coverage.Percent = coverage.WithAttachment * 100 / coverage.Total
	}
	return coverage, nil
}

func rankedRows(rows []map[string]any) []libraryWrappedRankedCount {
	out := make([]libraryWrappedRankedCount, 0, len(rows))
	for _, row := range rows {
		name := strings.TrimSpace(sqlStringValue(row["name"]))
		if name == "" {
			continue
		}
		out = append(out, libraryWrappedRankedCount{Name: name, Count: sqlIntValue(row["count"])})
	}
	return out
}

// PATCH(marketing-heroes-2): render an honest local-data summary without implying unavailable sections exist.
func printLibraryWrapped(cmd *cobra.Command, report libraryWrappedReport) error {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Zotio Wrapped %d\n", report.Year)
	fmt.Fprintln(w, "==================")
	fmt.Fprintln(w, "Your Zotero year, counted from the local synced store.")
	fmt.Fprintln(w)

	if report.Items.Total == 0 {
		fmt.Fprintf(w, "No items were added in %d.\n", report.Year)
		if report.CardPath != "" {
			fmt.Fprintf(w, "SVG card written: %s\n", report.CardPath)
		}
		return nil
	}

	fmt.Fprintf(w, "Items added:       %d\n", report.Items.Total)
	if report.PDFCoverage != nil {
		fmt.Fprintf(w, "PDF coverage:      %d/%d (%d%%)\n", report.PDFCoverage.WithAttachment, report.PDFCoverage.Total, report.PDFCoverage.Percent)
	}
	if report.Annotations != nil {
		busiest := "n/a"
		if report.Annotations.BusiestMonth != nil {
			busiest = fmt.Sprintf("%s (%d)", report.Annotations.BusiestMonth.Name, report.Annotations.BusiestMonth.Count)
		}
		fmt.Fprintf(w, "Annotations made:  %d; busiest month: %s\n", report.Annotations.Count, busiest)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Items by Month")
	if err := printWrappedBars(w, report.Items.ByMonth, wrappedMaxMonthCount(report.Items.ByMonth)); err != nil {
		return err
	}
	fmt.Fprintln(w)

	if len(report.Items.ByItemType) > 0 {
		fmt.Fprintln(w, "Item Types")
		if err := printWrappedRankedBars(w, report.Items.ByItemType); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	if len(report.TopVenues) > 0 {
		fmt.Fprintln(w, "Top Venues")
		if err := printWrappedRankedBars(w, report.TopVenues); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	if len(report.TopAuthors) > 0 {
		fmt.Fprintln(w, "Top First Creators")
		if err := printWrappedRankedBars(w, report.TopAuthors); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	if report.CardPath != "" {
		fmt.Fprintf(w, "SVG card written: %s\n", report.CardPath)
	}
	return nil
}

func printWrappedBars(w interface{ Write([]byte) (int, error) }, months []libraryWrappedMonthCount, maxCount int) error {
	tw := newTabWriter(w)
	for _, month := range months {
		fmt.Fprintf(tw, "%s\t%4d\t%s\n", month.Name, month.Count, wrappedBar(month.Count, maxCount, 22))
	}
	return tw.Flush()
}

func printWrappedRankedBars(w interface{ Write([]byte) (int, error) }, rows []libraryWrappedRankedCount) error {
	tw := newTabWriter(w)
	maxCount := 0
	for _, row := range rows {
		if row.Count > maxCount {
			maxCount = row.Count
		}
	}
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%4d\t%s\n", truncate(row.Name, 42), row.Count, wrappedBar(row.Count, maxCount, 22))
	}
	return tw.Flush()
}

func wrappedBar(count, maxCount, width int) string {
	if count <= 0 || maxCount <= 0 || width <= 0 {
		return ""
	}
	n := int(math.Round(float64(count) / float64(maxCount) * float64(width)))
	if n < 1 {
		n = 1
	}
	return strings.Repeat("█", n)
}

func wrappedMaxMonthCount(months []libraryWrappedMonthCount) int {
	maxCount := 0
	for _, month := range months {
		if month.Count > maxCount {
			maxCount = month.Count
		}
	}
	return maxCount
}

func shortMonthName(month int) string {
	if month < 1 || month > 12 {
		return ""
	}
	return time.Month(month).String()[:3]
}

// PATCH(marketing-heroes-2): generate a dependency-free SVG card with escaped local metadata.
func writeLibraryWrappedCard(path string, report libraryWrappedReport) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating card directory: %w", err)
		}
	}
	svg := renderLibraryWrappedSVG(report)
	// #nosec G306 -- the SVG card is a user-requested shareable artifact, not a secret.
	if err := os.WriteFile(path, []byte(svg), 0o644); err != nil {
		return fmt.Errorf("writing SVG card: %w", err)
	}
	return nil
}

func renderLibraryWrappedSVG(report libraryWrappedReport) string {
	topVenue := "No venue data"
	if len(report.TopVenues) > 0 {
		topVenue = fmt.Sprintf("%s (%d)", report.TopVenues[0].Name, report.TopVenues[0].Count)
	}
	topAuthor := "No creator data"
	if len(report.TopAuthors) > 0 {
		topAuthor = fmt.Sprintf("%s (%d)", report.TopAuthors[0].Name, report.TopAuthors[0].Count)
	}
	pdfLine := "PDF coverage not available"
	if report.PDFCoverage != nil {
		pdfLine = fmt.Sprintf("%d/%d with PDFs", report.PDFCoverage.WithAttachment, report.PDFCoverage.Total)
	}
	annotationLine := "Annotations not synced locally"
	if report.Annotations != nil {
		annotationLine = fmt.Sprintf("%d annotations", report.Annotations.Count)
		if report.Annotations.BusiestMonth != nil {
			annotationLine += fmt.Sprintf(", busiest in %s", report.Annotations.BusiestMonth.Name)
		}
	}
	monthBars := wrappedSVGMonthBars(report.Items.ByMonth)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" width="800" height="418" viewBox="0 0 800 418" role="img" aria-labelledby="title desc">
  <title id="title">zotio wrapped %d</title>
  <desc id="desc">A local Zotero year in review: %d items added in %d.</desc>
  <rect width="800" height="418" rx="28" fill="#101827"/>
  <circle cx="680" cy="62" r="116" fill="#24324b" opacity="0.62"/>
  <circle cx="104" cy="360" r="142" fill="#1f3a4d" opacity="0.52"/>
  <text x="54" y="64" fill="#e5edf7" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="30" font-weight="700">zotio</text>
  <text x="54" y="101" fill="#8fb4d9" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="18">Wrapped %d</text>
  <text x="54" y="172" fill="#ffffff" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="72" font-weight="800">%d</text>
  <text x="54" y="205" fill="#c7d7ea" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="24">items added locally</text>
  <text x="54" y="260" fill="#e5edf7" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="20" font-weight="700">Top venue</text>
  <text x="54" y="289" fill="#9fb7d3" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="18">%s</text>
  <text x="54" y="331" fill="#e5edf7" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="20" font-weight="700">Top first creator</text>
  <text x="54" y="360" fill="#9fb7d3" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="18">%s</text>
  <rect x="450" y="118" width="296" height="224" rx="22" fill="#172338" stroke="#2e4566"/>
  <text x="480" y="158" fill="#e5edf7" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="20" font-weight="700">Monthly rhythm</text>
  %s
  <text x="480" y="306" fill="#9fb7d3" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="16">%s</text>
  <text x="480" y="331" fill="#9fb7d3" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="16">%s</text>
</svg>
`, report.Year, report.Items.Total, report.Year, report.Year, report.Items.Total, svgText(topVenue), svgText(topAuthor), monthBars, svgText(pdfLine), svgText(annotationLine))
}

func wrappedSVGMonthBars(months []libraryWrappedMonthCount) string {
	maxCount := wrappedMaxMonthCount(months)
	if len(months) == 0 || maxCount == 0 {
		return `<text x="480" y="214" fill="#9fb7d3" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="16">No monthly items for this year</text>`
	}
	var b strings.Builder
	for i, month := range months {
		x := 480 + i*21
		height := 8 + int(math.Round(float64(month.Count)/float64(maxCount)*82))
		y := 268 - height
		fmt.Fprintf(&b, `  <rect x="%d" y="%d" width="12" height="%d" rx="4" fill="#6bc7ff"/>`+"\n", x, y, height)
		fmt.Fprintf(&b, `  <text x="%d" y="286" fill="#6f89a8" font-family="Inter, ui-sans-serif, system-ui, sans-serif" font-size="9">%s</text>`+"\n", x-1, svgText(month.Name[:1]))
	}
	return b.String()
}

func svgText(s string) string {
	return html.EscapeString(strings.TrimSpace(s))
}
