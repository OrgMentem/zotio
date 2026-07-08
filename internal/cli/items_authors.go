// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type itemAuthorRow struct {
	DisplayName string `json:"display_name"`
	CreatorType string `json:"creator_type"`
	ItemCount   int64  `json:"item_count"`
}

func newItemsAuthorsCmd(flags *rootFlags) *cobra.Command {
	var flagType string
	var flagTop int
	var flagCollection string

	cmd := &cobra.Command{
		Use:         "authors",
		Short:       "Count synced items per creator",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
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

			rows, err := queryItemAuthors(db, flagType, flagCollection, flagTop)
			if err != nil {
				return fmt.Errorf("querying authors: %w", err)
			}
			out := normalizeItemAuthorRows(rows)
			data, err := json.Marshal(out)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by creatorType (for example author or editor)")
	cmd.Flags().IntVar(&flagTop, "top", 20, "Maximum number of authors to return")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Filter to items in this collection key")

	return cmd
}

func queryItemAuthors(db localQueryStore, creatorType, collectionKey string, top int) ([]map[string]any, error) {
	query := `
SELECT
	COALESCE(TRIM(json_extract(creator.value,'$.lastName')),'') AS last_name,
	COALESCE(TRIM(json_extract(creator.value,'$.firstName')),'') AS first_name,
	COALESCE(TRIM(json_extract(creator.value,'$.name')),'') AS name,
	json_extract(creator.value,'$.creatorType') AS creator_type,
	COUNT(DISTINCT i.id) AS item_count
FROM resources i, json_each(json_extract(i.data,'$.data.creators')) AS creator
WHERE i.resource_type='items'
	AND json_extract(i.data,'$.data.itemType') NOT IN ('attachment','note','annotation')`
	args := make([]any, 0, 3)
	if creatorType != "" {
		query += `
	AND json_extract(creator.value,'$.creatorType') = ?`
		args = append(args, creatorType)
	}
	if collectionKey != "" {
		query += `
	AND EXISTS (
		SELECT 1 FROM json_each(json_extract(i.data,'$.data.collections')) c
		WHERE c.value = ?
	)`
		args = append(args, collectionKey)
	}
	query += `
GROUP BY last_name, first_name, name, creator_type
ORDER BY item_count DESC`
	if top > 0 {
		query += `
LIMIT ?`
		args = append(args, top)
	}
	return db.QueryRaw(query, args...)
}

func normalizeItemAuthorRows(rows []map[string]any) []itemAuthorRow {
	out := make([]itemAuthorRow, 0, len(rows))
	for _, row := range rows {
		displayName := formatCreatorDisplayName(sqlText(row["last_name"]), sqlText(row["first_name"]), sqlText(row["name"]))
		out = append(out, itemAuthorRow{
			DisplayName: displayName,
			CreatorType: sqlText(row["creator_type"]),
			ItemCount:   toInt64(row["item_count"]),
		})
	}
	return out
}

func formatCreatorDisplayName(lastName, firstName, name string) string {
	lastName = strings.TrimSpace(lastName)
	firstName = strings.TrimSpace(firstName)
	name = strings.TrimSpace(name)
	if lastName != "" {
		if firstName != "" {
			return lastName + ", " + firstName
		}
		return lastName
	}
	if name != "" {
		return name
	}
	return firstName
}
