// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local missing-PDF audit workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

const missingPDFItemTypesSQL = `
		'journalArticle', 'book', 'bookSection', 'conferencePaper',
		'report', 'thesis', 'preprint', 'manuscript', 'document'
`

func newItemsMissingPdfCmd(flags *rootFlags) *cobra.Command {
	var flagType string
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "missing-pdf",
		Short:       "List items that should have an attached PDF but do not",
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

			rows, err := queryMissingPDFItems(db, flagType, flagLimit, "")
			if err != nil {
				return fmt.Errorf("querying missing PDFs: %w", err)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().StringVar(&flagType, "type", "", "Filter to a specific Zotero item type")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return (0 = no limit)")

	return cmd
}

func queryMissingPDFItems(db localQueryStore, itemType string, limit int, collection string) ([]map[string]any, error) {
	// PATCH(glean perf-audit m4ku): filter on the indexed item_type/parent_key
	// columns instead of json_extract so SQLite uses idx_resources_item_type /
	// idx_resources_parent_key rather than scanning and JSON-parsing every row.
	// PATCH(glean bugfix): accept an optional collection filter for items enrich.
	query := `
SELECT
	i.id AS key,
	json_extract(i.data, '$.data.title') AS title,
	i.item_type AS item_type,
	json_extract(i.data, '$.data.DOI') AS doi,
	json_extract(i.data, '$.data.dateAdded') AS date_added
FROM resources i
WHERE i.resource_type = 'items'
	AND i.item_type IN (` + missingPDFItemTypesSQL + `)
	AND NOT EXISTS (
		SELECT 1 FROM resources a
		WHERE a.resource_type = 'items'
			AND a.item_type = 'attachment'
			AND json_extract(a.data, '$.data.contentType') = 'application/pdf'
			AND a.parent_key = i.id
	)`
	args := make([]any, 0, 3)
	if itemType != "" {
		query += `
	AND i.item_type = ?`
		args = append(args, itemType)
	}
	if collection != "" {
		query += `
	AND EXISTS (SELECT 1 FROM json_each(json_extract(i.data,'$.data.collections')) WHERE value = ?)`
		args = append(args, collection)
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

func queryMissingPDFCount(db localQueryStore) (int, error) {
	rows, err := db.QueryRaw(`
SELECT COUNT(*) AS count
FROM resources i
WHERE i.resource_type = 'items'
	AND i.item_type IN (` + missingPDFItemTypesSQL + `)
	AND NOT EXISTS (
		SELECT 1 FROM resources a
		WHERE a.resource_type = 'items'
			AND a.item_type = 'attachment'
			AND json_extract(a.data, '$.data.contentType') = 'application/pdf'
			AND a.parent_key = i.id
	)`)
	if err != nil {
		return 0, err
	}
	return firstCount(rows), nil
}
