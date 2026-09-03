// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

// --- shared --scope adoption ---
//
// `items audit`, `items enrich`, `items summarize` and `vault sync` all pick an
// item cohort out of the synced local store, so they reach the one grammar in
// scope.go instead of each growing its own selection dialect. The flag's help
// text and defaults live beside that grammar (scopeFlagUsage and friends in
// scope.go); these are the resolution helpers the four share. They live here
// because `items audit` is the read that defines the work queues, and this file
// already hosts the selection SQL the audit/enrich pair shares
// (enrichCollectionFilterArgs, the citation predicates, sqlStringValue).

// scopeSelection is a resolved cohort in the form these commands consume: an
// allow-set of item keys. A nil Keys map means unrestricted, which is what both
// `--scope library` and an absent --scope produce, so an unscoped run keeps
// taking exactly the query path it took before --scope existed.
type scopeSelection struct {
	Expr  string
	Type  string
	Value string
	Keys  map[string]bool
}

func (s scopeSelection) restricted() bool { return s.Keys != nil }

// allows reports cohort membership. A nil Keys map admits every key, so callers
// can filter with one predicate instead of branching on restricted() first.
func (s scopeSelection) allows(key string) bool {
	return s.Keys == nil || s.Keys[key]
}

// queryLimit drops the SQL LIMIT for a query whose rows still have to pass the
// cohort filter: LIMIT would cut rows before the filter runs and report fewer
// items than the cohort actually holds.
func (s scopeSelection) queryLimit(limit int) int {
	if s.restricted() {
		return 0
	}
	return limit
}

// resolveScopeSelection is the single adoption point for --scope across these
// four commands. An empty expr means no --scope and yields an unrestricted
// selection.
//
// saved-search:KEY carries scope.go's live_local_api precondition because a
// saved search is evaluated by Zotero and has no local mirror. It therefore
// refuses through the shared precondition_unmet emitter rather than operating on
// the empty cohort resolveScope hands back.
func resolveScopeSelection(cmd *cobra.Command, flags *rootFlags, capability string, db localQueryStore, expr string) (scopeSelection, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return scopeSelection{}, nil
	}
	spec, err := parseScopeSpec(expr)
	if err != nil {
		return scopeSelection{}, usageErr(err)
	}
	result, err := resolveScope(db, spec)
	if err != nil {
		return scopeSelection{}, err
	}
	if result.Precondition != "" {
		out := cmd.OutOrStdout()
		if flags != nil && flags.quiet {
			out = nil
		}
		return scopeSelection{}, emitPreconditionUnmetWithRemediation(
			out,
			flags,
			capability,
			result.Precondition,
			fmt.Sprintf("scope %q is evaluated by Zotero and has no local mirror, so it resolves no items while the desktop local API is unreachable", result.Expr),
			remediationFor(cmd.Context(), flags, result.Precondition),
		)
	}
	sel := scopeSelection{Expr: result.Expr, Type: result.Type, Value: spec.Value}
	if !result.All {
		sel.Keys = make(map[string]bool, len(result.Keys))
		for _, key := range result.Keys {
			sel.Keys[key] = true
		}
	}
	return sel, nil
}

// scopeSugarFlag pairs one of a command's own cohort flags with the scope
// expression it means. Both spellings stay supported — README.md, SKILL.md and
// docs/ document the older one — but a run that sets both and means two
// different cohorts is a caller bug: silently preferring either would operate on
// a set the author never asked for.
type scopeSugarFlag struct {
	name string
	set  bool
	// expr is empty when the grammar has no arm for this flag, which makes the
	// combination unreconcilable rather than merely disagreeing.
	expr string
}

// scopeSugarFor lowers a bespoke cohort flag to its scope expression.
func scopeSugarFor(name, scopeType, value string) scopeSugarFlag {
	value = strings.TrimSpace(value)
	if value == "" {
		return scopeSugarFlag{name: name}
	}
	return scopeSugarFlag{name: name, set: true, expr: scopeType + ":" + value}
}

