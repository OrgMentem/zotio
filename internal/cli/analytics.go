// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newAnalyticsCmd(flags *rootFlags) *cobra.Command {
	var resourceType string
	var groupBy string
	var dbFlag string
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
			// The mirror path is a local, resolved on every invocation and
			// never written back into dbFlag, so a `--group all` re-entry
			// counts its own library: see resolveDBPath.
			dbPath, err := resolveDBPath(dbFlag, "zotio")
			if err != nil {
				return err
			}

			var rows []map[string]any
			var prov DataProvenance
			humanMode := ""

			if _, err := os.Stat(dbPath); err != nil && os.IsNotExist(err) {
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
					isKnownKind := analyticsResourceKinds[resourceType]
					isKnownItemType := analyticsItemTypes[resourceType]
					if !isKnownKind && !isKnownItemType {
						return fmt.Errorf("unknown analytics type %q: expected a mirrored resource kind or Zotero item type", resourceType)
					}
					if isKnownKind && resourceType != "items" {
						return fmt.Errorf("--group-by applies to Zotero items, not resource kind %q", resourceType)
					}
					reportedType := resourceType
					if !isKnownKind {
						reportedType = "items"
					}
					rows = make([]map[string]any, 0)
					prov = DataProvenance{Source: "local", Reason: "local_only", ResourceType: reportedType, GroupBy: groupBy}
					humanMode = "group"
				} else {
					if cmd.Flags().Changed("limit") {
						return fmt.Errorf("--limit requires --group-by")
					}
					if resourceType == "" {
						rows = make([]map[string]any, 0)
						prov = DataProvenance{Source: "local", Reason: "local_only", ResourceType: "analytics"}
						humanMode = "status"
					} else {
						isKnownKind := analyticsResourceKinds[resourceType]
						isKnownItemType := analyticsItemTypes[resourceType]
						if !isKnownKind && !isKnownItemType {
							return fmt.Errorf("unknown analytics type %q: expected a mirrored resource kind or Zotero item type", resourceType)
						}
						reportedType := resourceType
						itemType := ""
						if !isKnownKind {
							reportedType = "items"
							itemType = resourceType
						}
						row := map[string]any{"resource_type": reportedType, "count": 0}
						if itemType != "" {
							row["item_type"] = itemType
						}
						rows = make([]map[string]any, 0, 1)
						rows = append(rows, row)
						prov = DataProvenance{Source: "local", Reason: "local_only", ResourceType: reportedType}
						humanMode = "type"
					}
				}
			} else {
				db, err := store.OpenReadOnlyContext(cmd.Context(), dbPath)
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
					rows, err = analyticsGroupRows(db, resourceType, groupBy, limit)
					if err != nil {
						return err
					}
					// analyticsGroupRows refuses a scope that does not resolve to
					// items, so a successful call fixes the resource type. Resolving
					// the scope again here would list and JSON-decode the whole
					// items mirror a second time for one string.
					prov = localProvenance(db, "items", "local_only")
					prov.GroupBy = groupBy
					humanMode = "group"
				} else {
					if cmd.Flags().Changed("limit") {
						return fmt.Errorf("--limit requires --group-by")
					}
					if resourceType == "" {
						rows, err = analyticsStatusRows(db)
						if err != nil {
							return fmt.Errorf("getting status: %w", err)
						}
						prov = localProvenance(db, "analytics", "local_only")
						humanMode = "status"
					} else {
						rows, err = analyticsTypeRows(db, resourceType)
						if err != nil {
							return err
						}
						// The row already carries the resolved resource type, so
						// read it back instead of resolving the scope a second
						// time over the whole items mirror.
						reported := resourceType
						if len(rows) == 1 {
							if rt, ok := rows[0]["resource_type"].(string); ok && rt != "" {
								reported = rt
							}
						}
						prov = localProvenance(db, reported, "local_only")
						humanMode = "type"
					}
				}
			}

			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			printProvenance(cmd, len(rows), prov)
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := json.RawMessage(data)
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
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				w := cmd.OutOrStdout()
				switch humanMode {
				case "group":
					fmt.Fprintf(w, "%s\tCount\n", groupBy)
					fmt.Fprintln(w, "---\t-----")
					for _, row := range rows {
						fmt.Fprintf(w, "%s\t%d\n", row["value"], row["count"])
					}
				case "status":
					fmt.Fprintln(w, "Resource Type\tCount")
					fmt.Fprintln(w, "-------------\t-----")
					for _, row := range rows {
						fmt.Fprintf(w, "%s\t%d\n", row["resource_type"], row["count"])
					}
				case "type":
					count := 0
					if len(rows) == 1 {
						count, _ = rows[0]["count"].(int)
					}
					fmt.Fprintf(w, "%s: %d records\n", resourceType, count)
				}
				return nil
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}

	cmd.Flags().StringVar(&resourceType, "type", "", "Resource kind or Zotero item type to analyze")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "Group items by year, itemType, collection, creator, or tag")
	cmd.Flags().StringVar(&dbFlag, "db", "", "Database path")
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

func analyticsGroupRows(db *store.Store, resourceType, field string, limit int) ([]map[string]any, error) {
	scope, err := analyticsScopeForType(db, resourceType)
	if err != nil {
		return nil, err
	}
	if scope.resourceType != "items" {
		return nil, fmt.Errorf("--group-by applies to Zotero items, not resource kind %q", scope.resourceType)
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
		Key   string
		Count int
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

	rows := make([]map[string]any, 0, len(sorted))
	for _, item := range sorted {
		rows = append(rows, map[string]any{"value": item.Key, "count": item.Count})
	}
	return rows, nil
}

func analyticsStatusRows(db *store.Store) ([]map[string]any, error) {
	status, err := db.Status()
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(status))
	for resourceType, count := range status {
		rows = append(rows, map[string]any{"resource_type": resourceType, "count": count})
	}
	sort.Slice(rows, func(i, j int) bool {
		leftCount := rows[i]["count"].(int)
		rightCount := rows[j]["count"].(int)
		if leftCount != rightCount {
			return leftCount > rightCount
		}
		return rows[i]["resource_type"].(string) < rows[j]["resource_type"].(string)
	})
	return rows, nil
}

func analyticsTypeRows(db *store.Store, requested string) ([]map[string]any, error) {
	scope, err := analyticsScopeForType(db, requested)
	if err != nil {
		return nil, err
	}
	row := map[string]any{"resource_type": scope.resourceType, "count": len(scope.rows)}
	if scope.itemType != "" {
		row["item_type"] = scope.itemType
	}
	rows := make([]map[string]any, 0, 1)
	rows = append(rows, row)
	return rows, nil
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
