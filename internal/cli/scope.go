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
	"sort"
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
//
// Arms intersect: `collection:KEY AND tag:NAME` is the items in both. The token
// is a space, uppercase AND, and a space, and it splits only where the text
// after it starts with another arm, so `query:trust AND safety` stays one
// query. The help text names the AND form once, here, for the same reason it
// names each arm once.
const scopeArmsUsage = "library | collection:KEY | tag:NAME | item:KEY | query:TEXT | saved-search:KEY"

const scopeFlagUsage = "Item cohort: " + scopeArmsUsage + "; combine arms with AND to intersect, e.g. collection:KEY AND tag:NAME"

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

// scopeTypeIntersection is the Type of a compound spec: the AND of Arms.
const scopeTypeIntersection = "intersection"

// scopeAndToken joins the arms of an intersection. It is a whole token: a
// space, uppercase AND, a space. A lowercase "and" or an AND glued to text
// never splits an expression.
const scopeAndToken = " AND "

// scopeArmPrefixes are the arm names that may follow an AND. A split happens
// only before one of these, so a query or tag that merely contains " AND "
// stays whole.
var scopeArmPrefixes = []string{"collection:", "tag:", "item:", "query:", "saved-search:"}

// captures one parsed item-cohort scope expression before store resolution.
// A single arm has Type and Value. An intersection has Type
// scopeTypeIntersection, two or more single-arm Arms, and no Value.
type scopeSpec struct {
	Type  string
	Value string
	Arms  []scopeSpec
}

// expr renders the spec in the canonical grammar, with each arm trimmed and
// joined by one AND token.
func (s scopeSpec) expr() string {
	switch s.Type {
	case "library":
		return "library"
	case scopeTypeIntersection:
		parts := make([]string, len(s.Arms))
		for i, arm := range s.Arms {
			parts[i] = arm.expr()
		}
		return strings.Join(parts, scopeAndToken)
	default:
		return s.Type + ":" + s.Value
	}
}

// parses the shared scope grammar without losing colons inside query text.
// Arms joined by AND parse to an intersection; one arm parses exactly as it
// always did.
func parseScopeSpec(expr string) (scopeSpec, error) {
	expr = strings.TrimSpace(expr)
	parts := splitScopeArms(expr)
	if len(parts) == 1 {
		return parseScopeArm(expr)
	}
	arms := make([]scopeSpec, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "AND" || strings.HasSuffix(part, scopeAndToken[:4]) {
			return scopeSpec{}, fmt.Errorf("scope %q has an empty operand next to AND; name an arm on each side", expr)
		}
		arm, err := parseScopeArm(part)
		if err != nil {
			return scopeSpec{}, err
		}
		if arm.Type == "library" {
			return scopeSpec{}, fmt.Errorf("scope %q cannot combine library with AND: library already selects every item; drop it or name an arm", expr)
		}
		arms = append(arms, arm)
	}
	return scopeSpec{Type: scopeTypeIntersection, Arms: arms}, nil
}

// splitScopeArms cuts expr at each AND token that is followed by an arm
// prefix (or by the bare word library, which parseScopeSpec then refuses with
// its own message). Any other " AND " belongs to the arm text.
func splitScopeArms(expr string) []string {
	var parts []string
	start := 0
	for from := 0; from < len(expr); {
		i := strings.Index(expr[from:], scopeAndToken)
		if i < 0 {
			break
		}
		at := from + i
		rest := strings.TrimLeft(expr[at+len(scopeAndToken):], " ")
		if startsScopeArm(rest) {
			parts = append(parts, expr[start:at])
			start = at + len(scopeAndToken)
		}
		// Advance one byte, not past the token, so "x AND AND tag:y" still
		// finds the second token and reports the empty operand.
		from = at + 1
	}
	return append(parts, expr[start:])
}