// reconcileScopeFlags returns the effective scope expression for a run. It is
// empty when --scope was absent, which leaves the bespoke flags on the query
// path they already had.
func reconcileScopeFlags(scopeExpr string, sugar ...scopeSugarFlag) (string, error) {
	scopeExpr = strings.TrimSpace(scopeExpr)
	if scopeExpr == "" {
		return "", nil
	}
	for _, s := range sugar {
		if !s.set {
			continue
		}
		if s.expr == "" {
			return "", usageErr(fmt.Errorf("--%s cannot be combined with --scope: the scope grammar has no --%s arm, so the two selections cannot be reconciled; select the whole cohort with --scope, or drop --scope and keep --%s", s.name, s.name, s.name))
		}
		if s.expr != scopeExpr {
			return "", usageErr(fmt.Errorf("--%s and --scope disagree: --%s selects %q while --scope selects %q; pass one of them", s.name, s.name, s.expr, scopeExpr))
		}
	}
	return scopeExpr, nil
}

// scopedAuditRows runs a work-queue query and narrows it to the cohort. The
// cohort filter runs in Go rather than as a SQL IN-list because a cohort has no
// size ceiling while SQLite's bound-variable count does (enrichKeyChunkSize
// exists for the same reason), so the LIMIT has to move after the filter.
func scopedAuditRows(sel scopeSelection, limit int, query func(int) ([]map[string]any, error)) ([]map[string]any, error) {
	rows, err := query(sel.queryLimit(limit))
	if err != nil {
		return nil, err
	}
	if !sel.restricted() {
		return rows, nil
	}
	return applyEnrichQueueLimit(filterEnrichRowsByKeys(rows, sel.Keys), limit), nil
}

type itemsAuditSummary struct {
	// TopLevelItems is the denominator every count below is measured against.
	// Without it the counts read as fractions of an unstated whole.
	TopLevelItems   int       `json:"top_level_items"`
	MissingPDF      int       `json:"missing_pdf"`
	MissingAbstract int       `json:"missing_abstract"`
	MissingDOI      int       `json:"missing_doi"`
	MissingTags     int       `json:"missing_tags"`
	MissingCitation int       `json:"missing_citation"`
	Findings        []Finding `json:"findings"`
}

