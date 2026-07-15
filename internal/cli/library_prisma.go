// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type libraryPrismaReport struct {
	SchemaVersion             int            `json:"schema_version"`
	Scope                     string         `json:"scope"`
	By                        string         `json:"by"`
	SyncedAt                  string         `json:"synced_at"`
	Identified                prismaCorpus   `json:"identified"`
	DuplicateClusters         int            `json:"duplicate_clusters"`
	DuplicateRecordsRemoved   int            `json:"duplicate_records_removed"`
	RecordsAfterDeduplication int            `json:"records_after_deduplication"`
	Prisma                    prismaFlowData `json:"prisma"`
}

type prismaCorpus struct {
	Total    int            `json:"total"`
	BySource map[string]int `json:"by_source"`
}

type prismaFlowData struct {
	RecordsIdentified       int `json:"records_identified"`
	DuplicateRecordsRemoved int `json:"duplicate_records_removed"`
	RecordsScreenedInput    int `json:"records_screened_input"`
}

type prismaCorpusItem struct {
	Key    string
	Source string
}

func newLibraryPrismaCmd(flags *rootFlags) *cobra.Command {
	var flagScope string
	var flagBy string

	cmd := &cobra.Command{
		Use:   "prisma",
		Short: "Report PRISMA 2020 identification-stage counts from the local store",
		Long: `Report the PRISMA 2020 identification-stage counts for a locally synced screening corpus.

The report counts records identified (including their source databases), duplicate
records removed, and the records-after-deduplication input to screening. Screening
itself is deliberately out of scope: use Rayyan, ASReview, or another screening tool
for that stage. Run 'zotio items duplicates resolve' to actually remove duplicates
before exporting the certified corpus.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			by := strings.ToLower(strings.TrimSpace(flagBy))
			switch by {
			case "doi", "title", "all":
			default:
				return usageErr(fmt.Errorf("invalid --by value %q: must be doi, title, or all", flagBy))
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first to enable duplicate detection.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{Store: rawDB}

			scope := scopeResult{All: true, Expr: "library"}
			if strings.TrimSpace(flagScope) != "" {
				spec, perr := parseScopeSpec(flagScope)
				if perr != nil {
					return usageErr(perr)
				}
				scope, err = resolveScope(db, spec)
				if err != nil {
					return err
				}
				if scope.Precondition != "" {
					return preconditionErr(fmt.Errorf("scope %q needs the %s precondition (Zotero desktop / local API); open Zotero and enable Settings -> Advanced -> 'Allow other applications', then re-run", scope.Expr, scope.Precondition))
				}
			}

			report, err := assembleLibraryPrismaReport(db, scope, by)
			if err != nil {
				return fmt.Errorf("assembling PRISMA report: %w", err)
			}
			if flags.asJSON {
				data, err := json.Marshal(report)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			return printLibraryPrisma(cmd, report)
		},
	}
	cmd.Flags().StringVar(&flagScope, "scope", "", "Limit to library, collection:<key>, tag:<name>, item:<key>, or query:<text>")
	cmd.Flags().StringVar(&flagBy, "by", "all", "Duplicate detector to run (doi, title, all)")

	return cmd
}

func assembleLibraryPrismaReport(db localQueryStore, scope scopeResult, by string) (libraryPrismaReport, error) {
	items, err := queryPrismaCorpus(db, scope)
	if err != nil {
		return libraryPrismaReport{}, err
	}

	identified := prismaCorpus{BySource: make(map[string]int)}
	keys := make(map[string]struct{}, len(items))
	for _, item := range items {
		identified.Total++
		identified.BySource[item.Source]++
		keys[item.Key] = struct{}{}
	}

	rows, err := queryPrismaDuplicateRows(db, by, keys)
	if err != nil {
		return libraryPrismaReport{}, err
	}
	clusters := mergePrismaDuplicateGroups(normalizeDuplicateRows(rows), keys)
	removed := 0
	for _, cluster := range clusters {
		removed += len(cluster) - 1
	}

	syncedAt := ""
	if _, lastSynced, _, syncErr := db.GetSyncState("items"); syncErr == nil && !lastSynced.IsZero() {
		syncedAt = lastSynced.UTC().Format(time.RFC3339)
	}
	afterDeduplication := identified.Total - removed
	return libraryPrismaReport{
		SchemaVersion:             1,
		Scope:                     scope.Expr,
		By:                        by,
		SyncedAt:                  syncedAt,
		Identified:                identified,
		DuplicateClusters:         len(clusters),
		DuplicateRecordsRemoved:   removed,
		RecordsAfterDeduplication: afterDeduplication,
		Prisma: prismaFlowData{
			RecordsIdentified:       identified.Total,
			DuplicateRecordsRemoved: removed,
			RecordsScreenedInput:    afterDeduplication,
		},
	}, nil
}

func queryPrismaCorpus(db localQueryStore, scope scopeResult) ([]prismaCorpusItem, error) {
	clause, args, err := prismaScopeClause(scope)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryRaw(fmt.Sprintf(`
SELECT
	id AS key,
	COALESCE(NULLIF(TRIM(json_extract(data, '$.data.libraryCatalog')), ''), 'unspecified') AS source
FROM resources
WHERE resource_type='items'
	AND COALESCE(json_extract(data, '$.data.itemType'), '') NOT IN ('attachment', 'annotation', 'note')%s
ORDER BY id`, clause), args...)
	if err != nil {
		return nil, err
	}
	items := make([]prismaCorpusItem, 0, len(rows))
	for _, row := range rows {
		key := sqlStringValue(row["key"])
		if key == "" {
			continue
		}
		source := sqlStringValue(row["source"])
		if source == "" {
			source = "unspecified"
		}
		items = append(items, prismaCorpusItem{Key: key, Source: source})
	}
	return items, nil
}

func prismaScopeClause(scope scopeResult) (string, []any, error) {
	if scope.All {
		return "", nil, nil
	}
	return duplicateKeyScopeClause(scope.Keys)
}

func queryPrismaDuplicateRows(db localQueryStore, by string, keys map[string]struct{}) ([]map[string]any, error) {
	switch by {
	case "doi":
		return queryDuplicateDOIs(db, keys)
	case "title":
		return queryDuplicateTitles(db, keys)
	case "all":
		doiRows, err := queryDuplicateDOIs(db, keys)
		if err != nil {
			return nil, err
		}
		titleRows, err := queryDuplicateTitles(db, keys)
		if err != nil {
			return nil, err
		}
		return append(doiRows, titleRows...), nil
	default:
		return nil, fmt.Errorf("invalid duplicate detector %q", by)
	}
}

// mergePrismaDuplicateGroups collapses overlapping detector results so a DOI and
// title match for the same records removes those records only once.
func mergePrismaDuplicateGroups(groups []map[string]any, allowed map[string]struct{}) [][]string {
	parent := make(map[string]string)

	var root func(string) string
	root = func(key string) string {
		if parent[key] != key {
			parent[key] = root(parent[key])
		}
		return parent[key]
	}
	union := func(left, right string) {
		left, right = root(left), root(right)
		if left != right {
			parent[right] = left
		}
	}

	for _, group := range groups {
		rawKeys, ok := group["keys"].([]string)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(rawKeys))
		seen := make(map[string]struct{}, len(rawKeys))
		for _, key := range rawKeys {
			if key == "" {
				continue
			}
			if allowed != nil {
				if _, ok := allowed[key]; !ok {
					continue
				}
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		if len(keys) < 2 {
			continue
		}
		// Initialize only unseen keys: re-rooting a key already unioned by an
		// earlier detector group would sever that cluster and split it.
		for _, key := range keys {
			if _, ok := parent[key]; !ok {
				parent[key] = key
			}
		}
		for _, key := range keys[1:] {
			union(keys[0], key)
		}
	}

	byRoot := make(map[string][]string)
	for key := range parent {
		byRoot[root(key)] = append(byRoot[root(key)], key)
	}
	clusters := make([][]string, 0, len(byRoot))
	for _, keys := range byRoot {
		if len(keys) < 2 {
			continue
		}
		sort.Strings(keys)
		clusters = append(clusters, keys)
	}
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i][0] < clusters[j][0]
	})
	return clusters
}

func printLibraryPrisma(cmd *cobra.Command, report libraryPrismaReport) error {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Records identified: %d\n", report.Identified.Total)
	type sourceCount struct {
		source string
		count  int
	}
	sources := make([]sourceCount, 0, len(report.Identified.BySource))
	for source, count := range report.Identified.BySource {
		sources = append(sources, sourceCount{source: source, count: count})
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].count != sources[j].count {
			return sources[i].count > sources[j].count
		}
		return sources[i].source < sources[j].source
	})
	for _, source := range sources {
		fmt.Fprintf(w, "  %s: %d\n", source.source, source.count)
	}
	detector := report.By
	if detector == "all" {
		detector = "doi+title"
	}
	fmt.Fprintf(w, "Duplicate records removed: %d (%d clusters, by %s)\n", report.DuplicateRecordsRemoved, report.DuplicateClusters, detector)
	fmt.Fprintf(w, "Records after deduplication (input to screening): %d\n", report.RecordsAfterDeduplication)
	return nil
}
