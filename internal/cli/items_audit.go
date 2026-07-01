// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written local item metadata health audit missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

type itemsAuditSummary struct {
	MissingPDF      int `json:"missing_pdf"`
	MissingAbstract int `json:"missing_abstract"`
	MissingDOI      int `json:"missing_doi"`
	MissingTags     int `json:"missing_tags"`
	MissingCitation int `json:"missing_citation"`
}

func newItemsAuditCmd(flags *rootFlags) *cobra.Command {
	var flagMissingPDF bool
	var flagMissingAbstract bool
	var flagMissingDOI bool
	var flagMissingTags bool
	var flagCitations bool
	var flagLimit int
	var flagVerifyFiles bool

	cmd := &cobra.Command{
		Use:         "audit",
		Short:       "Audit locally synced items for missing metadata and PDFs",
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

			if flagVerifyFiles {
				return runVerifyAttachmentFiles(cmd, db, flags, flagLimit)
			}

			checks := selectedItemsAuditChecks(flagMissingPDF, flagMissingAbstract, flagMissingDOI, flagMissingTags, flagCitations)
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
	cmd.Flags().BoolVar(&flagCitations, "missing-citation", false, "List citeable items missing core citation fields (creators, title, date, venue)")
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items per category (0 = no limit)")
	cmd.Flags().BoolVar(&flagVerifyFiles, "verify-files", false, "Verify each PDF attachment's file exists on disk (one local-API lookup per attachment)")

	return cmd
}

type itemsAuditCheck struct {
	name  string
	query func(localQueryStore, int) ([]map[string]any, error)
}

func selectedItemsAuditChecks(missingPDF, missingAbstract, missingDOI, missingTags, missingCitation bool) []itemsAuditCheck {
	checks := make([]itemsAuditCheck, 0, 5)
	if missingPDF {
		checks = append(checks, itemsAuditCheck{name: "missing_pdf", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return queryMissingPDFItems(db, "", limit, "")
		}})
	}
	if missingAbstract {
		checks = append(checks, itemsAuditCheck{name: "missing_abstract", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return queryMissingAbstractItems(db, limit, "")
		}})
	}
	if missingDOI {
		checks = append(checks, itemsAuditCheck{name: "missing_doi", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return queryMissingDOIItems(db, limit, "")
		}})
	}
	if missingTags {
		checks = append(checks, itemsAuditCheck{name: "missing_tags", query: queryMissingTagsItems})
	}
	// PATCH(glean dxut): citation-readiness check — items that cannot be cited
	// because a core field is missing.
	if missingCitation {
		checks = append(checks, itemsAuditCheck{name: "missing_citation", query: queryCitationIncompleteItems})
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
	COUNT(CASE WHEN COALESCE(json_array_length(json_extract(data, '$.data.tags')), 0) = 0 THEN 1 END) AS missing_tags,
	COUNT(CASE WHEN json_extract(data, '$.data.itemType') NOT IN ('attachment', 'annotation', 'note') AND ` + citationIncompletePredicate + ` THEN 1 END) AS missing_citation
FROM resources
WHERE resource_type = 'items'`)
	if err != nil {
		return itemsAuditSummary{}, err
	}
	var missingAbstract, missingDOI, missingTags, missingCitation int
	if len(rows) > 0 {
		missingAbstract = sqlIntValue(rows[0]["missing_abstract"])
		missingDOI = sqlIntValue(rows[0]["missing_doi"])
		missingTags = sqlIntValue(rows[0]["missing_tags"])
		missingCitation = sqlIntValue(rows[0]["missing_citation"])
	}
	return itemsAuditSummary{
		MissingPDF:      missingPDF,
		MissingAbstract: missingAbstract,
		MissingDOI:      missingDOI,
		MissingTags:     missingTags,
		MissingCitation: missingCitation,
	}, nil
}

func printItemsAuditSummary(cmd *cobra.Command, summary itemsAuditSummary) error {
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "Check\tCount")
	fmt.Fprintf(tw, "%s\t%d\n", "missing-pdf", summary.MissingPDF)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-abstract", summary.MissingAbstract)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-doi", summary.MissingDOI)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-tags", summary.MissingTags)
	fmt.Fprintf(tw, "%s\t%d\n", "missing-citation", summary.MissingCitation)
	return tw.Flush()
}

func queryMissingAbstractItems(db localQueryStore, limit int, collection string) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.title') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	json_extract(data, '$.data.DOI') AS doi,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND (json_extract(data, '$.data.abstractNote') IS NULL OR TRIM(json_extract(data, '$.data.abstractNote')) = '')`
	// PATCH(glean bugfix): let items enrich scope missing-abstract candidates to a collection.
	args := enrichCollectionFilterArgs(&query, "data", collection)
	query += `
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit, args...)
}

func queryMissingDOIItems(db localQueryStore, limit int, collection string) ([]map[string]any, error) {
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
	AND (json_extract(data, '$.data.DOI') IS NULL OR TRIM(json_extract(data, '$.data.DOI')) = '')`
	// PATCH(glean bugfix): let items enrich scope missing-DOI candidates to a collection.
	args := enrichCollectionFilterArgs(&query, "data", collection)
	query += `
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit, args...)
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

func queryItemsAuditRows(db localQueryStore, query string, limit int, args ...any) ([]map[string]any, error) {
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	return db.QueryRaw(query, args...)
}

func enrichCollectionFilterArgs(query *string, dataExpr string, collection string) []any {
	if collection == "" {
		return nil
	}
	*query += `
	AND EXISTS (SELECT 1 FROM json_each(json_extract(` + dataExpr + `,'$.data.collections')) WHERE value = ?)`
	return []any{collection}
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

// citationIncompletePredicate matches citeable items missing a core citation
// field. PATCH(glean dxut): shared by the audit summary scan and the
// --missing-citation listing so the count and the list never drift.
const citationIncompletePredicate = `(
	COALESCE(json_array_length(json_extract(data, '$.data.creators')), 0) = 0
	OR TRIM(COALESCE(json_extract(data, '$.data.title'), '')) = ''
	OR TRIM(COALESCE(json_extract(data, '$.data.date'), '')) = ''
	OR (json_extract(data, '$.data.itemType') IN ('journalArticle', 'conferencePaper', 'preprint') AND TRIM(COALESCE(json_extract(data, '$.data.publicationTitle'), '')) = '')
	OR (json_extract(data, '$.data.itemType') = 'book' AND TRIM(COALESCE(json_extract(data, '$.data.publisher'), '')) = '')
)`

// queryCitationIncompleteItems lists citeable items missing core citation fields,
// annotating each row with the specific fields it lacks. PATCH(glean dxut).
func queryCitationIncompleteItems(db localQueryStore, limit int) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	COALESCE(json_extract(data, '$.data.title'), '') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	COALESCE(json_array_length(json_extract(data, '$.data.creators')), 0) AS n_creators,
	TRIM(COALESCE(json_extract(data, '$.data.date'), '')) AS date,
	TRIM(COALESCE(json_extract(data, '$.data.publicationTitle'), '')) AS publication_title,
	TRIM(COALESCE(json_extract(data, '$.data.publisher'), '')) AS publisher,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND json_extract(data, '$.data.itemType') NOT IN ('attachment', 'annotation', 'note')
	AND ` + citationIncompletePredicate + `
ORDER BY date_added DESC`
	rows, err := queryItemsAuditRows(db, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"key":       sqlStringValue(r["key"]),
			"title":     sqlStringValue(r["title"]),
			"item_type": sqlStringValue(r["item_type"]),
			"missing":   strings.Join(citationMissingFields(r), ", "),
		})
	}
	return out, nil
}

