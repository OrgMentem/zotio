// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type libraryStats struct {
	ItemsByType []libraryTypeCount  `json:"items_by_type"`
	ItemsByYear []libraryYearCount  `json:"items_by_year"`
	AddedBy     string              `json:"added_by,omitempty"`
	ItemsAdded  []libraryAddedCount `json:"items_added,omitempty"`
	TopVenues   []libraryVenueCount `json:"top_venues"`
	PDFCoverage libraryPDFCoverage  `json:"pdf_coverage"`
}

type libraryTypeCount struct {
	ItemType string `json:"item_type"`
	Count    int    `json:"count"`
}

type libraryYearCount struct {
	Year  string `json:"year"`
	Count int    `json:"count"`
}

type libraryAddedCount struct {
	Period string `json:"period"`
	Count  int    `json:"count"`
}

type libraryVenueCount struct {
	Venue string `json:"venue"`
	Count int    `json:"count"`
}

type libraryPDFCoverage struct {
	TotalItems   int `json:"total_items"`
	ItemsWithPDF int `json:"items_with_pdf"`
	Pct          int `json:"pct"`
}

func newLibraryStatsCmd(flags *rootFlags) *cobra.Command {
	var flagTop int
	var flagYears int
	var flagAddedBy string

	cmd := &cobra.Command{
		Use:         "stats",
		Short:       "Show library-wide local statistics",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagTop < 0 {
				return fmt.Errorf("--top must be zero or greater")
			}
			if flagYears < 0 {
				return fmt.Errorf("--years must be zero or greater")
			}
			addedBy := strings.ToLower(strings.TrimSpace(flagAddedBy))
			if addedBy != "" && addedBy != "month" && addedBy != "year" {
				return fmt.Errorf("--added-by must be month or year")
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			stats, err := queryLibraryStats(db, flagTop, flagYears, addedBy)
			if err != nil {
				return fmt.Errorf("querying library statistics: %w", err)
			}
			if flags.asJSON {
				data, err := json.Marshal(stats)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			return printLibraryStats(cmd, stats, flagYears)
		},
	}
	cmd.Flags().IntVar(&flagTop, "top", 10, "Number of venues to show")
	cmd.Flags().IntVar(&flagYears, "years", 20, "Number of years to show")
	cmd.Flags().StringVar(&flagAddedBy, "added-by", "", "Group item intake by month or year")

	return cmd
}

func queryLibraryStats(db localQueryStore, top, years int, addedBy string) (libraryStats, error) {
	itemsByType, err := queryLibraryItemsByType(db)
	if err != nil {
		return libraryStats{}, err
	}
	itemsByYear, err := queryLibraryItemsByYear(db, years)
	if err != nil {
		return libraryStats{}, err
	}
	itemsAdded, err := queryLibraryItemsAdded(db, addedBy)
	if err != nil {
		return libraryStats{}, err
	}
	topVenues, err := queryLibraryTopVenues(db, top)
	if err != nil {
		return libraryStats{}, err
	}
	pdfCoverage, err := queryLibraryPDFCoverage(db)
	if err != nil {
		return libraryStats{}, err
	}
	return libraryStats{
		ItemsByType: itemsByType,
		ItemsByYear: itemsByYear,
		AddedBy:     addedBy,
		ItemsAdded:  itemsAdded,
		TopVenues:   topVenues,
		PDFCoverage: pdfCoverage,
	}, nil
}

func queryLibraryItemsByType(db localQueryStore) ([]libraryTypeCount, error) {
	rows, err := db.QueryRaw(`
SELECT json_extract(data,'$.data.itemType') AS item_type, COUNT(*) AS count
FROM resources WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')
GROUP BY item_type ORDER BY count DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]libraryTypeCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, libraryTypeCount{
			ItemType: sqlStringValue(row["item_type"]),
			Count:    sqlIntValue(row["count"]),
		})
	}
	return out, nil
}

func queryLibraryItemsByYear(db localQueryStore, years int) ([]libraryYearCount, error) {
	rows, err := db.QueryRaw(`
SELECT
	SUBSTR(COALESCE(json_extract(data,'$.data.date'),''), 1, 4) AS year,
	COUNT(*) AS count
FROM resources WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')
	AND year != '' AND year GLOB '[12][0-9][0-9][0-9]'
GROUP BY year ORDER BY year DESC LIMIT ?`, years)
	if err != nil {
		return nil, err
	}
	out := make([]libraryYearCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, libraryYearCount{
			Year:  sqlStringValue(row["year"]),
			Count: sqlIntValue(row["count"]),
		})
	}
	return out, nil
}

func queryLibraryItemsAdded(db localQueryStore, addedBy string) ([]libraryAddedCount, error) {
	if addedBy == "" {
		return nil, nil
	}
	const dateAddedExpression = `TRIM(COALESCE(json_extract(data,'$.data.dateAdded'),''))`
	var periodExpression string
	switch addedBy {
	case "month":
		periodExpression = `SUBSTR(` + dateAddedExpression + `, 1, 7)`
	case "year":
		periodExpression = `SUBSTR(` + dateAddedExpression + `, 1, 4)`
	default:
		return nil, fmt.Errorf("unsupported item-intake bucket %q", addedBy)
	}
	rows, err := db.QueryRaw(`
SELECT ` + periodExpression + ` AS period, COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND ` + libraryTopLevelItemsPredicate + `
	AND LENGTH(` + dateAddedExpression + `) = 20
	AND strftime('%Y-%m-%dT%H:%M:%SZ', ` + dateAddedExpression + `) = ` + dateAddedExpression + `
GROUP BY period
ORDER BY period DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]libraryAddedCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, libraryAddedCount{
			Period: sqlStringValue(row["period"]),
			Count:  sqlIntValue(row["count"]),
		})
	}
	return out, nil
}