// items audit is intentionally report-shaped rather than a list read: its JSON
// payload contains named check arrays and summary counters, plus findings. The
// generated command reference documents this and journal show as explicit
// exceptions from the {meta, results} envelope used by ordinary reads.
func newItemsAuditCmd(flags *rootFlags) *cobra.Command {
	var flagMissingPDF bool
	var flagMissingAbstract bool
	var flagMissingDOI bool
	var flagMissingTags bool
	var flagCitations bool
	var flagLimit int
	var flagVerifyFiles bool
	var flagScope string

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

			sel, err := resolveScopeSelection(cmd, flags, "items audit", db, flagScope)
			if err != nil {
				return err
			}

			if flagVerifyFiles {
				return runVerifyAttachmentFiles(cmd, db, flags, flagLimit, sel)
			}

			checks := selectedItemsAuditChecks(flagMissingPDF, flagMissingAbstract, flagMissingDOI, flagMissingTags, flagCitations, sel)
			if len(checks) == 0 {
				summary, err := itemsAuditSummaryForScope(db, sel)
				if err != nil {
					return fmt.Errorf("querying item audit summary: %w", err)
				}
				if flags.asJSON {
					summary.Findings = []Finding{}
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
			if flags.asJSON {
				out := make(map[string]any, len(results)+1)
				for _, check := range checks {
					out[check.name] = results[check.name]
				}
				out["findings"] = itemsAuditFindingsForChecks(checks, results)
				data, err := json.Marshal(out)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
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
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultUnset, scopeFlagUsageDefaultLibrary)

	return cmd
}

type itemsAuditCheck struct {
	name  string
	query func(localQueryStore, int) ([]map[string]any, error)
}

// selectedItemsAuditChecks binds the requested checks to one resolved cohort.
// The cohort is baked into each closure so every check narrows through the same
// scopedAuditRows path and the per-check limit keeps applying to the cohort
// rather than to the whole library.
func selectedItemsAuditChecks(missingPDF, missingAbstract, missingDOI, missingTags, missingCitation bool, sel scopeSelection) []itemsAuditCheck {
	checks := make([]itemsAuditCheck, 0, 5)
	if missingPDF {
		checks = append(checks, itemsAuditCheck{name: "missing_pdf", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return scopedAuditRows(sel, limit, func(queryLimit int) ([]map[string]any, error) {
				return queryMissingPDFItems(db, "", queryLimit, "")
			})
		}})
	}
	if missingAbstract {
		checks = append(checks, itemsAuditCheck{name: "missing_abstract", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return scopedAuditRows(sel, limit, func(queryLimit int) ([]map[string]any, error) {
				return queryMissingAbstractItems(db, queryLimit, "")
			})
		}})
	}
	if missingDOI {
		checks = append(checks, itemsAuditCheck{name: "missing_doi", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return scopedAuditRows(sel, limit, func(queryLimit int) ([]map[string]any, error) {
				return queryMissingDOIItems(db, queryLimit, "")
			})
		}})
	}
	if missingTags {
		checks = append(checks, itemsAuditCheck{name: "missing_tags", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return scopedAuditRows(sel, limit, func(queryLimit int) ([]map[string]any, error) {
				return queryMissingTagsItems(db, queryLimit)
			})
		}})
	}
	// Citation-readiness check — items that cannot be cited
	// because a core field is missing.
	if missingCitation {
		checks = append(checks, itemsAuditCheck{name: "missing_citation", query: func(db localQueryStore, limit int) ([]map[string]any, error) {
			return scopedAuditRows(sel, limit, func(queryLimit int) ([]map[string]any, error) {
				return queryCitationIncompleteItems(db, queryLimit, "")
			})
		}})
	}
	return checks
}

func itemsAuditFindingsForChecks(checks []itemsAuditCheck, results map[string][]map[string]any) []Finding {
	findings := make([]Finding, 0)
	for _, check := range checks {
		for _, row := range results[check.name] {
			finding := Finding{
				Kind:        check.name,
				Severity:    itemsAuditFindingSeverity(check.name),
				ItemKey:     sqlStringValue(row["key"]),
				Title:       sqlStringValue(row["title"]),
				Source:      FindingSource{Kind: "local"},
				Autofixable: itemsAuditFindingAutofixable(check.name),
			}
			if action := itemsAuditFindingAction(check.name); action != nil {
				finding.RecommendedAction = action
			}
			evidence := map[string]any{}
			for _, field := range []string{"item_type", "doi", "date_added", "missing"} {
				if value := sqlStringValue(row[field]); value != "" {
					evidence[field] = value
				}
			}
			if len(evidence) > 0 {
				finding.Evidence = evidence
			}
			findings = append(findings, finding)
		}
	}
	return findings
}

func itemsAuditFindingSeverity(kind string) string {
	switch kind {
	case "missing_doi", "missing_pdf", "missing_citation":
		return sevHigh
	default:
		return sevInfo
	}
}

func itemsAuditFindingAutofixable(kind string) bool {
	switch kind {
	case "missing_doi", "missing_pdf", "missing_abstract", "missing_citation":
		return true
	default:
		return false
	}
}

func itemsAuditFindingAction(kind string) *RecommendedAction {
	switch kind {
	case "missing_doi":
		return &RecommendedAction{Command: "zotio items enrich --missing-doi --keys-from -"}
	case "missing_pdf":
		return &RecommendedAction{Command: "zotio items enrich --missing-pdf --keys-from -"}
	case "missing_abstract":
		return &RecommendedAction{Command: "zotio items enrich --missing-abstract --keys-from -"}
	case "missing_citation":
		return &RecommendedAction{Command: "zotio items enrich --missing-citation --keys-from -"}
	default:
		return nil
	}
}

