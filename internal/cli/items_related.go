// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type itemRelatedReport struct {
	Key       string             `json:"key"`
	Related   []itemRelationEdge `json:"related"`
	Truncated bool               `json:"truncated"`
}

type itemRelationEdge struct {
	Direction     string `json:"direction"`
	Predicate     string `json:"predicate"`
	SourceKey     string `json:"source_key"`
	TargetKey     string `json:"target_key"`
	TargetURI     string `json:"target_uri"`
	TargetPresent bool   `json:"target_present"`
	Title         string `json:"title"`
	ItemType      string `json:"item_type"`

	parsedTargetKey string
	relatedKey      string
}

type itemRelationSummary struct {
	Title    string
	ItemType string
}

func newItemsRelatedCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "related <itemKey>",
		Short:       "Show outgoing and incoming Zotero related-item edges",
		Args:        cobra.ExactArgs(1),
		Example:     "  zotio items related ABC12345\n  zotio items related ABC12345 --json",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			report, found, synced, err := itemRelatedReportFromLocalStore(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !synced {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			if !found {
				if flags.asJSON {
					data, err := graphNotFoundJSON(args[0])
					if err != nil {
						return err
					}
					return printOutput(cmd.OutOrStdout(), data, true)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Item not found: %s\n", args[0])
				return nil
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			return printItemRelatedReport(cmd, report)
		},
	}
	return cmd
}

func itemRelatedReportFromLocalStore(ctx context.Context, key string) (itemRelatedReport, bool, bool, error) {
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return itemRelatedReport{}, false, false, fmt.Errorf("opening local database: %w", err)
	}
	if db == nil {
		return itemRelatedReport{}, false, false, nil
	}
	defer db.Close()

	report, found, err := buildItemRelatedReport(localQueryStore{db}, key, graphNodeCap)
	return report, found, true, err
}

func buildItemRelatedReport(db localQueryStore, key string, cap int) (itemRelatedReport, bool, error) {
	report := itemRelatedReport{Key: key, Related: []itemRelationEdge{}}
	raw, err := db.Get("items", key)
	if err != nil {
		return report, false, fmt.Errorf("loading source item: %w", err)
	}
	if raw == nil {
		return report, false, nil
	}

	outgoing, err := outgoingRelationEdges(raw, key)
	if err != nil {
		return report, false, err
	}
	for _, edge := range outgoing {
		appendCappedRelation(&report, edge, cap)
	}

	if !report.Truncated {
		if err := appendIncomingRelationEdges(db, key, cap, &report); err != nil {
			return report, false, err
		}
	}
	if len(report.Related) > cap {
		report.Related = report.Related[:cap]
		report.Truncated = true
	}

	if err := hydrateRelationEdges(db, &report); err != nil {
		return report, false, err
	}
	return report, true, nil
}

func outgoingRelationEdges(raw json.RawMessage, sourceKey string) ([]itemRelationEdge, error) {
	relations, err := itemRelationsFromRaw(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing source item relations: %w", err)
	}
	predicates := make([]string, 0, len(relations))
	for predicate := range relations {
		predicates = append(predicates, predicate)
	}
	sort.Strings(predicates)

	edges := make([]itemRelationEdge, 0, len(relations))
	for _, predicate := range predicates {
		uris := relationURIsFromJSON(relations[predicate])
		for _, uri := range uris {
			parsedKey := parseZoteroItemURIKey(uri)
			edges = append(edges, itemRelationEdge{
				Direction:       "outgoing",
				Predicate:       predicate,
				SourceKey:       sourceKey,
				TargetURI:       uri,
				parsedTargetKey: parsedKey,
				relatedKey:      parsedKey,
			})
		}
	}
	return edges, nil
}