func queryLibraryTopVenues(db localQueryStore, top int) ([]libraryVenueCount, error) {
	rows, err := db.QueryRaw(`
SELECT
	COALESCE(
		NULLIF(TRIM(json_extract(data,'$.data.publicationTitle')),''),
		NULLIF(TRIM(json_extract(data,'$.data.bookTitle')),''),
		NULLIF(TRIM(json_extract(data,'$.data.publisher')),''),
		'Unknown'
	) AS venue,
	COUNT(*) AS count
FROM resources WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')
GROUP BY venue HAVING venue != 'Unknown'
ORDER BY count DESC LIMIT ?`, top)
	if err != nil {
		return nil, err
	}
	out := make([]libraryVenueCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, libraryVenueCount{
			Venue: sqlStringValue(row["venue"]),
			Count: sqlIntValue(row["count"]),
		})
	}
	return out, nil
}

func queryLibraryPDFCoverage(db localQueryStore) (libraryPDFCoverage, error) {
	// Count distinct parent items with at least one PDF, not distinct
	// attachments. LEFT JOIN + CASE guard ensures items with zero PDFs
	// are excluded from the numerator (see library_wrapped.go:313).
	rows, err := db.QueryRaw(`
SELECT
	COUNT(DISTINCT i.id) AS total_items,
	COUNT(DISTINCT CASE WHEN a.id IS NOT NULL THEN i.id END) AS items_with_pdf
FROM resources i
LEFT JOIN resources a ON
	a.resource_type='items'
	AND json_extract(a.data,'$.data.itemType')='attachment'
	AND json_extract(a.data,'$.data.contentType')='application/pdf'
	AND json_extract(a.data,'$.data.parentItem')=i.id
WHERE i.resource_type='items'
	AND json_extract(i.data,'$.data.itemType') IN (
		'journalArticle','book','bookSection','conferencePaper','report','thesis','preprint','manuscript','document'
	)`)
	if err != nil {
		return libraryPDFCoverage{}, err
	}
	if len(rows) == 0 {
		return libraryPDFCoverage{}, nil
	}
	coverage := libraryPDFCoverage{
		TotalItems:   sqlIntValue(rows[0]["total_items"]),
		ItemsWithPDF: sqlIntValue(rows[0]["items_with_pdf"]),
	}
	if coverage.TotalItems > 0 {
		coverage.Pct = coverage.ItemsWithPDF * 100 / coverage.TotalItems
	}
	return coverage, nil
}

func printLibraryStats(cmd *cobra.Command, stats libraryStats, years int) error {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, bold("Library Statistics"))
	fmt.Fprintln(w)

	typeRows := make([]statRow, 0, len(stats.ItemsByType))
	for _, row := range stats.ItemsByType {
		typeRows = append(typeRows, statRow{label: row.ItemType, count: row.Count})
	}
	printStatSection(w, "Items by Type", typeRows)

	yearRows := make([]statRow, 0, len(stats.ItemsByYear))
	for _, row := range stats.ItemsByYear {
		yearRows = append(yearRows, statRow{label: row.Year, count: row.Count})
	}
	printStatSection(w, fmt.Sprintf("Items by Year (last %d years)", years), yearRows)
	if stats.AddedBy != "" {
		addedRows := make([]statRow, 0, len(stats.ItemsAdded))
		for _, row := range stats.ItemsAdded {
			addedRows = append(addedRows, statRow{label: row.Period, count: row.Count})
		}
		heading := "Items Added by Month"
		if stats.AddedBy == "year" {
			heading = "Items Added by Year"
		}
		printStatSection(w, heading, addedRows)
	}

	venueRows := make([]statRow, 0, len(stats.TopVenues))
	for _, row := range stats.TopVenues {
		venueRows = append(venueRows, statRow{label: row.Venue, count: row.Count})
	}
	printStatSection(w, "Top Venues", venueRows)

	cov := stats.PDFCoverage
	fmt.Fprintf(w, "%s  %d/%d (%d%%)  %s\n",
		bold("PDF Coverage"), cov.ItemsWithPDF, cov.TotalItems, cov.Pct, statBar(cov.Pct, 100))
	return nil
}

type statRow struct {
	label string
	count int
}

// printStatSection renders one aligned stats block: label column, right-aligned
// count, and a bar proportional to the section's largest count.
func printStatSection(w io.Writer, title string, rows []statRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, bold(title))
	labelW, countW, maxCount := 0, 0, 0
	counts := make([]string, len(rows))
	for i, r := range rows {
		r.label = sanitizeForTerminal(r.label)
		rows[i] = r
		if lw := displayWidth(r.label); lw > labelW {
			labelW = lw
		}
		counts[i] = strconv.Itoa(r.count)
		if cw := len(counts[i]); cw > countW {
			countW = cw
		}
		if r.count > maxCount {
			maxCount = r.count
		}
	}
	for i, r := range rows {
		fmt.Fprintf(w, "%s  %*s  %s\n", padRight(r.label, labelW), countW, counts[i], statBar(r.count, maxCount))
	}
	fmt.Fprintln(w)
}

// statBar renders a bar of up to 24 cells proportional to count/max.
// Non-zero counts always get at least one cell.
func statBar(count, max int) string {
	if max <= 0 || count <= 0 {
		return ""
	}
	const cells = 24
	n := count * cells / max
	if n < 1 {
		n = 1
	}
	return cyan(strings.Repeat("▆", n))
}
