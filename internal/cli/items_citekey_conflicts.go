// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written Better BibTeX citation-key audit missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type citekeyConflictRow struct {
	Type    string `json:"type"`
	Key     string `json:"key"`
	Title   string `json:"title"`
	CiteKey string `json:"cite_key"`
}

type citekeyItem struct {
	Key     string
	Title   string
	CiteKey string
}

// citekeyAuditQuery selects every citeable item with its Better BibTeX citation
// key source (the `extra` field). PATCH(glean roadmap-phase1): shared by
// `items citekey-conflicts` and the `library health` citekey checks so they
// never drift.
const citekeyAuditQuery = `
SELECT
	id AS key,
	json_extract(data,'$.data.title') AS title,
	COALESCE(json_extract(data,'$.data.extra'),'') AS extra
FROM resources
WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')`

func newItemsCitekeyConflictsCmd(flags *rootFlags) *cobra.Command {
	var flagMissing bool
	var flagConflicts bool

	cmd := &cobra.Command{
		Use:   "citekey-conflicts",
		Short: "Find missing and duplicate Better BibTeX citation keys",
		Example: `  zotio items citekey-conflicts
  zotio items citekey-conflicts --missing
  zotio items citekey-conflicts --conflicts --json`,
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

			rows, err := db.QueryRaw(citekeyAuditQuery)
			if err != nil {
				return fmt.Errorf("querying citation keys: %w", err)
			}

			out := buildCitekeyConflictRows(rows, flagMissing, flagConflicts)
			data, err := json.Marshal(out)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().BoolVar(&flagMissing, "missing", false, "Show only items without a citation key")
	cmd.Flags().BoolVar(&flagConflicts, "conflicts", false, "Show only items with duplicate citation keys")

	return cmd
}

func buildCitekeyConflictRows(rows []map[string]any, missingOnly, conflictsOnly bool) []citekeyConflictRow {
	showMissing := missingOnly || (!missingOnly && !conflictsOnly)
	showConflicts := conflictsOnly || (!missingOnly && !conflictsOnly)

	missing := make([]citekeyConflictRow, 0)
	byCiteKey := make(map[string][]citekeyItem)
	for _, row := range rows {
		item := citekeyItem{
			Key:   sqlText(row["key"]),
			Title: sqlText(row["title"]),
		}
		extra := sqlText(row["extra"])
		for _, line := range strings.Split(extra, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Citation Key: ") {
				item.CiteKey = strings.TrimSpace(strings.TrimPrefix(trimmed, "Citation Key: "))
				break
			}
		}
		if item.CiteKey == "" {
			if showMissing {
				missing = append(missing, citekeyConflictRow{
					Type:    "missing",
					Key:     item.Key,
					Title:   item.Title,
					CiteKey: "",
				})
			}
			continue
		}
		byCiteKey[item.CiteKey] = append(byCiteKey[item.CiteKey], item)
	}

	sort.Slice(missing, func(i, j int) bool {
		return citekeyItemLess(
			citekeyItem{Key: missing[i].Key, Title: missing[i].Title},
			citekeyItem{Key: missing[j].Key, Title: missing[j].Title},
		)
	})

	out := make([]citekeyConflictRow, 0, len(missing))
	out = append(out, missing...)
	if !showConflicts {
		return out
	}

	citeKeys := make([]string, 0, len(byCiteKey))
	for citeKey, items := range byCiteKey {
		if len(items) > 1 {
			citeKeys = append(citeKeys, citeKey)
		}
	}
	sort.Strings(citeKeys)
	for _, citeKey := range citeKeys {
		items := byCiteKey[citeKey]
		sort.Slice(items, func(i, j int) bool {
			return citekeyItemLess(items[i], items[j])
		})
		for _, item := range items {
			out = append(out, citekeyConflictRow{
				Type:    "conflict",
				Key:     item.Key,
				Title:   item.Title,
				CiteKey: item.CiteKey,
			})
		}
	}
	return out
}

func citekeyItemLess(a, b citekeyItem) bool {
	if !strings.EqualFold(a.Title, b.Title) {
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	}
	return a.Key < b.Key
}

func sqlText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return ""
	}
}
