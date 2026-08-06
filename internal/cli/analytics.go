// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newAnalyticsCmd(flags *rootFlags) *cobra.Command {
	var resourceType string
	var groupBy string
	var dbPath string
	var limit int

	cmd := &cobra.Command{
		Use:         "analytics",
		Short:       "Run analytics queries on locally synced data",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Long: `Analyze locally synced data with count, group-by, and summary operations.
Data must be synced first with the sync command.`,
		Example: `  # Count records by type
  zotio analytics --type messages

  # Group by a field
  zotio analytics --type messages --group-by author_id

  # Top 10 most frequent values
  zotio analytics --type messages --group-by channel_id --limit 10 --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if dbPath == "" {
				dbPath, err = defaultDBPath("zotio")
				if err != nil {
					return err
				}
			}

			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening local database: %w\nRun 'zotio sync' first.", err)
			}
			defer db.Close()

			if resourceType == "" {
				// Show summary of all resource types
				status, err := db.Status()
				if err != nil {
					return fmt.Errorf("getting status: %w", err)
				}
				if flags.asJSON {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(status)
				}
				w := cmd.OutOrStdout()
				fmt.Fprintln(w, "Resource Type\tCount")
				fmt.Fprintln(w, "-------------\t-----")
				for rt, count := range status {
					fmt.Fprintf(w, "%s\t%d\n", rt, count)
				}
				return nil
			}

			if groupBy != "" {
				return runGroupBy(cmd.OutOrStdout(), db, resourceType, groupBy, limit, flags)
			}

			count, err := db.Count(resourceType)
			if err != nil {
				return fmt.Errorf("counting: %w", err)
			}

			if flags.asJSON {
				result := map[string]any{"resource_type": resourceType, "count": count}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d records\n", resourceType, count)
			return nil
		},
	}

	cmd.Flags().StringVar(&resourceType, "type", "", "Resource type to analyze")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "Field to group by")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path")
	cmd.Flags().IntVar(&limit, "limit", 25, "Max groups to show")

	return cmd
}

func runGroupBy(w io.Writer, db *store.Store, resourceType, field string, limit int, flags *rootFlags) error {
	items, err := db.List(resourceType, 0)
	if err != nil {
		return err
	}

	counts := make(map[string]int)
	for _, item := range items {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		val := groupByFieldValue(obj, field)
		counts[val]++
	}

	type kv struct {
		Key   string `json:"value"`
		Count int    `json:"count"`
	}
	var sorted []kv
	for k, v := range counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Count > sorted[j].Count })
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}

	if flags.asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(sorted)
	}

	fmt.Fprintf(w, "%s\tCount\n", field)
	fmt.Fprintln(w, "---\t-----")
	for _, kv := range sorted {
		fmt.Fprintf(w, "%s\t%d\n", kv.Key, kv.Count)
	}
	return nil
}

// groupByUnsetSentinel marks resources where the requested group-by field is
// absent at every level. fmt.Sprintf("%v", nil) renders as the Go-internal
// string "<nil>", which would silently mix real data with a formatting
// artifact; a distinct, obviously-non-data sentinel keeps missing values
// visible and unambiguous in reports.
const groupByUnsetSentinel = "(unset)"

// groupByFieldValue resolves field for grouping using the same precedence
// synced Zotero payloads use elsewhere in the codebase: nested obj["data"][field]
// first, then the top-level obj[field] as a fallback for flat/local payloads.
// This mirrors the COALESCE(data.dateModified, dateModified) ordering used by
// QueryTrash's ORDER BY (internal/store/query.go:133-136).
func groupByFieldValue(obj map[string]any, field string) string {
	if data, ok := obj["data"].(map[string]any); ok {
		if v, present := data[field]; present && v != nil {
			return fmt.Sprintf("%v", v)
		}
	}
	if v, present := obj[field]; present && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return groupByUnsetSentinel
}
