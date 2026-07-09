// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// capture canonical item content for reproducible exports.

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

const exportLockfileSchemaVersion = 2

type exportLockItem struct {
	Key           string `json:"key"`
	Version       int    `json:"version"`
	Title         string `json:"title,omitempty"`
	ContentSHA256 string `json:"content_sha256"`
}

type exportLockfile struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   string           `json:"generated_at"`
	Scope         string           `json:"scope"`
	Format        string           `json:"format"`
	Count         int              `json:"count"`
	ContentSHA256 string           `json:"content_sha256"`
	Items         []exportLockItem `json:"items"`
}

func buildExportLockfile(scope, format string, items []json.RawMessage) exportLockfile {
	lockItems := make([]exportLockItem, 0, len(items))
	for _, raw := range items {
		key := exportItemKey(raw)
		if key == "" {
			continue
		}
		contentSHA, err := exportItemContentSHA256(raw)
		if err != nil {
			continue
		}
		lockItems = append(lockItems, exportLockItem{
			Key:           key,
			Version:       exportItemVersion(raw),
			Title:         exportItemTitle(raw),
			ContentSHA256: contentSHA,
		})
	}

	sort.Slice(lockItems, func(i, j int) bool {
		return lockItems[i].Key < lockItems[j].Key
	})

	hash := sha256.New()
	for _, item := range lockItems {
		if item.ContentSHA256 == "" {
			_, _ = fmt.Fprintf(hash, "%s:%d\n", item.Key, item.Version)
			continue
		}
		_, _ = fmt.Fprintf(hash, "%s:%s\n", item.Key, item.ContentSHA256)
	}

	return exportLockfile{
		SchemaVersion: exportLockfileSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Scope:         scope,
		Format:        format,
		Count:         len(lockItems),
		ContentSHA256: hex.EncodeToString(hash.Sum(nil)),
		Items:         lockItems,
	}
}

func writeExportLockfile(w io.Writer, lf exportLockfile) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(lf)
}

func readExportLockfile(path string) (exportLockfile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return exportLockfile{}, fmt.Errorf("reading lockfile: %w", err)
	}
	var lf exportLockfile
	if err := json.Unmarshal(raw, &lf); err != nil {
		return exportLockfile{}, fmt.Errorf("decoding lockfile: %w", err)
	}
	sort.Slice(lf.Items, func(i, j int) bool {
		return lf.Items[i].Key < lf.Items[j].Key
	})
	return lf, nil
}

func exportItemKey(raw json.RawMessage) string {
	return jsonStringField(raw, "key")
}

func exportItemTitle(raw json.RawMessage) string {
	return jsonStringField(raw, "title")
}

func exportItemVersion(raw json.RawMessage) int {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0
	}
	if version, ok := intFromJSONValue(obj["version"]); ok {
		return version
	}
	if dataObj, ok := obj["data"].(map[string]any); ok {
		if version, ok := intFromJSONValue(dataObj["version"]); ok {
			return version
		}
	}
	return 0
}

func intFromJSONValue(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func exportItemContentSHA256(raw json.RawMessage) (string, error) {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	hasData := false
	if outer, ok := obj.(map[string]any); ok {
		if data, ok := outer["data"]; ok {
			obj = data
			hasData = true
		}
	}
	normalized := normalizeExportItemContent(obj)
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	if !hasData && string(canonical) == "{}" {
		return "", nil
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeExportItemContent(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, child := range v {
			if exportVolatileItemField(key) {
				continue
			}
			out[key] = normalizeExportItemContent(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = normalizeExportItemContent(child)
		}
		return out
	default:
		return v
	}
}

func exportVolatileItemField(key string) bool {
	switch key {
	case "key", "version", "dateAdded", "dateModified":
		return true
	default:
		return false
	}
}
