// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local unfiled-items report missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newItemsUnfiledCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagType string

	cmd := &cobra.Command{
		Use:         "unfiled",
		Short:       "List top-level items not assigned to any collection",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
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

			rows, err := queryUnfiledItems(db, flagType, flagLimit)
			if err != nil {
				return fmt.Errorf("querying unfiled items: %w", err)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return (0 = no limit)")
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by Zotero item type")

	return cmd
}

func queryUnfiledItems(db localQueryStore, itemType string, limit int) ([]map[string]any, error) {
	query := `
SELECT
	i.id AS key,
	json_extract(i.data, '$.data.title') AS title,
	json_extract(i.data, '$.data.itemType') AS item_type,
	json_extract(i.data, '$.data.dateAdded') AS date_added
FROM resources i
WHERE i.resource_type = 'items'
	AND json_extract(i.data, '$.data.itemType') NOT IN ('attachment', 'note', 'annotation')
	AND (
		json_extract(i.data, '$.data.collections') IS NULL
		OR json_array_length(json_extract(i.data, '$.data.collections')) = 0
	)`
	args := make([]any, 0, 2)
	if itemType != "" {
		query += `
	AND json_extract(i.data, '$.data.itemType') = ?`
		args = append(args, itemType)
	}
	query += `
ORDER BY date_added DESC`
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	return db.QueryRaw(query, args...)
}
