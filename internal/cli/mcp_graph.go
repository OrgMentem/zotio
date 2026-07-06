// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase5 bounded-graph): expose bounded local graph reads
// for MCP resources without letting recursive Zotero relationships fan out
// unboundedly.

package cli

import (
	"context"
	"encoding/json"
)

const graphNodeCap = 500
const graphMaxDepth = 6

type graphCollectionNode struct {
	Key            string                `json:"key"`
	Name           string                `json:"name"`
	Subcollections []graphCollectionNode `json:"subcollections"`
}

type graphCollectionTree struct {
	Key            string                `json:"key"`
	Name           string                `json:"name"`
	Subcollections []graphCollectionNode `json:"subcollections"`
	Truncated      bool                  `json:"truncated"`
}

type graphItemChild struct {
	Key      string `json:"key"`
	ItemType string `json:"item_type"`
	Title    string `json:"title"`
}

type graphItemChildren struct {
	Key       string           `json:"key"`
	Children  []graphItemChild `json:"children"`
	Truncated bool             `json:"truncated"`
}

type graphItemAttachment struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	ContentType string `json:"content_type"`
	LinkMode    string `json:"link_mode"`
}

type graphItemAttachments struct {
	Key         string                `json:"key"`
	Attachments []graphItemAttachment `json:"attachments"`
	Truncated   bool                  `json:"truncated"`
}

type graphItemContext struct {
	Key             string   `json:"key"`
	ItemType        string   `json:"item_type"`
	Title           string   `json:"title"`
	Parent          string   `json:"parent"`
	Collections     []string `json:"collections"`
	Tags            []string `json:"tags"`
	ChildCount      int      `json:"child_count"`
	AttachmentCount int      `json:"attachment_count"`
	Truncated       bool     `json:"truncated"`
}

func graphUnsyncedJSON() ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"synced": false,
		"note":   "local store not synced; run sync",
	}, "", "  ")
}

func graphNotFoundJSON(key string) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"error": "not found",
		"key":   key,
	}, "", "  ")
}

// CollectionTreeJSON returns the named collection and bounded nested
// subcollections for MCP graph resources.
// PATCH(glean roadmap-phase5 bounded-graph): cap recursion by both depth and
// node count so malformed collection graphs cannot exhaust MCP hosts.
func CollectionTreeJSON(key string) ([]byte, error) {
	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return graphUnsyncedJSON()
	}
	defer db.Close()

	qs := localQueryStore{db}
	rows, err := qs.QueryRaw(`
SELECT json_extract(data,'$.data.name') AS name
FROM resources
WHERE resource_type='collections' AND id=?`, key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return graphNotFoundJSON(key)
	}

	truncated := false
	total := 1
	root := graphCollectionTree{
		Key:            key,
		Name:           sqlStringValue(rows[0]["name"]),
		Subcollections: []graphCollectionNode{},
	}
	children, err := graphCollectionChildren(qs, key, 1, &total, &truncated)
	if err != nil {
		return nil, err
	}
	root.Subcollections = children
	root.Truncated = truncated
	return json.MarshalIndent(root, "", "  ")
}

func graphCollectionChildren(qs localQueryStore, parentKey string, depth int, total *int, truncated *bool) ([]graphCollectionNode, error) {
	if *total >= graphNodeCap {
		*truncated = true
		return []graphCollectionNode{}, nil
	}
	if depth > graphMaxDepth {
		rows, err := qs.QueryRaw(`
SELECT id AS key
FROM resources
WHERE resource_type='collections' AND json_extract(data,'$.data.parentCollection')=?
LIMIT 1`, parentKey)
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			*truncated = true
		}
		return []graphCollectionNode{}, nil
	}

	remaining := graphNodeCap - *total
	rows, err := qs.QueryRaw(`
SELECT id AS key, json_extract(data,'$.data.name') AS name
FROM resources
WHERE resource_type='collections' AND json_extract(data,'$.data.parentCollection')=?
ORDER BY name, id
LIMIT ?`, parentKey, remaining+1)
	if err != nil {
		return nil, err
	}
	if len(rows) > remaining {
		*truncated = true
		rows = rows[:remaining]
	}

	children := make([]graphCollectionNode, 0, len(rows))
	for _, row := range rows {
		(*total)++
		child := graphCollectionNode{
			Key:            sqlStringValue(row["key"]),
			Name:           sqlStringValue(row["name"]),
			Subcollections: []graphCollectionNode{},
		}
		grandchildren, err := graphCollectionChildren(qs, child.Key, depth+1, total, truncated)
		if err != nil {
			return nil, err
		}
		child.Subcollections = grandchildren
		children = append(children, child)
		if *total >= graphNodeCap {
			*truncated = true
			break
		}
	}
	return children, nil
}

