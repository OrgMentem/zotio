// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// shared local scope resolver for trust-contract commands.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"zotio/internal/client"
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
// saved-search:KEY has no local mirror, so resolveScopeLive executes it
// through the Zotero desktop local API; when that API is not the configured
// base or is not reachable, each adopter refuses with exit 9 plus a
// remediation envelope — louder and more precise than a caveat in a flag
// description.
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
	// PreconditionDetail says why Precondition is still unmet after a live
	// attempt (wrong plane, desktop unreachable). Empty from resolveScope.
	PreconditionDetail string
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

// savedSearchLiveClient returns the desktop local API read client, or an
// unmet-precondition detail when the configured base is not that API or it
// does not answer. It applies the same gate as the live_local_api preflight.
// Tests replace it: the gate requires the fixed desktop port 23119, which a
// test cannot bind.
var savedSearchLiveClient = func(ctx context.Context, flags *rootFlags) (*client.Client, string, error) {
	ok, detail, err := checkLiveLocalAPIPrecondition(ctx, flags, nil, capabilityEntry{})
	if err != nil || !ok {
		return nil, detail, err
	}
	c, err := flags.newClient()
	return c, "", err
}

// resolveScopeLive is resolveScope plus live execution of saved-search:KEY.
// A saved search is evaluated by Zotero, so its keys come from
// /searches/KEY/items on the desktop local API. When that API is not the
// configured base or does not answer, the result keeps Precondition set with
// PreconditionDetail explaining why and a nil error; each adopter then
// refuses in its own voice (see scopePreconditionErr). A 404 for the key is
// an error naming the key, never an empty cohort.
func resolveScopeLive(ctx context.Context, flags *rootFlags, db localQueryStore, spec scopeSpec) (scopeResult, error) {
	result, err := resolveScope(db, spec)
	if err != nil || result.Precondition == "" || spec.Type != "saved-search" {
		return result, err
	}
	if flags == nil {
		flags = &rootFlags{}
	}
	c, detail, err := savedSearchLiveClient(ctx, flags)
	if err != nil {
		return scopeResult{}, err
	}
	if c == nil {
		result.PreconditionDetail = detail
		return result, nil
	}
	path := "/searches/" + url.PathEscape(spec.Value) + "/items"
	items, err := searchesMaterializeItems(c, path, "saved search "+spec.Value)
	if err != nil {
		if isAPIStatus(err, http.StatusNotFound) {
			return scopeResult{}, fmt.Errorf("saved search %s not found on the Zotero desktop local API: %w", spec.Value, err)
		}
		if isNetworkError(err) {
			// Zotero went away between the probe and the read.
			result.PreconditionDetail = fmt.Sprintf("Zotero desktop local API stopped answering while executing saved search %s: %v", spec.Value, err)
			return result, nil
		}
		return scopeResult{}, err
	}
	// Child attachments and notes are skipped: the collection: arm matches on
	// data.collections, which Zotero leaves empty for child items, so a
	// collection cohort holds top-level items only. A saved search can return
	// children (searches materialize observes this), and they are dropped
	// here so both arms name the same kind of cohort.
	for _, item := range items {
		if item.Data.ParentItem == "" && item.Key != "" {
			result.Keys = append(result.Keys, item.Key)
		}
	}
	result.Precondition = ""
	return result, nil
}

// scopePreconditionErr is the shared loud refusal for a scope whose live
// precondition resolveScopeLive could not meet: a precondition_unmet
// envelope (exit 9) with the remediation for live_local_api. A nil out skips
// the machine envelope and puts the remediation in the error text instead.
func scopePreconditionErr(ctx context.Context, out io.Writer, flags *rootFlags, capability string, result scopeResult) error {
	if flags != nil && flags.quiet {
		out = nil
	}
	detail := fmt.Sprintf("scope %q is evaluated by Zotero and has no local mirror, so it resolves no items while the desktop local API is unreachable", result.Expr)
	if result.PreconditionDetail != "" {
		detail += " (" + result.PreconditionDetail + ")"
	}
	return emitPreconditionUnmetWithRemediation(out, flags, capability, result.Precondition, detail, remediationFor(ctx, flags, result.Precondition))
}