// libraryTopLevelItemsPredicate is defined in library_health.go, the shared
// home for "how many items does this library have" — see its doc comment.
// Attachments, notes and annotations cannot carry an abstract, a DOI or tags,
// so counting them made every denominator meaningless — a 928-item library
// reported 4018 "missing tags", mostly PDFs.

// itemsAuditSummaryForScope keeps the unscoped summary on its folded
// single-scan aggregate below. A scoped summary cannot use it: the cohort is a
// key set, not a SQL predicate, and an IN-list sized to an arbitrary cohort
// would exceed SQLite's bound-variable ceiling. It counts exactly the rows the
// list mode returns for the same cohort, so a summary counter can never
// disagree with the list that explains it.
func itemsAuditSummaryForScope(db localQueryStore, sel scopeSelection) (itemsAuditSummary, error) {
	if !sel.restricted() {
		return queryItemsAuditSummary(db)
	}
	topLevel, err := db.QueryRaw(`
SELECT id AS key
FROM resources
WHERE resource_type = 'items'
	AND ` + libraryTopLevelItemsPredicate)
	if err != nil {
		return itemsAuditSummary{}, err
	}
	summary := itemsAuditSummary{TopLevelItems: len(filterEnrichRowsByKeys(topLevel, sel.Keys))}
	counters := []struct {
		into  *int
		query func(int) ([]map[string]any, error)
	}{
		{&summary.MissingPDF, func(limit int) ([]map[string]any, error) { return queryMissingPDFItems(db, "", limit, "") }},
		{&summary.MissingAbstract, func(limit int) ([]map[string]any, error) { return queryMissingAbstractItems(db, limit, "") }},
		{&summary.MissingDOI, func(limit int) ([]map[string]any, error) { return queryMissingDOIItems(db, limit, "") }},
		{&summary.MissingTags, func(limit int) ([]map[string]any, error) { return queryMissingTagsItems(db, limit) }},
		{&summary.MissingCitation, func(limit int) ([]map[string]any, error) { return queryCitationIncompleteItems(db, limit, "") }},
	}
	for _, counter := range counters {
		rows, err := scopedAuditRows(sel, 0, counter.query)
		if err != nil {
			return itemsAuditSummary{}, err
		}
		*counter.into = len(rows)
	}
	return summary, nil
}

func queryItemsAuditSummary(db localQueryStore) (itemsAuditSummary, error) {
	missingPDF, err := queryMissingPDFCount(db)
	if err != nil {
		return itemsAuditSummary{}, err
	}
	// Fold the three single-row predicate counts
	// (abstract/DOI/tags) into one table scan with conditional aggregation
	// instead of three separate COUNT scans. The PDF count keeps its own query
	// because it needs the attachment anti-join; the DOI predicate uses the
	// indexed item_type column.
	rows, err := db.QueryRaw(`
SELECT
	COUNT(*) AS top_level_items,
	COUNT(CASE WHEN json_extract(data, '$.data.abstractNote') IS NULL OR TRIM(json_extract(data, '$.data.abstractNote')) = '' THEN 1 END) AS missing_abstract,
	COUNT(CASE WHEN item_type IN ('journalArticle', 'conferencePaper', 'preprint')
		AND (json_extract(data, '$.data.DOI') IS NULL OR TRIM(json_extract(data, '$.data.DOI')) = '') THEN 1 END) AS missing_doi,
	COUNT(CASE WHEN COALESCE(json_array_length(json_extract(data, '$.data.tags')), 0) = 0 THEN 1 END) AS missing_tags,
	COUNT(CASE WHEN ` + citationIncompletePredicate + ` THEN 1 END) AS missing_citation
FROM resources
WHERE resource_type = 'items'
	AND ` + libraryTopLevelItemsPredicate)
	if err != nil {
		return itemsAuditSummary{}, err
	}
	var topLevel, missingAbstract, missingDOI, missingTags, missingCitation int
	if len(rows) > 0 {
		topLevel = sqlIntValue(rows[0]["top_level_items"])
		missingAbstract = sqlIntValue(rows[0]["missing_abstract"])
		missingDOI = sqlIntValue(rows[0]["missing_doi"])
		missingTags = sqlIntValue(rows[0]["missing_tags"])
		missingCitation = sqlIntValue(rows[0]["missing_citation"])
	}
	return itemsAuditSummary{
		TopLevelItems:   topLevel,
		MissingPDF:      missingPDF,
		MissingAbstract: missingAbstract,
		MissingDOI:      missingDOI,
		MissingTags:     missingTags,
		MissingCitation: missingCitation,
	}, nil
}

