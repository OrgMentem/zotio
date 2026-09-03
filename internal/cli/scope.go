// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// shared local scope resolver for trust-contract commands.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- the one --scope flag registration ---
//
// Eleven commands register a --scope flag against the grammar below. Before
// this was centralized they carried four separately worded copies of the same
// help text, and the wordings had drifted apart: three named `library` but not
// `saved-search`, one named `saved-search` but not `library`, and two spelled
// the same arms as `collection:<key>` rather than `collection:KEY`. A reader
// comparing two --help outputs read them as two grammars. The text therefore
// lives beside parseScopeSpec, which is what actually defines it, and every arm
// named here is an arm parseScopeSpec accepts.
//
// Runtime preconditions are deliberately absent from this string.
// saved-search:KEY has no local mirror, so resolveScope reports
// live_local_api and each adopter refuses with exit 9 plus a remediation
// envelope — louder and more precise than a caveat in a flag description. A
// command whose limitation is permanent rather than environmental appends it
// (see items_bibliography.go, which can never render a saved search).
const scopeFlagUsage = "Item cohort: library | collection:KEY | tag:NAME | item:KEY | query:TEXT | saved-search:KEY"

// scopeFlagUsageDefaultLibrary is for the adopters whose flag defaults to the
// empty string. Cobra prints "(default ...)" only for a non-empty default, so
// without this clause their help never says what omitting --scope does.
const scopeFlagUsageDefaultLibrary = scopeFlagUsage + " (default: the whole library)"

// scopeFlagUsageRequired is for `import discover`, which has no meaningful
// library-wide run: it mines candidate DOIs out of a chosen cohort, and the
// whole library would be neither bounded nor reviewable.
const scopeFlagUsageRequired = scopeFlagUsage + " (required)"

// Both default spellings are live and they are NOT interchangeable.
//
// scopeFlagDefaultLibrary is for a command that parses --scope
// unconditionally: it needs a parseable expression, and "" would fail with
// `unknown scope type ""`.
//
// scopeFlagDefaultUnset is for a command that has to tell "no cohort was
// asked for" from "the library cohort was asked for". `items enrich` and
// `vault sync` reconcile their older selection flags against --scope, so an
// expression default would make `--collection KEY` alone look like a
// disagreement and be refused.
const (
	scopeFlagDefaultLibrary = "library"
	scopeFlagDefaultUnset   = ""
)

// captures one parsed item-cohort scope expression before store resolution.
type scopeSpec struct {
	Type  string
	Value string
}

// parses the shared scope grammar without losing colons inside query text.
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

// describes a resolved local item cohort plus any unmet live precondition.
type scopeResult struct {
	Expr         string
	Type         string
	Keys         []string
	All          bool
	Precondition string
}

// resolves the shared scope grammar against the synced local store.
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
		items, err := db.Search(spec.Value, -1)
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
