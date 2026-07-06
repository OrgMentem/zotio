// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written local tag inventory report missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type tagInventoryItem struct {
	Tag             string `json:"tag"`
	CollectionCount int    `json:"collection_count"`
	LibraryCount    int    `json:"library_count"`
	CollectionOnly  bool   `json:"collection_only"`
}

func newTagsInventoryCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string

	cmd := &cobra.Command{
		Use:         "inventory",
		Short:       "Show tag usage within collections and across the library",
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

			scopedRows, err := db.QueryRaw(`
SELECT
	json_each_tags.value AS tag_json,
	json_each_colls.value AS coll_key_json
FROM resources i,
	json_each(json_extract(i.data,'$.data.tags')) AS json_each_tags,
	json_each(json_extract(i.data,'$.data.collections')) AS json_each_colls
WHERE i.resource_type='items'
	AND json_extract(i.data,'$.data.itemType') NOT IN ('attachment','note','annotation')`)
			if err != nil {
				return fmt.Errorf("querying collection tag usage: %w", err)
			}
			libraryRows, err := db.QueryRaw(`
SELECT json_extract(t.value,'$.tag') AS tag_name, COUNT(*) AS total
FROM resources, json_each(json_extract(data,'$.data.tags')) AS t
WHERE resource_type='items'
GROUP BY tag_name`)
			if err != nil {
				return fmt.Errorf("querying library tag usage: %w", err)
			}

			inventory := buildTagInventory(scopedRows, libraryRows, flagCollection)
			data, err := json.Marshal(inventory)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Show only tags within this collection key")

	return cmd
}

func buildTagInventory(scopedRows, libraryRows []map[string]any, collection string) []tagInventoryItem {
	libraryCounts := make(map[string]int, len(libraryRows))
	for _, row := range libraryRows {
		tag := strings.TrimSpace(sqlStringValue(row["tag_name"]))
		if tag == "" {
			continue
		}
		libraryCounts[tag] = sqlIntValue(row["total"])
	}

	collectionCounts := map[string]int{}
	for _, row := range scopedRows {
		tag := tagNameFromInventoryJSON(sqlStringValue(row["tag_json"]))
		collKey := collectionKeyFromInventoryJSON(sqlStringValue(row["coll_key_json"]))
		if tag == "" {
			continue
		}
		if collection != "" && collKey != collection {
			continue
		}
		collectionCounts[tag]++
	}

	out := make([]tagInventoryItem, 0, len(collectionCounts))
	for tag, count := range collectionCounts {
		libraryCount := libraryCounts[tag]
		out = append(out, tagInventoryItem{
			Tag:             tag,
			CollectionCount: count,
			LibraryCount:    libraryCount,
			CollectionOnly:  count == libraryCount,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CollectionCount != out[j].CollectionCount {
			return out[i].CollectionCount > out[j].CollectionCount
		}
		return strings.ToLower(out[i].Tag) < strings.ToLower(out[j].Tag)
	})
	return out
}

func tagNameFromInventoryJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) == nil {
		return strings.TrimSpace(fmt.Sprintf("%v", obj["tag"]))
	}
	var tag string
	if json.Unmarshal([]byte(raw), &tag) == nil {
		return strings.TrimSpace(tag)
	}
	return strings.Trim(raw, `"`)
}

func collectionKeyFromInventoryJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var key string
	if json.Unmarshal([]byte(raw), &key) == nil {
		return strings.TrimSpace(key)
	}
	return strings.Trim(raw, `"`)
}