// citationMissingFields returns the core citation fields absent from a row
// produced by queryCitationIncompleteItems.
func citationMissingFields(r map[string]any) []string {
	var missing []string
	if sqlIntValue(r["n_creators"]) == 0 {
		missing = append(missing, "creators")
	}
	if sqlStringValue(r["title"]) == "" {
		missing = append(missing, "title")
	}
	if sqlStringValue(r["date"]) == "" {
		missing = append(missing, "date")
	}
	switch sqlStringValue(r["item_type"]) {
	case "journalArticle", "conferencePaper", "preprint":
		if sqlStringValue(r["publication_title"]) == "" {
			missing = append(missing, "publicationTitle")
		}
	case "book":
		if sqlStringValue(r["publisher"]) == "" {
			missing = append(missing, "publisher")
		}
	}
	return missing
}

// runVerifyAttachmentFiles checks that every PDF attachment's file is present on
// disk, resolving each path via the local API and stat-ing it. PATCH(glean dxut).
func runVerifyAttachmentFiles(cmd *cobra.Command, db localQueryStore, flags *rootFlags, limit int) error {
	c, err := flags.newClient()
	if err != nil {
		return err
	}
	attachments, err := queryPDFAttachments(db, limit)
	if err != nil {
		return fmt.Errorf("querying PDF attachments: %w", err)
	}
	broken := make([]map[string]any, 0)
	for _, a := range attachments {
		key := sqlStringValue(a["key"])
		path, reason := attachmentFileStatus(c, key)
		if reason == "" {
			continue
		}
		broken = append(broken, map[string]any{
			"key":    key,
			"parent": sqlStringValue(a["parent"]),
			"name":   sqlStringValue(a["name"]),
			"path":   path,
			"reason": reason,
		})
	}
	if flags.asJSON {
		data, err := json.Marshal(map[string]any{"checked": len(attachments), "broken": broken})
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Checked %d PDF attachment(s); %d missing on disk.\n", len(attachments), len(broken))
	for _, b := range broken {
		fmt.Fprintf(out, "  [%s] %s — %s (%s)\n", sqlStringValue(b["reason"]), sqlStringValue(b["key"]), sqlStringValue(b["name"]), sqlStringValue(b["path"]))
	}
	return nil
}

// attachmentFileStatus resolves an attachment's on-disk path via the local API
// and stats it. reason is "" when the file is present, else the failure cause.
func attachmentFileStatus(c *client.Client, key string) (path, reason string) {
	fileURL, ok := fetchAttachmentFileURL(c, key)
	if !ok || fileURL == "" {
		return "", "unresolved"
	}
	path = fileURLToPath(fileURL)
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return path, "missing"
	case info.IsDir():
		return path, "not-a-file"
	default:
		return path, ""
	}
}

// queryPDFAttachments lists PDF attachments that should have a local file
// (excludes linked_url web bookmarks). PATCH(glean dxut).
func queryPDFAttachments(db localQueryStore, limit int) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	json_extract(data, '$.data.parentItem') AS parent,
	COALESCE(json_extract(data, '$.data.filename'), json_extract(data, '$.data.title'), '') AS name,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND json_extract(data, '$.data.itemType') = 'attachment'
	AND json_extract(data, '$.data.contentType') = 'application/pdf'
	AND COALESCE(json_extract(data, '$.data.linkMode'), '') IN ('imported_file', 'linked_file', 'imported_url')
ORDER BY date_added DESC`
	return queryItemsAuditRows(db, query, limit)
}
