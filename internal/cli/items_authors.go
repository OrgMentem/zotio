// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type itemAuthorRow struct {
	DisplayName string `json:"display_name"`
	CreatorType string `json:"creator_type"`
	ItemCount   int64  `json:"item_count"`
}

func newItemsAuthorsCmd(flags *rootFlags) *cobra.Command {
	var flagCreatorType string
	var flagLimit int
	var flagCollection string
	var flagScope string

	cmd := &cobra.Command{
		Use:         "authors",
		Short:       "Count synced items per creator",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMirrorOnlySource(flags, cmd.CommandPath()); err != nil {
				return err
			}
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			if flagCollection != "" {
				if err := validateKeyArg("collection", flagCollection); err != nil {
					return err
				}
			}
			scopeExpr, err := reconcileScopeFlags(flagScope, scopeSugarFor("collection", "collection", flagCollection))
			if err != nil {
				return err
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				return errUnsyncedStoreFor(cmd, flags)
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			sel, err := resolveScopeSelection(cmd, flags, "items authors", db, scopeExpr)
			if err != nil {
				return err
			}
			rows, err := queryItemAuthorsScoped(db, flagCreatorType, flagCollection, sel, flagLimit)
			if err != nil {
				return fmt.Errorf("querying authors: %w", err)
			}
			out, err := normalizeItemAuthorRows(rows)
			if err != nil {
				return err
			}
			data, err := json.Marshal(out)
			if err != nil {
				return err
			}
			prov := localProvenance(rawDB, "items", "local_only")
			printProvenance(cmd, len(out), prov)
			return printMirrorRows(cmd, flags, data, prov)
		},
	}
	cmd.Flags().StringVar(&flagCreatorType, "creator-type", "", "Filter by creatorType (for example author or editor)")
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of authors to return (0 = no limit)")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Filter to items in this collection key (same as --scope collection:KEY)")
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultUnset, scopeFlagUsageDefaultLibrary)

	return cmd
}

// queryItemAuthors trims inside SQL on purpose: the trimmed name fields are the
// GROUP BY key, so trimming after the scan would keep the grouping on untrimmed
// text and split "Curie" from "\tCurie" into two creators with separate counts.
// SQL therefore owns the normalization for this path; see sqlWhitespaceCharSet
// for why the character set is spelled out.
func queryItemAuthors(db localQueryStore, creatorType, collectionKey string, top int) ([]map[string]any, error) {
	return queryItemAuthorsScoped(db, creatorType, collectionKey, scopeSelection{}, top)
}

// queryItemAuthorsScoped narrows the creator count to a resolved --scope
// cohort inside SQL, so the LIMIT still applies to creators rather than to
// items. The cohort travels as one JSON array parameter: a cohort has no size
// ceiling, while SQLite's bound-variable count does.
func queryItemAuthorsScoped(db localQueryStore, creatorType, collectionKey string, sel scopeSelection, top int) ([]map[string]any, error) {
	query := `
SELECT
	COALESCE(TRIM(json_extract(creator.value,'$.lastName'), ` + sqlWhitespaceCharSet + `),'') AS last_name,
	COALESCE(TRIM(json_extract(creator.value,'$.firstName'), ` + sqlWhitespaceCharSet + `),'') AS first_name,
	COALESCE(TRIM(json_extract(creator.value,'$.name'), ` + sqlWhitespaceCharSet + `),'') AS name,
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
	if sel.restricted() {
		keys := make([]string, 0, len(sel.Keys))
		for key := range sel.Keys {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		keyJSON, err := json.Marshal(keys)
		if err != nil {
			return nil, err
		}
		query += `
	AND i.id IN (SELECT value FROM json_each(?))`
		args = append(args, string(keyJSON))
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

func normalizeItemAuthorRows(rows []map[string]any) ([]itemAuthorRow, error) {
	out := make([]itemAuthorRow, 0, len(rows))
	for _, row := range rows {
		displayName := formatCreatorDisplayName(sqlText(row["last_name"]), sqlText(row["first_name"]), sqlText(row["name"]))
		itemCount, err := toInt64(row["item_count"])
		if err != nil {
			return nil, fmt.Errorf("parsing item count for %q: %w", displayName, err)
		}
		out = append(out, itemAuthorRow{
			DisplayName: displayName,
			CreatorType: sqlText(row["creator_type"]),
			ItemCount:   itemCount,
		})
	}
	return out, nil
}

// formatCreatorDisplayName keeps its own strings.TrimSpace even though the
// queryItemAuthors rows arrive already trimmed by SQL: items_note_template.go
// calls it with creator fields decoded straight from item JSON, which no SQL
// TRIM ever touched. So SQL owns the normalization wherever a trimmed value is
// a grouping key, and this function owns it for the raw-JSON callers. The two
// agree on ASCII whitespace by construction (see sqlWhitespaceCharSet); the
// second pass over already-trimmed SQL output is therefore a no-op, not a
// competing rule.
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
