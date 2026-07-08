// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// map Zotero Web API collection keys to desktop Connector tree targets.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"zotio/internal/connector"
)

type connectorTargetInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	Level         int    `json:"level"`
	FilesEditable bool   `json:"files_editable"`
}

type apiCollectionRow struct {
	Key  string `json:"key"`
	Data struct {
		Key              string `json:"key"`
		Name             string `json:"name"`
		ParentCollection any    `json:"parentCollection"`
	} `json:"data"`
}

func connectorCollectionKeyFromItem(item map[string]any) string {
	raw, ok := item["collections"]
	if !ok {
		return ""
	}
	switch collections := raw.(type) {
	case []string:
		if len(collections) > 0 {
			return strings.TrimSpace(collections[0])
		}
	case []any:
		for _, rawCollection := range collections {
			if collection, ok := rawCollection.(string); ok && strings.TrimSpace(collection) != "" {
				return strings.TrimSpace(collection)
			}
		}
	}
	return ""
}

func resolveConnectorTargetForItem(ctx context.Context, flags *rootFlags, conn *connector.Client, item map[string]any, collectionRequested bool) (string, error) {
	if strings.TrimSpace(flags.connectorTarget) != "" {
		return strings.TrimSpace(flags.connectorTarget), nil
	}
	if !collectionRequested {
		return "", nil
	}
	collectionKey := connectorCollectionKeyFromItem(item)
	if collectionKey == "" {
		return "", fmt.Errorf("--collection was requested but no collection key was present on the item")
	}
	return resolveConnectorTarget(ctx, flags, conn, collectionKey)
}

func resolveConnectorTarget(ctx context.Context, flags *rootFlags, conn *connector.Client, collectionKey string) (string, error) {
	apiPath, err := apiCollectionPath(flags, collectionKey)
	if err != nil {
		return "", err
	}
	selected, err := conn.SelectedCollection(ctx)
	if err != nil {
		return "", err
	}
	targets := connectorTargetPaths(selected)
	var matches []connectorTargetInfo
	for _, target := range targets {
		if target.Path == apiPath {
			matches = append(matches, target)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("collection %s maps to path %q, but no desktop connector target matched it; run 'zotio import targets --agent' and pass --connector-target C<n>", collectionKey, apiPath)
	default:
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		return "", fmt.Errorf("collection %s maps to ambiguous connector path %q (%s); pass --connector-target C<n>", collectionKey, apiPath, strings.Join(ids, ", "))
	}
}

func apiCollectionPath(flags *rootFlags, collectionKey string) (string, error) {
	c, err := flags.newClient()
	if err != nil {
		return "", err
	}
	c.NoCache = true
	var rows []apiCollectionRow
	for start := 0; ; start += 100 {
		data, err := c.Get("/collections", map[string]string{"limit": "100", "start": fmt.Sprintf("%d", start)})
		if err != nil {
			return "", classifyAPIError(err, flags)
		}
		var page []apiCollectionRow
		if err := json.Unmarshal(data, &page); err != nil {
			return "", fmt.Errorf("decoding collections for connector target resolution: %w", err)
		}
		rows = append(rows, page...)
		if len(page) < 100 {
			break
		}
	}
	paths := apiCollectionPaths(rows)
	path := paths[collectionKey]
	if path == "" {
		return "", fmt.Errorf("collection key %s was not found in the live Zotero collection list", collectionKey)
	}
	return path, nil
}

func apiCollectionPaths(rows []apiCollectionRow) map[string]string {
	byKey := make(map[string]apiCollectionRow, len(rows))
	for _, row := range rows {
		key := strings.TrimSpace(row.Key)
		if key == "" {
			key = strings.TrimSpace(row.Data.Key)
		}
		if key != "" {
			row.Key = key
			byKey[key] = row
		}
	}
	paths := make(map[string]string, len(rows))
	var build func(string) string
	build = func(key string) string {
		if path := paths[key]; path != "" {
			return path
		}
		row, ok := byKey[key]
		if !ok {
			return ""
		}
		name := strings.TrimSpace(row.Data.Name)
		parent := collectionParentKey(row.Data.ParentCollection)
		if parent == "" {
			paths[key] = name
			return name
		}
		parentPath := build(parent)
		if parentPath == "" {
			paths[key] = name
			return name
		}
		paths[key] = parentPath + "/" + name
		return paths[key]
	}
	for key := range byKey {
		build(key)
	}
	return paths
}

func collectionParentKey(raw any) string {
	switch parent := raw.(type) {
	case string:
		return strings.TrimSpace(parent)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(parent))
	}
}

func connectorTargetPaths(selected connector.SelectedCollection) []connectorTargetInfo {
	paths := make([]connectorTargetInfo, 0, len(selected.Targets))
	stack := map[int]string{}
	for _, target := range selected.Targets {
		name := strings.TrimSpace(target.Name)
		if strings.HasPrefix(target.ID, "L") {
			stack[0] = name
			continue
		}
		level := target.Level
		if level < 1 {
			level = 1
		}
		stack[level] = name
		for stale := range stack {
			if stale > level {
				delete(stack, stale)
			}
		}
		parts := make([]string, 0, level)
		for i := 1; i <= level; i++ {
			if part := stack[i]; part != "" {
				parts = append(parts, part)
			}
		}
		paths = append(paths, connectorTargetInfo{ID: target.ID, Name: name, Path: strings.Join(parts, "/"), Level: target.Level, FilesEditable: target.FilesEditable})
	}
	return paths
}
