// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local identifier lookup missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newItemsFindCmd(flags *rootFlags) *cobra.Command {
	var flagDOI string
	var flagISBN string
	var flagPMID string
	var flagCitekey string

	cmd := &cobra.Command{
		Use:   "find",
		Short: "Find locally synced items by DOI, ISBN, PMID, or citation key",
		Example: `  zotio items find --doi 10.1145/3290605.3300709
  zotio items find --isbn 978-0-262-03384-8
  zotio items find --citekey smith2023 --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagDOI == "" && flagISBN == "" && flagPMID == "" && flagCitekey == "" {
				return fmt.Errorf("at least one of --doi, --isbn, --pmid, or --citekey is required")
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first to enable identifier lookup.")
				return nil
			}
			var storeDB *store.Store = rawDB
			defer storeDB.Close()
			db := localQueryStore{Store: storeDB}

			// PATCH(glean zotero-pp-cli-513fd990b1b79758): keep PMID/citekey lookups exact by escaping SQLite LIKE wildcards.
			escapedPMID := escapeSQLiteLikeLiteral(flagPMID)
			escapedCitekey := escapeSQLiteLikeLiteral(flagCitekey)
			rows, err := db.QueryRaw(`
SELECT id, data
FROM resources
WHERE resource_type = 'items'
	AND (
		(? != '' AND json_extract(data, '$.data.DOI') = ?)
		OR (? != '' AND json_extract(data, '$.data.ISBN') = ?)
		OR (? != '' AND json_extract(data, '$.data.extra') LIKE '%PMID: ' || ? || '%' ESCAPE '\')
		OR (? != '' AND json_extract(data, '$.data.extra') LIKE '%Citation Key: ' || ? || '%' ESCAPE '\')
	)
ORDER BY id`, flagDOI, flagDOI, flagISBN, flagISBN, flagPMID, escapedPMID, flagCitekey, escapedCitekey)
			if err != nil {
				return fmt.Errorf("querying local identifiers: %w", err)
			}
			data, err := json.Marshal(extractItemDataRows(rows))
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagDOI, "doi", "", "Find items with this DOI")
	cmd.Flags().StringVar(&flagISBN, "isbn", "", "Find items with this ISBN")
	cmd.Flags().StringVar(&flagPMID, "pmid", "", "Find items with this PMID in Extra")
	cmd.Flags().StringVar(&flagCitekey, "citekey", "", "Find items with this Better BibTeX citation key")

	return cmd
}

func escapeSQLiteLikeLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

func extractItemDataRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		raw, ok := row["data"].(string)
		if !ok {
			out = append(out, row)
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(raw), &item) != nil {
			out = append(out, row)
			continue
		}
		out = append(out, item)
	}
	return out
}