// ItemChildrenJSON returns bounded child items for a parent item key.
// PATCH(glean roadmap-phase5 bounded-graph): read children through indexed
// parent_key with an explicit cap for MCP callers.
func ItemChildrenJSON(key string) ([]byte, error) {
	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return graphUnsyncedJSON()
	}
	defer db.Close()

	qs := localQueryStore{db}
	found, err := graphItemExists(qs, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return graphNotFoundJSON(key)
	}

	rows, err := qs.QueryRaw(`
SELECT id AS key, item_type, json_extract(data,'$.data.title') AS title
FROM resources
WHERE resource_type='items' AND parent_key=?
ORDER BY title, id
LIMIT ?`, key, graphNodeCap+1)
	if err != nil {
		return nil, err
	}
	truncated := len(rows) > graphNodeCap
	if truncated {
		rows = rows[:graphNodeCap]
	}

	out := graphItemChildren{Key: key, Children: make([]graphItemChild, 0, len(rows)), Truncated: truncated}
	for _, row := range rows {
		out.Children = append(out.Children, graphItemChild{
			Key:      sqlStringValue(row["key"]),
			ItemType: sqlStringValue(row["item_type"]),
			Title:    sqlStringValue(row["title"]),
		})
	}
	return json.MarshalIndent(out, "", "  ")
}

// ItemAttachmentsJSON returns bounded attachment children for a parent item key.
// PATCH(glean roadmap-phase5 bounded-graph): expose attachment metadata while
// keeping traversal local and capped.
func ItemAttachmentsJSON(key string) ([]byte, error) {
	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return graphUnsyncedJSON()
	}
	defer db.Close()

	qs := localQueryStore{db}
	found, err := graphItemExists(qs, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return graphNotFoundJSON(key)
	}

	rows, err := qs.QueryRaw(`
SELECT id AS key,
       json_extract(data,'$.data.title') AS title,
       json_extract(data,'$.data.contentType') AS content_type,
       json_extract(data,'$.data.linkMode') AS link_mode
FROM resources
WHERE resource_type='items' AND parent_key=? AND item_type='attachment'
ORDER BY title, id
LIMIT ?`, key, graphNodeCap+1)
	if err != nil {
		return nil, err
	}
	truncated := len(rows) > graphNodeCap
	if truncated {
		rows = rows[:graphNodeCap]
	}

	out := graphItemAttachments{Key: key, Attachments: make([]graphItemAttachment, 0, len(rows)), Truncated: truncated}
	for _, row := range rows {
		out.Attachments = append(out.Attachments, graphItemAttachment{
			Key:         sqlStringValue(row["key"]),
			Title:       sqlStringValue(row["title"]),
			ContentType: sqlStringValue(row["content_type"]),
			LinkMode:    sqlStringValue(row["link_mode"]),
		})
	}
	return json.MarshalIndent(out, "", "  ")
}

// ItemContextJSON returns bounded local graph context for an item key.
// PATCH(glean roadmap-phase5 bounded-graph): summarize parent, membership, tags,
// and dependent counts with capped JSON-list expansion.
func ItemContextJSON(key string) ([]byte, error) {
	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return graphUnsyncedJSON()
	}
	defer db.Close()

	qs := localQueryStore{db}
	rows, err := qs.QueryRaw(`
SELECT id AS key,
       item_type,
       json_extract(data,'$.data.title') AS title,
       json_extract(data,'$.data.parentItem') AS parent
FROM resources
WHERE resource_type='items' AND id=?`, key)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return graphNotFoundJSON(key)
	}

	collections, collectionsTruncated, err := graphStringList(qs, `
SELECT collection.value AS value
FROM resources r, json_each(json_extract(r.data,'$.data.collections')) AS collection
WHERE r.resource_type='items' AND r.id=?
LIMIT ?`, key)
	if err != nil {
		return nil, err
	}
	tags, tagsTruncated, err := graphStringList(qs, `
SELECT json_extract(tag.value,'$.tag') AS value
FROM resources r, json_each(json_extract(r.data,'$.data.tags')) AS tag
WHERE r.resource_type='items' AND r.id=?
LIMIT ?`, key)
	if err != nil {
		return nil, err
	}
	childCount, err := graphCount(qs, `
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type='items' AND parent_key=?`, key)
	if err != nil {
		return nil, err
	}
	attachmentCount, err := graphCount(qs, `
SELECT COUNT(*) AS count
FROM resources
WHERE resource_type='items' AND parent_key=? AND item_type='attachment'`, key)
	if err != nil {
		return nil, err
	}

	out := graphItemContext{
		Key:             sqlStringValue(rows[0]["key"]),
		ItemType:        sqlStringValue(rows[0]["item_type"]),
		Title:           sqlStringValue(rows[0]["title"]),
		Parent:          sqlStringValue(rows[0]["parent"]),
		Collections:     collections,
		Tags:            tags,
		ChildCount:      childCount,
		AttachmentCount: attachmentCount,
		Truncated:       collectionsTruncated || tagsTruncated,
	}
	return json.MarshalIndent(out, "", "  ")
}

func graphItemExists(qs localQueryStore, key string) (bool, error) {
	rows, err := qs.QueryRaw(`
SELECT id
FROM resources
WHERE resource_type='items' AND id=?
LIMIT 1`, key)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

func graphStringList(qs localQueryStore, query string, key string) ([]string, bool, error) {
	rows, err := qs.QueryRaw(query, key, graphNodeCap+1)
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > graphNodeCap
	if truncated {
		rows = rows[:graphNodeCap]
	}
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		value := sqlStringValue(row["value"])
		if value != "" {
			values = append(values, value)
		}
	}
	return values, truncated, nil
}

func graphCount(qs localQueryStore, query string, key string) (int, error) {
	rows, err := qs.QueryRaw(query, key)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return sqlIntValue(rows[0]["count"]), nil
}
