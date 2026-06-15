// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local duplicate detection missing from the generated CLI.

package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"zotero-pp-cli/internal/store"

	"github.com/spf13/cobra"
)

type localQueryStore struct {
	*store.Store
}

func (s localQueryStore) QueryRaw(query string, args ...any) ([]map[string]any, error) {
	rows, err := s.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			row[col] = normalizeSQLValue(values[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeSQLValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	case sql.NullString:
		if x.Valid {
			return x.String
		}
		return nil
	default:
		return x
	}
}

func newItemsDuplicatesCmd(flags *rootFlags) *cobra.Command {
	var flagBy string

	cmd := &cobra.Command{
		Use:         "duplicates",
		Short:       "Find likely duplicate items in the local store",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first to enable duplicate detection.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{Store: rawDB}

			results := make([]map[string]any, 0)
			switch flagBy {
			case "doi":
				results, err = queryDuplicateDOIs(db)
			case "title":
				results, err = queryDuplicateTitles(db)
			case "all":
				results, err = queryDuplicateDOIs(db)
				if err == nil {
					var titleRows []map[string]any
					titleRows, err = queryDuplicateTitles(db)
					results = append(results, titleRows...)
				}
			default:
				return fmt.Errorf("invalid --by value %q: must be doi, title, or all", flagBy)
			}
			if err != nil {
				return fmt.Errorf("querying duplicates: %w", err)
			}
			data, err := json.Marshal(normalizeDuplicateRows(results))
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagBy, "by", "all", "Duplicate detector to run (doi, title, all)")

	return cmd
}

func queryDuplicateDOIs(db localQueryStore) ([]map[string]any, error) {
	return db.QueryRaw(`
SELECT
	'doi' AS "group",
	value,
	COUNT(*) AS count,
	json_group_array(id) AS keys
FROM (
	SELECT id, LOWER(TRIM(json_extract(data, '$.data.DOI'))) AS value
	FROM resources
	WHERE resource_type = 'items'
		AND COALESCE(TRIM(json_extract(data, '$.data.DOI')), '') != ''
)
GROUP BY value
HAVING COUNT(*) > 1
ORDER BY count DESC, value`)
}

func queryDuplicateTitles(db localQueryStore) ([]map[string]any, error) {
	return db.QueryRaw(`
SELECT
	'title' AS "group",
	MIN(title) AS value,
	COUNT(*) AS count,
	json_group_array(id) AS keys
FROM (
	SELECT
		id,
		TRIM(json_extract(data, '$.data.title')) AS title,
		LOWER(TRIM(json_extract(data, '$.data.title'))) AS normalized_title,
		COALESCE(json_extract(data, '$.data.itemType'), '') AS item_type
	FROM resources
	WHERE resource_type = 'items'
		AND COALESCE(TRIM(json_extract(data, '$.data.title')), '') != ''
)
GROUP BY normalized_title, item_type
HAVING COUNT(*) > 1
ORDER BY count DESC, value`)
}

func normalizeDuplicateRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		normalized := make(map[string]any, len(row))
		for k, v := range row {
			normalized[k] = v
		}
		if rawKeys, ok := normalized["keys"].(string); ok {
			var keys []string
			if json.Unmarshal([]byte(rawKeys), &keys) == nil {
				normalized["keys"] = keys
			}
		}
		out = append(out, normalized)
	}
	return out
}
