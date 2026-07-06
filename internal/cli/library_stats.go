// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written local library statistics report missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

type libraryStats struct {
	ItemsByType []libraryTypeCount  `json:"items_by_type"`
	ItemsByYear []libraryYearCount  `json:"items_by_year"`
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

			stats, err := queryLibraryStats(db, flagTop, flagYears)
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

	return cmd
}

func queryLibraryStats(db localQueryStore, top, years int) (libraryStats, error) {
	itemsByType, err := queryLibraryItemsByType(db)
	if err != nil {
		return libraryStats{}, err
	}
	itemsByYear, err := queryLibraryItemsByYear(db, years)
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
	rows, err := db.QueryRaw(`
SELECT
	COUNT(DISTINCT i.id) AS total_items,
	COUNT(DISTINCT a.id) AS items_with_pdf
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
	fmt.Fprintln(w, "Library Statistics")
	fmt.Fprintln(w, "==================")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Items by Type")
	tw := newTabWriter(w)
	for _, row := range stats.ItemsByType {
		fmt.Fprintf(tw, "%s\t%d\n", row.ItemType, row.Count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Items by Year (last %d years)\n", years)
	tw = newTabWriter(w)
	for _, row := range stats.ItemsByYear {
		fmt.Fprintf(tw, "%s\t%d\n", row.Year, row.Count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Top Venues")
	tw = newTabWriter(w)
	for _, row := range stats.TopVenues {
		fmt.Fprintf(tw, "%s\t%d\n", row.Venue, row.Count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "PDF Coverage: %d/%d (%d%%)\n", stats.PDFCoverage.ItemsWithPDF, stats.PDFCoverage.TotalItems, stats.PDFCoverage.Pct)
	return nil
}
