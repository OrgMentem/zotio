// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local item metadata health audit missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type itemsAuditSummary struct {
	MissingPDF      int `json:"missing_pdf"`
	MissingAbstract int `json:"missing_abstract"`
	MissingDOI      int `json:"missing_doi"`
	MissingTags     int `json:"missing_tags"`
}

func newItemsAuditCmd(flags *rootFlags) *cobra.Command {
	var flagMissingPDF bool
	var flagMissingAbstract bool
	var flagMissingDOI bool
	var flagMissingTags bool
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "audit",
		Short:       "Audit locally synced items for missing metadata and PDFs",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			checks := selectedItemsAuditChecks(flagMissingPDF, flagMissingAbstract, flagMissingDOI, flagMissingTags)
			if len(checks) == 0 {
				summary, err := queryItemsAuditSummary(db)
				if err != nil {
					return fmt.Errorf("querying item audit summary: %w", err)
				}
				if flags.asJSON {
					data, err := json.Marshal(summary)
					if err != nil {
						return err
					}
					return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
				}
				return printItemsAuditSummary(cmd, summary)
			}

			results := make(map[string][]map[string]any, len(checks))
			for _, check := range checks {
				rows, err := check.query(db, flagLimit)
				if err != nil {
					return fmt.Errorf("querying %s: %w", check.name, err)
				}
				results[check.name] = rows
			}
			if len(checks) == 1 {
				data, err := json.Marshal(results[checks[0].name])
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			data, err := json.Marshal(results)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().BoolVar(&flagMissingPDF, "missing-pdf", false, "List items that should have an attached PDF but do not")
	cmd.Flags().BoolVar(&flagMissingAbstract, "missing-abstract", false, "List items with no abstract")
	cmd.Flags().BoolVar(&flagMissingDOI, "missing-doi", false, "List journal articles, conference papers, and preprints with no DOI")
	cmd.Flags().BoolVar(&flagMissingTags, "missing-tags", false, "List items with no tags")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items per category (0 = no limit)")

	return cmd
}

type itemsAuditCheck struct {
	name  string
	query func(localQueryStore, int) ([]map[string]any, error)
}

func selectedItemsAuditChecks(missingPDF, missingAbstract, missingDOI, missingTags bool) []itemsAuditCheck {
	checks := make([]itemsAuditCheck, 0, 4)
	if missingPDF {
		checks = append(checks, itemsAuditCheck{name: "missing_pdf", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return queryMissingPDFItems(db, "", limit)
		}})
	}
	if missingAbstract {
		checks = append(checks, itemsAuditCheck{name: "missing_abstract", query: queryMissingAbstractItems})
	}
	if missingDOI {
		checks = append(checks, itemsAuditCheck{name: "missing_doi", query: queryMissingDOIItems})
	}
	if missingTags {
		checks = append(checks, itemsAuditCheck{name: "missing_tags", query: queryMissingTagsItems})
	}
	return checks
}

func queryItemsAuditSummary(db localQueryStore) (itemsAuditSummary, error) {
	missingPDF, err := queryMissingPDFCount(db)
	if err != nil {
		return itemsAuditSummary{}, err
	}
	// PATCH(glean perf-audit 2qhf): fold the three single-row predicate counts
	// (abstract/DOI/tags) into one table scan with conditional aggregation
	// instead of three separate COUNT scans. The PDF count keeps its own query
	// because it needs the attachment anti-join; the DOI predicate uses the
	// indexed item_type column (see m4ku).
	rows, err := db.QueryRaw(`
SELECT
	COUNT(CASE WHEN json_extract(data, '$.data.abstractNote') IS NULL OR TRIM(json_extract(data, '$.data.abstractNote')) = '' THEN 1 END) AS missing_abstract,
	COUNT(CASE WHEN item_type IN ('journalArticle', 'conferencePaper', 'preprint')
		AND (json_extract(data, '$.data.DOI') IS NULL OR TRIM(json_extract(data, '$.data.DOI')) = '') THEN 1 END) AS missing_doi,
	COUNT(CASE WHEN COALESCE(json_array_length(json_extract(data, '$.data.tags')), 0) = 0 THEN 1 END) AS missing_tags
FROM resources
WHERE resource_type = 'items'`)
	if err != nil {
		return itemsAuditSummary{}, err
	}
	var missingAbstract, missingDOI, missingTags int
	if len(rows) > 0 {
		missingAbstract = sqlIntValue(rows[0]["missing_abstract"])
		missingDOI = sqlIntValue(rows[0]["missing_doi"])
		missingTags = sqlIntValue(rows[0]["missing_tags"])
	}
	return itemsAuditSummary{
		MissingPDF:      missingPDF,
		MissingAbstract: missingAbstract,
		MissingDOI:      missingDOI,
		MissingTags:     missingTags,
	}, nil
}

func printItemsAuditSummary(cmd *cobra.Command, summary itemsAuditSummary) error {
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "Check\tCount")
	fmt.Fprintf(tw, "%s\t%d\n", "missing-pdf", summary.MissingPDF)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-abstract", summary.MissingAbstract)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-doi", summary.MissingDOI)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-tags", summary.MissingTags)
	return tw.Flush()
}

func queryMissingAbstractItems(db localQueryStore, limit int) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND (json_extract(data, '$.data.abstractNote') IS NULL OR TRIM(json_extract(data, '$.data.abstractNote')) = '')
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit)
}

func queryMissingDOIItems(db localQueryStore, limit int) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND json_extract(data, '$.data.itemType') IN ('journalArticle', 'conferencePaper', 'preprint')
	AND (json_extract(data, '$.data.DOI') IS NULL OR TRIM(json_extract(data, '$.data.DOI')) = '')
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit)
}

func queryMissingTagsItems(db localQueryStore, limit int) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(json_array_length(json_extract(data, '$.data.tags')), 0) = 0
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit)
}

func queryItemsAuditRows(db localQueryStore, query string, limit int) ([]map[string]any, error) {
	args := make([]any, 0, 1)
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	return db.QueryRaw(query, args...)
}

func firstCount(rows []map[string]any) int {
	if len(rows) == 0 {
		return 0
	}
	return sqlIntValue(rows[0]["count"])
}

func sqlIntValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	default:
		return 0
	}
}

func sqlStringValue(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", s)
	}
}