func itemRelationsFromRaw(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var item struct {
		Data struct {
			Relations map[string]json.RawMessage `json:"relations"`
		} `json:"data"`
		Relations map[string]json.RawMessage `json:"relations"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	if item.Data.Relations != nil {
		return item.Data.Relations, nil
	}
	if item.Relations != nil {
		return item.Relations, nil
	}
	return map[string]json.RawMessage{}, nil
}

func appendIncomingRelationEdges(db localQueryStore, key string, cap int, report *itemRelatedReport) error {
	rows, err := db.Query(`
SELECT
	r.id AS source_key,
	rel.key AS predicate,
	CAST(rel.value AS TEXT) AS relation_value,
	rel.type AS relation_type,
	COALESCE(json_extract(r.data,'$.data.title'), '') AS source_title,
	COALESCE(r.item_type, json_extract(r.data,'$.data.itemType'), '') AS source_item_type
FROM resources r, json_each(r.data, '$.data.relations') AS rel
WHERE r.resource_type='items'
ORDER BY r.id, rel.key`)
	if err != nil {
		return fmt.Errorf("querying incoming relations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sourceKey, predicate string
		var relationValue, relationType, sourceTitle, sourceItemType sql.NullString
		if err := rows.Scan(&sourceKey, &predicate, &relationValue, &relationType, &sourceTitle, &sourceItemType); err != nil {
			return err
		}
		for _, uri := range relationURIsFromSQL(relationValue.String, relationType.String) {
			parsedKey := parseZoteroItemURIKey(uri)
			if parsedKey != key {
				continue
			}
			edge := itemRelationEdge{
				Direction:       "incoming",
				Predicate:       predicate,
				SourceKey:       sourceKey,
				TargetURI:       uri,
				parsedTargetKey: parsedKey,
				relatedKey:      sourceKey,
				Title:           sourceTitle.String,
				ItemType:        sourceItemType.String,
			}
			appendCappedRelation(report, edge, cap)
			if report.Truncated {
				return rows.Err()
			}
		}
	}
	return rows.Err()
}

func appendCappedRelation(report *itemRelatedReport, edge itemRelationEdge, cap int) {
	if cap <= 0 {
		report.Truncated = true
		return
	}
	if len(report.Related) >= cap {
		report.Truncated = true
		return
	}
	report.Related = append(report.Related, edge)
}

func hydrateRelationEdges(db localQueryStore, report *itemRelatedReport) error {
	keys := make(map[string]bool)
	for _, edge := range report.Related {
		if edge.parsedTargetKey != "" {
			keys[edge.parsedTargetKey] = true
		}
		if edge.relatedKey != "" {
			keys[edge.relatedKey] = true
		}
	}
	summaries, err := itemSummariesForKeys(db, keys)
	if err != nil {
		return err
	}
	for i := range report.Related {
		edge := &report.Related[i]
		if edge.parsedTargetKey != "" {
			if _, ok := summaries[edge.parsedTargetKey]; ok {
				edge.TargetPresent = true
				edge.TargetKey = edge.parsedTargetKey
			}
		}
		if edge.relatedKey == "" {
			continue
		}
		if summary, ok := summaries[edge.relatedKey]; ok {
			edge.Title = summary.Title
			edge.ItemType = summary.ItemType
		}
	}
	return nil
}

func itemSummariesForKeys(db localQueryStore, keys map[string]bool) (map[string]itemRelationSummary, error) {
	out := make(map[string]itemRelationSummary)
	if len(keys) == 0 {
		return out, nil
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		if key != "" {
			ordered = append(ordered, key)
		}
	}
	sort.Strings(ordered)
	if len(ordered) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(ordered))
	args := make([]any, len(ordered))
	for i, key := range ordered {
		placeholders[i] = "?"
		args[i] = key
	}
	rows, err := db.QueryRaw(`
SELECT id AS key, COALESCE(json_extract(data,'$.data.title'), '') AS title, COALESCE(item_type, json_extract(data,'$.data.itemType'), '') AS item_type
FROM resources
WHERE resource_type='items' AND id IN (`+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, fmt.Errorf("looking up relation targets: %w", err)
	}
	for _, row := range rows {
		out[sqlStringValue(row["key"])] = itemRelationSummary{
			Title:    sqlStringValue(row["title"]),
			ItemType: sqlStringValue(row["item_type"]),
		}
	}
	return out, nil
}

func relationURIsFromJSON(raw json.RawMessage) []string {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return nonEmptyRelationURIs([]string{single})
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return nonEmptyRelationURIs(many)
	}
	var mixed []json.RawMessage
	if err := json.Unmarshal(raw, &mixed); err == nil {
		out := make([]string, 0, len(mixed))
		for _, value := range mixed {
			var uri string
			if err := json.Unmarshal(value, &uri); err == nil {
				out = append(out, uri)
			}
		}
		return nonEmptyRelationURIs(out)
	}
	return nil
}

func relationURIsFromSQL(value string, relationType string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if relationType == "array" || strings.HasPrefix(value, "[") {
		return relationURIsFromJSON(json.RawMessage(value))
	}
	return nonEmptyRelationURIs([]string{value})
}

func nonEmptyRelationURIs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func parseZoteroItemURIKey(rawURI string) string {
	u, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil || u == nil || !strings.EqualFold(u.Hostname(), "zotero.org") {
		return ""
	}
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segments) < 4 || (segments[0] != "users" && segments[0] != "groups") || segments[2] != "items" {
		return ""
	}
	key, err := url.PathUnescape(segments[3])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(key)
}

func printItemRelatedReport(cmd *cobra.Command, report itemRelatedReport) error {
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "DIRECTION\tPREDICATE\tKEY\tPRESENT\tTITLE")
	for _, edge := range report.Related {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", edge.Direction, edge.Predicate, itemRelationDisplayKey(edge), edge.TargetPresent, edge.Title)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if report.Truncated {
		fmt.Fprintf(cmd.OutOrStdout(), "\nTruncated at %d edge(s).\n", graphNodeCap)
	}
	return nil
}

func itemRelationDisplayKey(edge itemRelationEdge) string {
	if edge.Direction == "incoming" {
		return edge.SourceKey
	}
	if edge.TargetKey != "" {
		return edge.TargetKey
	}
	return edge.parsedTargetKey
}
