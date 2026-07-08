// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// HealthJSON runs the local library-health report for scopeExpr and returns its
// indented JSON form for MCP callers.
func HealthJSON(scopeExpr string) ([]byte, error) {
	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return nil, err
	}
	if db == nil {
		return json.MarshalIndent(map[string]any{
			"synced": false,
			"note":   "local store not synced; run sync",
		}, "", "  ")
	}
	defer db.Close()
	qs := localQueryStore{db}

	trimmed := strings.TrimSpace(scopeExpr)
	scope := scopeResult{All: true, Expr: "library"}
	if trimmed != "" && trimmed != "library" {
		spec, err := parseScopeSpec(trimmed)
		if err != nil {
			return nil, err
		}
		scope, err = resolveScope(qs, spec)
		if err != nil {
			return nil, err
		}
		if scope.Precondition != "" {
			return json.MarshalIndent(map[string]any{
				"scope":        scope.Expr,
				"precondition": scope.Precondition,
				"note":         "scope requires a live precondition; cannot run from the local store",
			}, "", "  ")
		}
	}

	var syncedAt *time.Time
	if _, lastSynced, _, e := db.GetSyncState("items"); e == nil && !lastSynced.IsZero() {
		v := lastSynced
		syncedAt = &v
	}
	ctx := &healthContext{
		src:    healthSource{Kind: "local", SyncedAt: syncedAt},
		preset: "all",
		limit:  0,
	}

	report, err := assembleHealthReport(qs, ctx, "all", healthPresets["all"], "", scope)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(report, "", "  ")
}
