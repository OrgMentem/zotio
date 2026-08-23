// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsFindCmd(flags *rootFlags) *cobra.Command {
	var flagDOI string
	var flagArXiv string
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
			if flagDOI == "" && flagArXiv == "" && flagISBN == "" && flagPMID == "" && flagCitekey == "" {
				return fmt.Errorf("at least one of --doi, --arxiv, --isbn, --pmid, or --citekey is required")
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first to enable identifier lookup.")
				return nil
			}
			var storeDB = rawDB
			defer storeDB.Close()
			db := localQueryStore{Store: storeDB}

			// Keep arXiv/PMID/citekey lookups exact by escaping SQLite LIKE
			// wildcards. Accept both Extra spellings: Zotero writes the
			// colon-tight form, while zotio's importer writes the spaced form.
			escapedArXiv := escapeSQLiteLikeLiteral(flagArXiv)
			escapedPMID := escapeSQLiteLikeLiteral(flagPMID)
			escapedCitekey := escapeSQLiteLikeLiteral(flagCitekey)
			rows, err := db.QueryRaw(`
SELECT id, data
FROM resources
WHERE resource_type = 'items'
	AND (parent_key IS NULL OR parent_key = '')
	AND (
		(? != '' AND json_extract(data, '$.data.DOI') = ?)
		OR (? != '' AND (
			json_extract(data, '$.data.archiveID') = 'arXiv:' || ?
			OR json_extract(data, '$.data.archiveID') = ?
			OR json_extract(data, '$.data.extra') LIKE '%arXiv: ' || ? || '%' ESCAPE '\'
			OR json_extract(data, '$.data.extra') LIKE '%arXiv:' || ? || '%' ESCAPE '\'
		))
		OR (? != '' AND json_extract(data, '$.data.ISBN') = ?)
		OR (? != '' AND (
			json_extract(data, '$.data.extra') LIKE '%PMID: ' || ? || '%' ESCAPE '\'
			OR json_extract(data, '$.data.extra') LIKE '%PMID:' || ? || '%' ESCAPE '\'
		))
		OR (? != '' AND (
			json_extract(data, '$.data.extra') LIKE '%Citation Key: ' || ? || '%' ESCAPE '\'
			OR json_extract(data, '$.data.extra') LIKE '%Citation Key:' || ? || '%' ESCAPE '\'
		))
		OR (? != '' AND json_extract(data, '$.data.citationKey') = ?)
	)
ORDER BY id`, flagDOI, flagDOI, flagArXiv, escapedArXiv, flagArXiv, escapedArXiv, escapedArXiv, flagISBN, flagISBN, flagPMID, escapedPMID, escapedPMID, flagCitekey, escapedCitekey, escapedCitekey, flagCitekey, flagCitekey)
			if err != nil {
				return fmt.Errorf("querying local identifiers: %w", err)
			}
			// Keep the SQL LIKE as a cheap candidate filter, then post-filter
			// in Go with exact token-boundary checks to avoid prefix
			// over-matches (e.g. smith2023 matching smith2023a, PMID 123
			// matching 12345). Token ends at end-of-string, newline, or
			// whitespace.
			if flagArXiv != "" || flagPMID != "" || flagCitekey != "" {
				rows = filterFindRowsExact(rows, flagDOI, flagArXiv, flagISBN, flagPMID, flagCitekey)
			}
			data, err := json.Marshal(extractItemDataRows(rows))
			if err != nil {
				return err
			}
			// identifier-lookup endpoint — so it always reads the local
			// mirror. Route through the same envelope pipeline as every
			// other read command so `.results` stays a JSON array here too
			// (it used to be a bare top-level array with no meta/results
			// wrapper at all).
			prov := localProvenance(rawDB, "items", "local_only")
			printProvenance(cmd, countResultItems(data), prov)
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			// For all other output modes (table, csv, plain, quiet), use the standard pipeline
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						return err
					}
					if len(items) >= 25 {
						fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
					}
					return nil
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagDOI, "doi", "", "Find items with this DOI")
	cmd.Flags().StringVar(&flagArXiv, "arxiv", "", "Find items with this arXiv ID")
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

func filterFindRowsExact(rows []map[string]any, flagDOI, flagArXiv, flagISBN, flagPMID, flagCitekey string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if findRowMatchesExact(row, flagDOI, flagArXiv, flagISBN, flagPMID, flagCitekey) {
			out = append(out, row)
		}
	}
	return out
}

func findRowMatchesExact(row map[string]any, flagDOI, flagArXiv, flagISBN, flagPMID, flagCitekey string) bool {
	raw, ok := row["data"].(string)
	if !ok {
		// Keep candidate; no data to check — let it through rather than dropping results silently.
		return true
	}
	var decoded struct {
		Data struct {
			DOI         string `json:"DOI"`
			ISBN        string `json:"ISBN"`
			ArchiveID   string `json:"archiveID"`
			CitationKey string `json:"citationKey"`
			Extra       string `json:"extra"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return true
	}
	d := decoded.Data
	if flagDOI != "" && d.DOI == flagDOI {
		return true
	}
	if flagISBN != "" && d.ISBN == flagISBN {
		return true
	}
	if flagArXiv != "" {
		// archiveID is exact: "arXiv:<id>" or plain id.
		if d.ArchiveID == "arXiv:"+flagArXiv || d.ArchiveID == flagArXiv {
			return true
		}
		if extraContainsExactToken(d.Extra, "arXiv: ", flagArXiv) ||
			extraContainsExactToken(d.Extra, "arXiv:", flagArXiv) {
			return true
		}
	}
	if flagPMID != "" &&
		(extraContainsExactToken(d.Extra, "PMID: ", flagPMID) ||
			extraContainsExactToken(d.Extra, "PMID:", flagPMID)) {
		return true
	}
	if flagCitekey != "" {
		if d.CitationKey == flagCitekey {
			return true
		}
		if extraContainsExactToken(d.Extra, "Citation Key: ", flagCitekey) ||
			extraContainsExactToken(d.Extra, "Citation Key:", flagCitekey) {
			return true
		}
	}
	return false
}

func extraContainsExactToken(extra, prefix, token string) bool {
	if token == "" {
		return false
	}
	// Extra is a set of newline-separated lines; we scan for the exact
	// "<prefix><token>" and verify the token ends at a boundary (end of
	// string, newline, or whitespace) so "smith2023" does not match
	// "smith2023a" and "123" does not match "12345".
	needle := prefix + token
	searchFrom := 0
	for {
		idx := strings.Index(extra[searchFrom:], needle)
		if idx < 0 {
			return false
		}
		pos := searchFrom + idx
		after := pos + len(needle)
		if after >= len(extra) {
			return true
		}
		switch extra[after] {
		case '\n', '\r', ' ', '\t':
			return true
		default:
			// Prefix match only — continue searching past this occurrence.
			searchFrom = after
		}
	}
}
