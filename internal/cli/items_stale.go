// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsStaleCmd(flags *rootFlags) *cobra.Command {
	var flagDays int
	var flagNoPDF bool
	var flagNoAnnotations bool
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "stale",
		Short:       "Find old items without PDFs or annotations",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagDays < 0 {
				return fmt.Errorf("--days must be non-negative")
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

			rows, err := queryStaleItems(db, flagDays, flagNoPDF, flagNoAnnotations, flagLimit)
			if err != nil {
				return fmt.Errorf("querying stale items: %w", err)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().IntVar(&flagDays, "days", 365, "Items added more than this many days ago")
	cmd.Flags().BoolVar(&flagNoPDF, "no-pdf", false, "Include only items without a PDF attachment")
	cmd.Flags().BoolVar(&flagNoAnnotations, "no-annotations", false, "Include only items without annotation children")
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of items to return")

	return cmd
}

func queryStaleItems(db localQueryStore, days int, noPDF, noAnnotations bool, limit int) ([]map[string]any, error) {
	conditions := []string{
		"i.resource_type='items'",
		"json_extract(i.data,'$.data.itemType') NOT IN ('attachment','note','annotation')",
		"DATE(json_extract(i.data,'$.data.dateAdded')) <= DATE('now', '-' || ? || ' days')",
	}
	args := []any{days}

	applyNoPDF := noPDF || (!noPDF && !noAnnotations)
	applyNoAnnotations := noAnnotations || (!noPDF && !noAnnotations)
	if applyNoPDF {
		conditions = append(conditions, `NOT EXISTS (
	SELECT 1 FROM resources a WHERE a.resource_type='items'
		AND json_extract(a.data,'$.data.itemType')='attachment'
		AND json_extract(a.data,'$.data.contentType')='application/pdf'
		AND json_extract(a.data,'$.data.parentItem')=i.id
)`)
	}
	if applyNoAnnotations {
		conditions = append(conditions, `NOT EXISTS (
	SELECT 1 FROM resources a WHERE a.resource_type='items'
		AND json_extract(a.data,'$.data.itemType')='annotation'
		AND json_extract(a.data,'$.data.parentItem')=i.id
)`)
	}

	query := fmt.Sprintf(`
SELECT
	i.id AS key,
	json_extract(i.data,'$.data.title') AS title,
	json_extract(i.data,'$.data.itemType') AS item_type,
	json_extract(i.data,'$.data.dateAdded') AS date_added
FROM resources i
WHERE %s
ORDER BY date_added ASC`, strings.Join(conditions, "\n\tAND "))
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	return db.QueryRaw(query, args...)
}
