// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase2): shared local scope resolver for trust-contract commands.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PATCH(glean roadmap-phase2): captures one parsed item-cohort scope expression before store resolution.
type scopeSpec struct {
	Type  string
	Value string
}

// PATCH(glean roadmap-phase2): parses the shared scope grammar without losing colons inside query text.
func parseScopeSpec(expr string) (scopeSpec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "library" {
		return scopeSpec{Type: "library"}, nil
	}

	scopeType, value, ok := strings.Cut(expr, ":")
	if !ok {
		return scopeSpec{}, fmt.Errorf("unknown scope type %q", expr)
	}

	scopeType = strings.TrimSpace(scopeType)
	value = strings.TrimSpace(value)
	switch scopeType {
	case "collection", "tag", "item", "query", "saved-search":
		if value == "" {
			return scopeSpec{}, fmt.Errorf("scope %q requires a value", scopeType)
		}
		return scopeSpec{Type: scopeType, Value: value}, nil
	default:
		return scopeSpec{}, fmt.Errorf("unknown scope type %q", scopeType)
	}
}

// PATCH(glean roadmap-phase2): describes a resolved local item cohort plus any unmet live precondition.
type scopeResult struct {
	Expr         string
	Type         string
	Keys         []string
	All          bool
	Precondition string
}

// PATCH(glean roadmap-phase2): resolves the shared scope grammar against the synced local store.
func resolveScope(db localQueryStore, spec scopeSpec) (scopeResult, error) {
	result := scopeResult{
		Type: spec.Type,
		Keys: make([]string, 0),
	}

	switch spec.Type {
	case "library":
		result.Expr = "library"
		result.All = true
		return result, nil
	case "item":
		if spec.Value == "" {
			return scopeResult{}, fmt.Errorf("scope %q requires a value", spec.Type)
		}
		result.Expr = "item:" + spec.Value
		result.Keys = append(result.Keys, spec.Value)
		return result, nil
	case "collection":
		if spec.Value == "" {
			return scopeResult{}, fmt.Errorf("scope %q requires a value", spec.Type)
		}
		result.Expr = "collection:" + spec.Value
		rows, err := db.QueryRaw(`SELECT id AS key FROM resources WHERE resource_type='items' AND EXISTS (SELECT 1 FROM json_each(json_extract(data,'$.data.collections')) WHERE value = ?)`, spec.Value)
		if err != nil {
			return scopeResult{}, fmt.Errorf("resolving collection scope: %w", err)
		}
		for _, row := range rows {
			if key := sqlStringValue(row["key"]); key != "" {
				result.Keys = append(result.Keys, key)
			}
		}
		return result, nil
	case "tag":
		if spec.Value == "" {
			return scopeResult{}, fmt.Errorf("scope %q requires a value", spec.Type)
		}
		result.Expr = "tag:" + spec.Value
		rows, err := db.QueryRaw(`SELECT DISTINCT r.id AS key FROM resources r, json_each(json_extract(r.data,'$.data.tags')) t WHERE r.resource_type='items' AND json_extract(t.value,'$.tag') = ?`, spec.Value)
		if err != nil {
			return scopeResult{}, fmt.Errorf("resolving tag scope: %w", err)
		}
		for _, row := range rows {
			if key := sqlStringValue(row["key"]); key != "" {
				result.Keys = append(result.Keys, key)
			}
		}
		return result, nil
	case "query":
		if spec.Value == "" {
			return scopeResult{}, fmt.Errorf("scope %q requires a value", spec.Type)
		}
		result.Expr = "query:" + spec.Value
		items, err := db.Search(spec.Value, 0)
		if err != nil {
			return scopeResult{}, fmt.Errorf("resolving query scope: %w", err)
		}
		for _, raw := range items {
			var item struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return scopeResult{}, fmt.Errorf("decoding query scope item: %w", err)
			}
			if item.Key != "" {
				result.Keys = append(result.Keys, item.Key)
			}
		}
		return result, nil
	case "saved-search":
		if spec.Value == "" {
			return scopeResult{}, fmt.Errorf("scope %q requires a value", spec.Type)
		}
		result.Expr = "saved-search:" + spec.Value
		result.Precondition = "live_local_api"
		return result, nil
	default:
		return scopeResult{}, fmt.Errorf("unknown scope type %q", spec.Type)
	}
}