func printItemsAuditSummary(cmd *cobra.Command, summary itemsAuditSummary) error {
	// State the denominator: the counts below are otherwise fractions of an
	// unstated whole, and the store holds several times more rows than items.
	fmt.Fprintf(cmd.OutOrStdout(), "Scope: %d top-level items\n", summary.TopLevelItems)
	rows := [][]string{
		{"missing-pdf", strconv.Itoa(summary.MissingPDF)},
		{"missing-abstract", strconv.Itoa(summary.MissingAbstract)},
		{"missing-doi", strconv.Itoa(summary.MissingDOI)},
		{"missing-tags", strconv.Itoa(summary.MissingTags)},
		{"missing-citation", strconv.Itoa(summary.MissingCitation)},
	}
	return renderColumns(cmd.OutOrStdout(), []string{"check", "count"}, rows)
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
	AND ` + libraryTopLevelItemsPredicate + `
	AND (json_extract(data, '$.data.abstractNote') IS NULL OR TRIM(json_extract(data, '$.data.abstractNote')) = '')`
	// Let items enrich scope missing-abstract candidates to a collection.
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
	AND ` + libraryTopLevelItemsPredicate + `
	AND item_type IN ('journalArticle', 'conferencePaper', 'preprint')
	AND (json_extract(data, '$.data.DOI') IS NULL OR TRIM(json_extract(data, '$.data.DOI')) = '')`
	// Let items enrich scope missing-DOI candidates to a collection.
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
	AND ` + libraryTopLevelItemsPredicate + `
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

// citationVenueFields maps a citeable item type to the one Zotero field that
// carries its venue. The field name differs per type and only journalArticle
// has publicationTitle: conferencePaper has proceedingsTitle, preprint has
// repository, and book has publisher. Testing the wrong field name flags every
// item of that type forever, because Zotero never stores a field its type does
// not define. One table therefore generates both the SQL predicate and the
// missing-field annotation so the two can never drift.
var citationVenueFields = map[string]string{
	"journalArticle":  "publicationTitle",
	"conferencePaper": "proceedingsTitle",
	"preprint":        "repository",
	"book":            "publisher",
}

// citationRenderFields maps an item type to the fields a reference needs to
// PRINT completely, as opposed to the fields it needs to be identified. Only
// types whose Zotero schema defines them appear: journalArticle and
// conferencePaper carry volume, issue and pages, and bookSection carries volume
// and pages but has no issue.
//
// These deliberately do NOT enter citationIncompletePredicate. A blank volume
// is not evidence of a defect: an article-number journal such as PLOS ONE has
// no volume and no page range, a conference paper usually has no volume, and a
// book section in a single-volume book has none either. Measured on a
// 1216-item library, requiring them locally would flag 20 of 20 conference
// papers and 29 of 30 book sections - the same "field the type never fills"
// error the venue arm used to make. Only the provider can tell a missing
// volume from a volume that does not exist, so these are measured by
// `items enrich --validate` against CrossRef and filled by the fixer, never
// asserted by a local predicate that gates CI.
var citationRenderFields = map[string][]string{
	"journalArticle":  {"volume", "issue", "pages"},
	"conferencePaper": {"volume", "issue", "pages"},
	"bookSection":     {"volume", "pages"},
}

// citationRenderTypes is citationRenderFields in a fixed order, so generated
// SQL is byte-identical between processes.
func citationRenderTypes() []string {
	types := make([]string, 0, len(citationRenderFields))
	for itemType := range citationRenderFields {
		types = append(types, itemType)
	}
	sort.Strings(types)
	return types
}

// citationRenderBlankPredicate matches a DOI-bearing item of a render-field
// type with at least one of those fields empty. It selects the FIXER's work
// queue, never a health finding: over-selecting here costs one provider lookup
// and a recorded skip, whereas over-selecting in a check would gate CI on a
// field the venue never had.
func citationRenderBlankPredicate() string {
	var arms []string
	for _, itemType := range citationRenderTypes() {
		var blanks []string
		for _, field := range citationRenderFields[itemType] {
			blanks = append(blanks, "TRIM(COALESCE(json_extract(data, '$.data."+field+"'), '')) = ''")
		}
		arms = append(arms, "(json_extract(data, '$.data.itemType') = '"+itemType+"' AND ("+strings.Join(blanks, " OR ")+"))")
	}
	return `(
	TRIM(COALESCE(json_extract(data, '$.data.DOI'), '')) <> ''
	AND (` + strings.Join(arms, "\n\t\tOR ") + `)
)`
}

// citationVenueTypes is citationVenueFields in a fixed order, so every
// generated SQL string is byte-identical between processes.
func citationVenueTypes() []string {
	types := make([]string, 0, len(citationVenueFields))
	for itemType := range citationVenueFields {
		types = append(types, itemType)
	}
	sort.Strings(types)
	return types
}

// citationVenueExpr is the SQL for the venue value of whichever type the row
// holds, and citationVenueFieldExpr is the SQL for that field's name. Both are
// empty strings for a type with no venue field, which is how the predicate
// below excludes such types from the venue arm.
func citationVenueExpr(valueOfField func(string) string) string {
	var b strings.Builder
	b.WriteString("CASE json_extract(data, '$.data.itemType')")
	for _, itemType := range citationVenueTypes() {
		b.WriteString("\n\t\tWHEN '" + itemType + "' THEN " + valueOfField(citationVenueFields[itemType]))
	}
	b.WriteString("\n\t\tELSE '' END")
	return b.String()
}

func citationVenueValueSQL() string {
	return citationVenueExpr(func(field string) string {
		return "TRIM(COALESCE(json_extract(data, '$.data." + field + "'), ''))"
	})
}

func citationVenueFieldSQL() string {
	return citationVenueExpr(func(field string) string { return "'" + field + "'" })
}

// citationIncompletePredicate matches citeable items missing a core citation
// field. Shared by the audit summary scan and the
// --missing-citation listing so the count and the list never drift.
var citationIncompletePredicate = `(
	COALESCE(json_array_length(json_extract(data, '$.data.creators')), 0) = 0
	OR TRIM(COALESCE(json_extract(data, '$.data.title'), '')) = ''
	OR TRIM(COALESCE(json_extract(data, '$.data.date'), '')) = ''
	OR (` + citationVenueFieldSQL() + ` <> '' AND ` + citationVenueValueSQL() + ` = '')
)`

// queryCitationIncompleteItems lists citeable items missing core citation fields,
// annotating each row with the specific fields it lacks. Pass a collection to
// scope the queue for `items enrich --missing-citation --collection`.
func queryCitationIncompleteItems(db localQueryStore, limit int, collection string) ([]map[string]any, error) {
	return queryCitationRows(db, citationIncompletePredicate, limit, collection)
}

// queryCitationEnrichCandidates is the FIXER's work queue: everything the check
// reports, plus DOI-bearing items whose render fields are blank. It is
// deliberately wider than the check, because the fixer verifies every candidate
// against CrossRef before proposing anything - an item whose venue simply has
// no volume yields a recorded skip, not a finding. Widening the check the same
// way would gate CI on a field the venue never had.
func queryCitationEnrichCandidates(db localQueryStore, limit int, collection string) ([]map[string]any, error) {
	return queryCitationRows(db, "("+citationIncompletePredicate+"\n\tOR "+citationRenderBlankPredicate()+")", limit, collection)
}

func queryCitationRows(db localQueryStore, predicate string, limit int, collection string) ([]map[string]any, error) {
	query := `
SELECT
	id AS key,
	COALESCE(json_extract(data, '$.data.title'), '') AS title,
	json_extract(data, '$.data.itemType') AS item_type,
	COALESCE(json_array_length(json_extract(data, '$.data.creators')), 0) AS n_creators,
	TRIM(COALESCE(json_extract(data, '$.data.date'), '')) AS date,
	` + citationVenueValueSQL() + ` AS venue,
	` + citationVenueFieldSQL() + ` AS venue_field,
	json_extract(data, '$.data.dateAdded') AS date_added
FROM resources
WHERE resource_type = 'items'
	AND ` + libraryTopLevelItemsPredicate + `
	AND ` + predicate
	args := enrichCollectionFilterArgs(&query, "data", collection)
	query += `
ORDER BY date_added DESC`
	rows, err := queryItemsAuditRows(db, query, limit, args...)
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
			// date_added carries the same ordering key every peer queue
			// selects, so a mixed-category enrich queue sorts consistently.
			"date_added": sqlStringValue(r["date_added"]),
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
	if field := sqlStringValue(r["venue_field"]); field != "" && sqlStringValue(r["venue"]) == "" {
		missing = append(missing, field)
	}
	return missing
}

// runVerifyAttachmentFiles checks that every PDF attachment's file is present on
// disk, resolving each path via the local API and stat-ing it.
//
// A cohort names items, never attachments, so --scope narrows this to the PDF
// children of the cohort's items. Ignoring the cohort here would silently stat
// the whole library after the operator asked for one collection.
func runVerifyAttachmentFiles(cmd *cobra.Command, db localQueryStore, flags *rootFlags, limit int, sel scopeSelection) error {
	c, err := flags.newClient()
	if err != nil {
		return err
	}
	attachments, err := queryPDFAttachments(db, sel.queryLimit(limit))
	if err != nil {
		return fmt.Errorf("querying PDF attachments: %w", err)
	}
	if sel.restricted() {
		attachments = applyEnrichQueueLimit(filterAuditRowsByParent(attachments, sel), limit)
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
		data, err := json.Marshal(map[string]any{
			"checked":  len(attachments),
			"broken":   broken,
			"findings": brokenAttachmentFindings(broken),
		})
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

func brokenAttachmentFindings(broken []map[string]any) []Finding {
	findings := make([]Finding, 0, len(broken))
	for _, b := range broken {
		findings = append(findings, Finding{
			Kind:     "broken_attachment_file",
			Severity: sevCritical,
			ItemKey:  sqlStringValue(b["key"]),
			Title:    sqlStringValue(b["name"]),
			Evidence: map[string]any{
				"parent": sqlStringValue(b["parent"]),
				"path":   sqlStringValue(b["path"]),
				"reason": sqlStringValue(b["reason"]),
			},
			Source:            FindingSource{Kind: "local"},
			RecommendedAction: &RecommendedAction{Text: "Re-link the file in Zotero or re-download the attachment"},
		})
	}
	return findings
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
// (excludes linked_url web bookmarks).
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

// filterAuditRowsByParent narrows attachment rows to the cohort that owns them.
// filterEnrichRowsByKeys cannot serve here: it matches the row's own key, and an
// attachment's key is never a member of an item cohort.
func filterAuditRowsByParent(rows []map[string]any, sel scopeSelection) []map[string]any {
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if sel.allows(sqlStringValue(row["parent"])) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}