func startsScopeArm(text string) bool {
	if text == "library" {
		return true
	}
	for _, prefix := range scopeArmPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// parseScopeArm parses one arm; it never sees AND.
func parseScopeArm(expr string) (scopeSpec, error) {
	expr = strings.TrimSpace(expr)
	if expr == "library" {
		return scopeSpec{Type: "library"}, nil
	}

	scopeType, value, ok := strings.Cut(expr, ":")
	if !ok {
		return scopeSpec{}, unknownScopeTypeErr(expr)
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
		return scopeSpec{}, unknownScopeTypeErr(scopeType)
	}
}

// unknownScopeTypeErr names the accepted arms from the one canonical help
// string, so every --scope consumer reports the same vocabulary.
func unknownScopeTypeErr(got string) error {
	return fmt.Errorf("unknown scope type %q; valid: %s", got, scopeArmsUsage)
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
	// Arms lists each operand of an intersection as written (canonical
	// form); Expr is the whole expression. Empty for a single arm.
	Arms []string
	// LocalEvaluation is non-empty when a saved-search cohort came from the
	// local mirror instead of Zotero: the fallback used when the desktop
	// local API could not answer (see savedSearchLocalFallback). It says
	// which search, how fresh the mirror was, and why live was skipped.
	LocalEvaluation string
}

// resolveScopeIntersection resolves each distinct operand of an intersection
// through resolveArm and returns the sorted unique keys present in every
// operand. An operand is resolved once however often it repeats. Every
// operand is resolved before the result is judged, so an operand with an
// unmet precondition (a saved search nobody can evaluate) is reported, never
// folded into an empty cohort: the result then carries that Precondition and
// no keys, and each adopter refuses through scopePreconditionErr.
func resolveScopeIntersection(spec scopeSpec, resolveArm func(scopeSpec) (scopeResult, error)) (scopeResult, error) {
	if len(spec.Arms) < 2 {
		return scopeResult{}, fmt.Errorf("scope intersection needs at least two arms, got %d", len(spec.Arms))
	}
	result := scopeResult{
		Expr: spec.expr(),
		Type: scopeTypeIntersection,
		Keys: make([]string, 0),
		Arms: make([]string, 0, len(spec.Arms)),
	}
	seen := make(map[string]bool, len(spec.Arms))
	var common map[string]bool
	for _, arm := range spec.Arms {
		if arm.Type == "library" || arm.Type == scopeTypeIntersection {
			return scopeResult{}, fmt.Errorf("scope %q cannot be an operand of AND", arm.expr())
		}
		armExpr := arm.expr()
		result.Arms = append(result.Arms, armExpr)
		if seen[armExpr] {
			continue
		}
		seen[armExpr] = true
		resolved, err := resolveArm(arm)
		if err != nil {
			return scopeResult{}, err
		}
		if resolved.Precondition != "" && result.Precondition == "" {
			result.Precondition = resolved.Precondition
			result.PreconditionDetail = resolved.PreconditionDetail
		}
		if resolved.LocalEvaluation != "" {
			if result.LocalEvaluation != "" {
				result.LocalEvaluation += "; "
			}
			result.LocalEvaluation += resolved.LocalEvaluation
		}
		keys := make(map[string]bool, len(resolved.Keys))
		for _, key := range resolved.Keys {
			keys[key] = true
		}
		if common == nil {
			common = keys
			continue
		}
		for key := range common {
			if !keys[key] {
				delete(common, key)
			}
		}
	}
	if result.Precondition != "" {
		// A refused cohort carries no provenance for a cohort that never formed.
		result.LocalEvaluation = ""
		return result, nil
	}
	for key := range common {
		result.Keys = append(result.Keys, key)
	}
	sort.Strings(result.Keys)
	return result, nil
}

// resolves the shared scope grammar against the synced local store.
func resolveScope(db localQueryStore, spec scopeSpec) (scopeResult, error) {
	if spec.Type == scopeTypeIntersection {
		return resolveScopeIntersection(spec, func(arm scopeSpec) (scopeResult, error) { return resolveScope(db, arm) })
	}
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
// configured base, does not answer, or refuses local access (HTTP 403), the
// local mirror takes over if it can reproduce the search: the synced
// definition is evaluated against the mirrored items (see
// savedsearch_local.go) and the result carries LocalEvaluation. A definition
// outside that evaluator's supported subset, or absent from the mirror, is
// never evaluated partially: the result keeps Precondition set with
// PreconditionDetail naming why live failed and what the mirror could not
// evaluate, and a nil error; each adopter then refuses in its own voice (see
// scopePreconditionErr). --data-source live disables the fallback. A 404 for
// the key is an error naming the key, never an empty cohort. With a mirror
// (db.Store != nil), a live key the mirror does not hold is a freshness
// refusal (exit 12): every mirror-based adopter would otherwise skip that
// item without saying so.
func resolveScopeLive(ctx context.Context, flags *rootFlags, db localQueryStore, spec scopeSpec) (scopeResult, error) {
	if spec.Type == scopeTypeIntersection {
		return resolveScopeIntersection(spec, func(arm scopeSpec) (scopeResult, error) {
			return resolveScopeLive(ctx, flags, db, arm)
		})
	}
	result, err := resolveScope(db, spec)
	if err != nil || result.Precondition == "" || spec.Type != "saved-search" {
		return result, err
	}
	if flags == nil {
		// Native MCP reads share the active library with the mirror path.
		// Preserve that group as well as the request context and timeout.
		flags = &rootFlags{ctx: ctx, timeout: defaultRequestTimeout, group: activeGroupIDLocked()}
	}
	c, detail, err := savedSearchLiveClient(ctx, flags)
	if err != nil {
		return scopeResult{}, err
	}
	if c == nil {
		result.PreconditionDetail = detail
		return savedSearchLocalFallback(ctx, flags, db, spec.Value, result), nil
	}
	// Membership changes whenever an item starts or stops matching, so a
	// cached page (up to 5 minutes old) is a stale cohort that a write
	// command would then plan against. Set here, not in the client seam, so
	// no seam can drop it.
	c.NoCache = true
	path := "/searches/" + url.PathEscape(spec.Value) + "/items"
	items, err := searchesMaterializeItems(c, path, "saved search "+spec.Value)
	if err != nil {
		if isAPIStatus(err, http.StatusNotFound) {
			return scopeResult{}, fmt.Errorf("saved search %s not found on the Zotero desktop local API: %w", spec.Value, err)
		}
		if isAPIStatus(err, http.StatusForbidden) {
			// Zotero answers 403 while local API access is switched off. The
			// reachability probe counts any HTTP answer as reachable, so the
			// refusal surfaces here.
			result.PreconditionDetail = fmt.Sprintf("Zotero refused saved search %s with HTTP 403 because local API access is off; enable Settings → Advanced → \"Allow other applications on this computer to communicate with Zotero\"", spec.Value)
			return savedSearchLocalFallback(ctx, flags, db, spec.Value, result), nil
		}
		if isNetworkError(err) {
			// Zotero went away between the probe and the read.
			result.PreconditionDetail = fmt.Sprintf("Zotero desktop local API stopped answering while executing saved search %s: %v", spec.Value, err)
			return savedSearchLocalFallback(ctx, flags, db, spec.Value, result), nil
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
	if db.Store != nil {
		if err := savedSearchRequireMirrored(db, spec.Value, result.Keys); err != nil {
			return scopeResult{}, err
		}
	}
	result.Precondition = ""
	return result, nil
}

// savedSearchRequireMirrored refuses (exit 12) when a live saved-search key
// has no live row in the mirror. Mirror-based adopters read only
// resource_type 'items', so such an item would be skipped silently, and
// `library health` could report passed over items it never checked. An item
// the mirror holds only in 'items-trash' counts as missing for the same
// reason: no adopter would read it.
func savedSearchRequireMirrored(db localQueryStore, searchKey string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("encoding saved search %s keys: %w", searchKey, err)
	}
	rows, err := db.QueryRaw(`SELECT j.value AS key FROM json_each(?) j WHERE NOT EXISTS (SELECT 1 FROM resources r WHERE r.resource_type='items' AND r.id = j.value) ORDER BY j.key`, string(encoded))
	if err != nil {
		return fmt.Errorf("checking saved search %s against the local mirror: %w", searchKey, err)
	}
	if len(rows) == 0 {
		return nil
	}
	shown := make([]string, 0, 5)
	for _, row := range rows {
		if len(shown) == cap(shown) {
			break
		}
		shown = append(shown, sqlStringValue(row["key"]))
	}
	list := strings.Join(shown, ", ")
	if len(rows) > len(shown) {
		list += ", ..."
	}
	return freshnessErr(fmt.Errorf("saved search %s returns %d item(s) the local mirror does not have (%s); run 'zotio sync' and retry", searchKey, len(rows), list))
}

// scopePreconditionErr is the shared loud refusal for a scope whose live
// precondition resolveScopeLive could not meet: a precondition_unmet
// envelope (exit 9) with the remediation for live_local_api. A nil out skips
// the machine envelope and puts the remediation in the error text instead.
func scopePreconditionErr(ctx context.Context, out io.Writer, flags *rootFlags, capability string, result scopeResult) error {
	if flags != nil && flags.quiet {
		out = nil
	}
	detail := fmt.Sprintf("scope %q is evaluated by Zotero, and the local mirror could not reproduce it, so it resolves no items while the desktop local API is unreachable", result.Expr)
	if result.PreconditionDetail != "" {
		detail += " (" + result.PreconditionDetail + ")"
	}
	return emitPreconditionUnmetWithRemediation(out, flags, capability, result.Precondition, detail, remediationFor(ctx, flags, result.Precondition))
}
