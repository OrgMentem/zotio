// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase6 d27f99d4): capture canonical item versions for reproducible exports.

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

const exportLockfileSchemaVersion = 1

type exportLockItem struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
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
		var item struct {
			Key     string `json:"key"`
			Version int    `json:"version"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.Key == "" {
			continue
		}
		lockItems = append(lockItems, exportLockItem{
			Key:     item.Key,
			Version: item.Version,
		})
	}

	sort.Slice(lockItems, func(i, j int) bool {
		return lockItems[i].Key < lockItems[j].Key
	})

	hash := sha256.New()
	for _, item := range lockItems {
		_, _ = fmt.Fprintf(hash, "%s:%d\n", item.Key, item.Version)
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
