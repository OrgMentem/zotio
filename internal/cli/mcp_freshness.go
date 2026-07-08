// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"time"
)

// FreshnessJSON returns per-resource local sync freshness for MCP consumers.
func FreshnessJSON() ([]byte, error) {
	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return json.MarshalIndent(map[string]any{"synced": false, "note": "local store not synced; run sync"}, "", "  ")
	}
	defer db.Close()

	qs := localQueryStore{db}
	rows, err := qs.QueryRaw("SELECT resource_type FROM sync_state ORDER BY resource_type")
	if err != nil {
		return nil, err
	}

	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		rt := sqlStringValue(row["resource_type"])
		_, lastSynced, count, _ := db.GetSyncState(rt)
		age := time.Since(lastSynced)
		entry := map[string]any{
			"resource":       rt,
			"count":          count,
			"last_synced_at": lastSynced.UTC().Format(time.RFC3339),
			"age_seconds":    int(age.Seconds()),
			"age":            durationAgo(age),
		}
		if lastSynced.IsZero() {
			entry["last_synced_at"] = ""
			entry["age"] = "never"
			entry["age_seconds"] = 0
		}
		entries = append(entries, entry)
	}

	return json.MarshalIndent(map[string]any{
		"synced":       true,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"resources":    entries,
	}, "", "  ")
}
