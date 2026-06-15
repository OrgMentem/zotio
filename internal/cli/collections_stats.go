// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newCollectionsStatsCmd(flags *rootFlags) *cobra.Command {
	var flagTop int

	cmd := &cobra.Command{
		Use:         "stats <collectionKey>",
		Short:       "Show analytics for a collection (item count, PDF coverage, year range, top journals)",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			collKey := args[0]

			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first to enable collection analytics.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			// Total items + year range
			summaryRows, err := db.QueryRaw(`
SELECT
  COUNT(*) AS total,
  MIN(CASE WHEN SUBSTR(COALESCE(json_extract(data,'$.data.date'),''),1,4) GLOB '[12][0-9][0-9][0-9]'
       THEN SUBSTR(json_extract(data,'$.data.date'),1,4) END) AS min_year,
  MAX(CASE WHEN SUBSTR(COALESCE(json_extract(data,'$.data.date'),''),1,4) GLOB '[12][0-9][0-9][0-9]'
       THEN SUBSTR(json_extract(data,'$.data.date'),1,4) END) AS max_year
FROM resources
WHERE resource_type='items'
  AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')
  AND EXISTS (
    SELECT 1 FROM json_each(json_extract(data,'$.data.collections')) c
    WHERE c.value = ?
  )`, collKey)
			if err != nil {
				return fmt.Errorf("querying collection stats: %w", err)
			}

			// PDF count
			pdfRows, err := db.QueryRaw(`
SELECT COUNT(*) AS items_with_pdf
FROM resources a
WHERE a.resource_type='items'
  AND json_extract(a.data,'$.data.itemType')='attachment'
  AND json_extract(a.data,'$.data.contentType')='application/pdf'
  AND EXISTS (
    SELECT 1 FROM resources i
    WHERE i.resource_type='items'
      AND i.id = json_extract(a.data,'$.data.parentItem')
      AND EXISTS (
        SELECT 1 FROM json_each(json_extract(i.data,'$.data.collections')) c
        WHERE c.value = ?
      )
  )`, collKey)
			if err != nil {
				return fmt.Errorf("querying PDF count: %w", err)
			}

			// Top journals
			venueRows, err := db.QueryRaw(`
SELECT
  COALESCE(
    NULLIF(TRIM(json_extract(data,'$.data.publicationTitle')),''),
    NULLIF(TRIM(json_extract(data,'$.data.bookTitle')),''),
    NULLIF(TRIM(json_extract(data,'$.data.publisher')),'')
  ) AS venue,
  COUNT(*) AS count
FROM resources
WHERE resource_type='items'
  AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')
  AND EXISTS (
    SELECT 1 FROM json_each(json_extract(data,'$.data.collections')) c
    WHERE c.value = ?
  )
GROUP BY venue
HAVING venue IS NOT NULL AND venue != ''
ORDER BY count DESC
LIMIT ?`, collKey, flagTop)
			if err != nil {
				return fmt.Errorf("querying top journals: %w", err)
			}

			// Build result
			var total, pdfCount int
			var minYear, maxYear string
			if len(summaryRows) > 0 {
				if v, ok := summaryRows[0]["total"]; ok {
					total = int(toInt64(v))
				}
				if v, ok := summaryRows[0]["min_year"]; ok && v != nil {
					minYear, _ = v.(string)
				}
				if v, ok := summaryRows[0]["max_year"]; ok && v != nil {
					maxYear, _ = v.(string)
				}
			}
			if len(pdfRows) > 0 {
				if v, ok := pdfRows[0]["items_with_pdf"]; ok {
					pdfCount = int(toInt64(v))
				}
			}

			var pdfPct float64
			if total > 0 {
				pdfPct = float64(pdfCount) / float64(total) * 100
			}

			type statsResult struct {
				CollectionKey string           `json:"collection_key"`
				TotalItems    int              `json:"total_items"`
				ItemsWithPDF  int              `json:"items_with_pdf"`
				PDFPct        float64          `json:"pdf_pct"`
				YearRange     string           `json:"year_range"`
				TopVenues     []map[string]any `json:"top_venues"`
			}

			yearRange := ""
			if minYear != "" && maxYear != "" {
				if minYear == maxYear {
					yearRange = minYear
				} else {
					yearRange = minYear + "–" + maxYear
				}
			}

			result := statsResult{
				CollectionKey: collKey,
				TotalItems:    total,
				ItemsWithPDF:  pdfCount,
				PDFPct:        pdfPct,
				YearRange:     yearRange,
				TopVenues:     venueRows,
			}

			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				data, _ := json.Marshal(result)
				return printOutput(cmd.OutOrStdout(), data, true)
			}

			// Human table output
			fmt.Fprintf(cmd.OutOrStdout(), "Collection: %s\n\n", collKey)
			fmt.Fprintf(cmd.OutOrStdout(), "Items:        %d\n", total)
			fmt.Fprintf(cmd.OutOrStdout(), "PDF coverage: %d/%d (%.0f%%)\n", pdfCount, total, pdfPct)
			if yearRange != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Year range:   %s\n", yearRange)
			}
			if len(venueRows) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nTop Venues:\n")
				for _, v := range venueRows {
					venue, _ := v["venue"].(string)
					count := toInt64(v["count"])
					fmt.Fprintf(cmd.OutOrStdout(), "  %-40s %d\n", venue, count)
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&flagTop, "top", 5, "Number of top venues to show")

	return cmd
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case string:
		var n int64
		fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}
