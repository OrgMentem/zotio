// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local venue-count report missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newItemsVenuesCmd(flags *rootFlags) *cobra.Command {
	var flagTop int
	var flagType string

	cmd := &cobra.Command{
		Use:   "venues",
		Short: "Count synced items by publication venue",
		Example: `  zotero-pp-cli items venues
  zotero-pp-cli items venues --top 10
  zotero-pp-cli items venues --type journalArticle --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			rows, err := queryItemVenues(db, flagType, flagTop)
			if err != nil {
				return fmt.Errorf("querying venues: %w", err)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().IntVar(&flagTop, "top", 20, "Maximum number of venues to return")
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by Zotero itemType")

	return cmd
}

func queryItemVenues(db localQueryStore, itemType string, top int) ([]map[string]any, error) {
	query := `
SELECT
	COALESCE(
		NULLIF(TRIM(json_extract(data,'$.data.publicationTitle')),''),
		NULLIF(TRIM(json_extract(data,'$.data.bookTitle')),''),
		NULLIF(TRIM(json_extract(data,'$.data.conferenceName')),''),
		NULLIF(TRIM(json_extract(data,'$.data.publisher')),'')
	) AS venue,
	json_extract(data,'$.data.itemType') AS item_type,
	MIN(SUBSTR(COALESCE(json_extract(data,'$.data.date'),''),1,4)) AS min_year,
	MAX(SUBSTR(COALESCE(json_extract(data,'$.data.date'),''),1,4)) AS max_year,
	COUNT(*) AS count
FROM resources
WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')`
	args := make([]any, 0, 2)
	if itemType != "" {
		query += `
	AND json_extract(data,'$.data.itemType') = ?`
		args = append(args, itemType)
	}
	query += `
GROUP BY venue
HAVING venue IS NOT NULL AND venue != ''
ORDER BY count DESC`
	if top > 0 {
		query += `
LIMIT ?`
		args = append(args, top)
	}
	return db.QueryRaw(query, args...)
}
