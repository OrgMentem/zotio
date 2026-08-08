// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

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
		Short:       "Analyze a synced Zotero library",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Long: `Analyze locally synced Zotero data.

--type accepts a mirrored resource kind (such as items or collections) for
resource counts, or a Zotero item type (such as journalArticle or book) to
count only matching rows in the items mirror. When --group-by is supplied,
the item rows are grouped by year, itemType, collection, creator, or tag.`,
		Example: `  # Count all items and only journal articles
  zotio analytics --type items
  zotio analytics --type journalArticle --json

  # Group items by publication year or collection
  zotio analytics --type items --group-by year
  zotio analytics --type items --group-by collection --limit 10 --json`,
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

			if cmd.Flags().Changed("limit") && limit <= 0 {
				return fmt.Errorf("--limit must be greater than zero")
			}
			if groupBy != "" {
				if resourceType == "" {
					return fmt.Errorf("--group-by requires --type items or a Zotero item type")
				}
				if err := validateAnalyticsGroupBy(groupBy); err != nil {
					return err
				}
				return runGroupBy(cmd.OutOrStdout(), db, resourceType, groupBy, limit, flags)
			}
			if cmd.Flags().Changed("limit") {
				return fmt.Errorf("--limit requires --group-by")
			}

			if resourceType == "" {
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

			scope, err := analyticsScopeForType(db, resourceType)
			if err != nil {
				return err
			}
			count := len(scope.rows)
			if flags.asJSON {
				result := map[string]any{"resource_type": scope.resourceType, "count": count}
				if scope.itemType != "" {
					result["item_type"] = scope.itemType
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d records\n", resourceType, count)
			return nil
		},
	}

	cmd.Flags().StringVar(&resourceType, "type", "", "Resource kind or Zotero item type to analyze")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "Group items by year, itemType, collection, creator, or tag")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path")
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum groups to show with --group-by")

	return cmd
}

type analyticsScope struct {
	resourceType string
	itemType     string
	rows         []json.RawMessage
}

var analyticsResourceKinds = map[string]bool{
	"collections": true, "items": true, "items-trash": true,
	"schema": true, "schema-creator-fields": true, "schema-item-fields": true,
	"searches": true, "tags": true,
}

// These are Zotero's built-in item types. The mirror may contain a newer type,
// which is also accepted when it is present in the synced items rows.
var analyticsItemTypes = map[string]bool{
	"artwork": true, "attachment": true, "audioRecording": true, "bill": true,
	"blogPost": true, "book": true, "bookSection": true, "case": true,
	"computerProgram": true, "conferencePaper": true, "dictionaryEntry": true,
	"document": true, "email": true, "encyclopediaArticle": true, "film": true,
	"forumPost": true, "hearing": true, "instantMessage": true, "interview": true,
	"journalArticle": true, "letter": true, "magazineArticle": true, "manuscript": true,
	"map": true, "newspaperArticle": true, "patent": true, "podcast": true,
	"presentation": true, "radioBroadcast": true, "report": true, "statute": true,
	"thesis": true, "tvBroadcast": true, "videoRecording": true, "webpage": true,
	"note": true, "annotation": true, "preprint": true, "dataset": true,
}

func analyticsScopeForType(db *store.Store, requested string) (analyticsScope, error) {
	status, err := db.Status()
	if err != nil {
		return analyticsScope{}, fmt.Errorf("getting resource status: %w", err)
	}
	if analyticsResourceKinds[requested] || status[requested] > 0 {
		rows, err := db.List(requested, 0)
		if err != nil {
			return analyticsScope{}, fmt.Errorf("listing %s: %w", requested, err)
		}
		return analyticsScope{resourceType: requested, rows: rows}, nil
	}

	// Item types are a view over the items mirror, not resource_type values.
	rows, err := db.List("items", 0)
	if err != nil {
		return analyticsScope{}, fmt.Errorf("listing items: %w", err)
	}
	matched := make([]json.RawMessage, 0)
	observed := false
	for _, raw := range rows {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		itemType := groupByFieldValue(obj, "itemType")
		if itemType != groupByUnsetSentinel {
			if itemType == requested {
				observed = true
				matched = append(matched, raw)
			}
		}
	}
	if !analyticsItemTypes[requested] && !observed {
		return analyticsScope{}, fmt.Errorf("unknown analytics type %q: expected a mirrored resource kind or Zotero item type", requested)
	}
	return analyticsScope{resourceType: "items", itemType: requested, rows: matched}, nil
}

func validateAnalyticsGroupBy(field string) error {
	switch field {
	case "year", "itemType", "collection", "creator", "tag":
		return nil
	default:
		return fmt.Errorf("--group-by %q is not supported; use year, itemType, collection, creator, or tag", field)
	}
}

func runGroupBy(w io.Writer, db *store.Store, resourceType, field string, limit int, flags *rootFlags) error {
	scope, err := analyticsScopeForType(db, resourceType)
	if err != nil {
		return err
	}
	if scope.resourceType != "items" {
		return fmt.Errorf("--group-by applies to Zotero items, not resource kind %q", scope.resourceType)
	}

	counts := make(map[string]int)
	for _, item := range scope.rows {
		var obj map[string]any
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		values := analyticsGroupValues(obj, field)
		for _, value := range values {
			counts[value]++
		}
	}

	type kv struct {
		Key   string `json:"value"`
		Count int    `json:"count"`
	}
	sorted := make([]kv, 0, len(counts))
	for k, v := range counts {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		return sorted[i].Key < sorted[j].Key
	})
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

func analyticsGroupValues(obj map[string]any, field string) []string {
	switch field {
	case "year":
		year := zoteroItemYear(obj)
		if year == "" {
			return []string{groupByUnsetSentinel}
		}
		return []string{year}
	case "collection":
		return analyticsArrayValues(obj, "collections", func(v any) string {
			return strings.TrimSpace(fmt.Sprintf("%v", v))
		})
	case "tag":
		return analyticsArrayValues(obj, "tags", func(v any) string {
			if tag, ok := v.(map[string]any); ok {
				return strings.TrimSpace(fmt.Sprintf("%v", tag["tag"]))
			}
			return strings.TrimSpace(fmt.Sprintf("%v", v))
		})
	case "creator":
		values := make([]string, 0)
		if data := zoteroData(obj); data != nil {
			if creators, ok := data["creators"].([]any); ok {
				for _, raw := range creators {
					if creator, ok := raw.(map[string]any); ok {
						if name := strings.TrimSpace(zoteroCreatorName(creator)); name != "" {
							values = append(values, name)
						}
					}
				}
			}
		}
		if len(values) == 0 {
			return []string{groupByUnsetSentinel}
		}
		return values
	default:
		return []string{groupByFieldValue(obj, field)}
	}
}

func analyticsArrayValues(obj map[string]any, field string, format func(any) string) []string {
	var raw any
	if data := zoteroData(obj); data != nil {
		raw = data[field]
	}
	if raw == nil {
		raw = obj[field]
	}
	values := make([]string, 0)
	if items, ok := raw.([]any); ok {
		for _, item := range items {
			if value := format(item); value != "" && value != "<nil>" {
				values = append(values, value)
			}
		}
	}
	if len(values) == 0 {
		return []string{groupByUnsetSentinel}
	}
	return values
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
