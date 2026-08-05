// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Duplicate resolution mutation command.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"zotio/internal/client"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

const duplicateResolveStrategyKeepMostComplete = "keep-most-complete"

// Centralize the high-risk title matcher warning for CLI output and tests.
const duplicateResolveTitleWarning = "warning: --title matches by title and can group distinct items; review the preview carefully before applying"

// duplicateResolveMergeKind is the planned op kind for every duplicate merge.
// duplicateResolveMergePartialKind replaces it, in place on that op, only when
// the merge-target PATCH committed but the follow-up trash PATCH failed — see
// applyDuplicateResolve. Because it is written directly into the op the
// engine already holds, it is what `journal show` later prints, distinguishing
// a half-applied merge from a clean one in the permanent record.
const duplicateResolveMergeKind = "duplicate_merge"
const duplicateResolveMergePartialKind = "duplicate_merge_partial"

type duplicateResolveItem struct {
	Key          string
	Version      int
	Data         map[string]any
	Collections  []string
	Tags         []map[string]any
	Completeness int
	HasDOI       bool
	HasPDF       bool
	Deleted      bool
}

func newItemsDuplicatesResolveCmd(flags *rootFlags) *cobra.Command {
	var strategy string
	var flagDOI bool
	var flagTitle bool

	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Resolve duplicate items by merging metadata onto a master and trashing duplicates",
		// Document DOI-only safe default and explicit risky title matching opt-in.
		Long: `Resolve duplicate items by merging metadata onto a master and trashing duplicates.

By default, resolve only matches duplicate DOI groups. Title matching can group
distinct items that share generic titles; pass --title only when you are ready to
review the preview carefully before applying.`,
		Example: `  zotio items duplicates resolve
  zotio items duplicates resolve --doi
  zotio items duplicates resolve --title
  zotio items duplicates resolve --doi --title --yes`,
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strategy != duplicateResolveStrategyKeepMostComplete {
				return fmt.Errorf("invalid --strategy value %q: must be %s", strategy, duplicateResolveStrategyKeepMostComplete)
			}

			// Default unresolved flag state to DOI-only instead of DOI+title.
			includeDOI, includeTitle := duplicateResolveIncludes(cmd, flagDOI, flagTitle)
			if includeTitle {
				fmt.Fprintln(cmd.ErrOrStderr(), duplicateResolveTitleWarning)
			}

			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				env, runErr := runMutation(cmd.Context(), flags, "items.duplicates.resolve", nil)
				renderErr := renderMutation(cmd, flags, env, itemsDuplicatesResolveSingleLine)
				if renderErr != nil {
					return renderErr
				}
				return runErr
			}
			defer rawDB.Close()

			ops, err := buildDuplicateResolveOps(localQueryStore{Store: rawDB}, flags, includeDOI, includeTitle)
			if err != nil {
				return err
			}
			env, runErr := runMutation(cmd.Context(), flags, "items.duplicates.resolve", ops)
			duplicateResolveSurfacePartialWarnings(&env, ops)
			renderErr := renderMutation(cmd, flags, env, itemsDuplicatesResolveSingleLine)
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&strategy, "strategy", duplicateResolveStrategyKeepMostComplete, "Resolution strategy (keep-most-complete)")
	cmd.Flags().BoolVar(&flagDOI, "doi", false, "Resolve duplicate DOI groups")
	// Flag help names title matching as an explicit riskier opt-in.
	cmd.Flags().BoolVar(&flagTitle, "title", false, "Opt into riskier duplicate title groups")

	return cmd
}

// No explicit matcher flags means safe DOI-only matching.
func duplicateResolveIncludes(cmd *cobra.Command, flagDOI, flagTitle bool) (bool, bool) {
	if !cmd.Flags().Changed("doi") && !cmd.Flags().Changed("title") {
		return true, false
	}
	return flagDOI, flagTitle
}

// duplicateResolvePending is one planned merge, computed before its
// mutation.Op is built. Building the ops slice happens in a second, fixed-size
// pass (see buildDuplicateResolveOps) so the &ops[i] pointer each op's Apply
// closure captures — to flip its own Kind on a half-applied outcome — stays
// valid: it must never be invalidated by a later append reallocating the
// backing array.
type duplicateResolvePending struct {
	id              string
	masterKey       string
	dupKey          string
	expectedVersion int
	changes         []mutation.Change
}

func buildDuplicateResolveOps(db localQueryStore, flags *rootFlags, includeDOI, includeTitle bool) ([]mutation.Op, error) {
	rows, err := duplicateResolveRows(db, includeDOI, includeTitle)
	if err != nil {
		return nil, fmt.Errorf("querying duplicates: %w", err)
	}
	pdfByParent, err := duplicateResolvePDFParents(db)
	if err != nil {
		return nil, fmt.Errorf("querying duplicate child PDFs: %w", err)
	}

	pending := make([]duplicateResolvePending, 0)
	plannedDupes := map[string]struct{}{}
	for _, row := range rows {
		keys, err := duplicateResolveRowKeys(row)
		if err != nil {
			return nil, err
		}
		if len(keys) < 2 {
			continue
		}
		items, err := duplicateResolveItemsForKeys(db, keys, pdfByParent)
		if err != nil {
			return nil, err
		}
		master, ok := duplicateResolveMaster(items, plannedDupes)
		if !ok {
			continue
		}
		for _, item := range items {
			if item.Key == master.Key {
				continue
			}
			if _, seen := plannedDupes[item.Key]; seen {
				continue
			}
			pending = append(pending, duplicateResolvePending{
				id:              "items.duplicates.resolve:" + master.Key + ":" + item.Key,
				masterKey:       master.Key,
				dupKey:          item.Key,
				expectedVersion: item.Version,
				changes:         duplicateResolveChanges(master, item),
			})
			plannedDupes[item.Key] = struct{}{}
			master.Collections, _ = duplicateResolveUnionStrings(master.Collections, item.Collections)
			master.Tags, _ = duplicateResolveUnionTags(master.Tags, item.Tags)
		}
	}

	// One mutation.Op per duplicate pair — unchanged from before, so
	// --max-changes still caps the number of pairs, not sub-writes. Allocated
	// at its final length up front (never appended to below) so the &ops[i]
	// pointers handed to each Apply closure stay stable for the run's whole
	// lifetime.
	ops := make([]mutation.Op, len(pending))
	for i, p := range pending {
		ops[i] = mutation.Op{
			ID:              p.id,
			Key:             p.dupKey,
			Kind:            duplicateResolveMergeKind,
			ExpectedVersion: p.expectedVersion,
			Changes:         p.changes,
			// A merge trashes the duplicate, so it is
			// destructive — require --allow-destructive (matches the capability
			// registry and the --allow-destructive help, which both name "merge").
			Destructive: true,
		}
		masterKey, dupKey, kindSlot := p.masterKey, p.dupKey, &ops[i].Kind
		ops[i].Apply = func() (string, any, error) {
			return applyDuplicateResolve(flags, masterKey, dupKey, kindSlot)
		}
	}
	return ops, nil
}

// duplicateResolveSurfacePartialWarnings makes a half-applied merge visible in
// this run's own output, not just the journal: applyDuplicateResolve reports
// such an op as "applied" (so Summary.Applied > 0 and the run gets journaled),
// so the summary counts alone would otherwise read as a clean success.
// renderMutation always prints env.Warnings, in both --json and human mode,
// before the summary — so this is never silent.
func duplicateResolveSurfacePartialWarnings(env *mutation.Envelope, ops []mutation.Op) {
	if env.Result == nil {
		return
	}
	partial := make(map[string]bool, len(ops))
	for _, op := range ops {
		if op.Kind == duplicateResolveMergePartialKind {
			partial[op.ID] = true
		}
	}
	if len(partial) == 0 {
		return
	}
	for _, item := range env.Result.Items {
		if partial[item.OpID] {
			env.Warnings = append(env.Warnings, fmt.Sprintf("%v", item.Reason))
		}
	}
}

func duplicateResolveRows(db localQueryStore, includeDOI, includeTitle bool) ([]map[string]any, error) {
	if !includeDOI && !includeTitle {
		// Direct callers with no selected matcher should use the same safe DOI-only default as the CLI.
		includeDOI = true
	}
	rows := make([]map[string]any, 0)
	if includeDOI {
		doiRows, err := queryDuplicateDOIs(db)
		if err != nil {
			return nil, err
		}
		rows = append(rows, doiRows...)
	}
	if includeTitle {
		titleRows, err := queryDuplicateTitles(db)
		if err != nil {
			return nil, err
		}
		rows = append(rows, titleRows...)
	}
	return rows, nil
}

func duplicateResolvePDFParents(db localQueryStore) (map[string]bool, error) {
	rows, err := db.QueryRaw(`
SELECT DISTINCT COALESCE(NULLIF(parent_key, ''), json_extract(data, '$.data.parentItem')) AS key
FROM resources
WHERE resource_type = 'items'
	AND COALESCE(json_extract(data, '$.data.itemType'), item_type, '') = 'attachment'
	AND json_extract(data, '$.data.contentType') = 'application/pdf'
	AND COALESCE(NULLIF(parent_key, ''), json_extract(data, '$.data.parentItem'), '') != ''`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		if key, ok := stringValue(normalizeSQLValue(row["key"])); ok && key != "" {
			out[key] = true
		}
	}
	return out, nil
}

func duplicateResolveRowKeys(row map[string]any) ([]string, error) {
	raw := normalizeSQLValue(row["keys"])
	keys := []string{}
	switch v := raw.(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &keys); err != nil {
			return nil, fmt.Errorf("parsing duplicate keys payload %q: %w", v, err)
		}
	case []string:
		keys = append(keys, v...)
	case []any:
		for _, item := range v {
			if key, ok := item.(string); ok && key != "" {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func duplicateResolveItemsForKeys(db localQueryStore, keys []string, pdfByParent map[string]bool) ([]duplicateResolveItem, error) {
	items := make([]duplicateResolveItem, 0, len(keys))
	for _, key := range keys {
		raw, err := db.Get("items", key)
		if err != nil {
			return nil, fmt.Errorf("reading duplicate item %s: %w", key, err)
		}
		if raw == nil {
			return nil, fmt.Errorf("duplicate item %s missing from local store", key)
		}
		item, err := duplicateResolveItemFromRaw(key, raw, pdfByParent[key])
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func duplicateResolveItemFromRaw(fallbackKey string, raw json.RawMessage, hasPDF bool) (duplicateResolveItem, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return duplicateResolveItem{}, fmt.Errorf("parsing duplicate item %s: %w", fallbackKey, err)
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		return duplicateResolveItem{}, fmt.Errorf("duplicate item %s missing data object", fallbackKey)
	}
	key := strings.TrimSpace(jsonStringFieldFromMap(obj, "key"))
	if key == "" {
		key = fallbackKey
	}
	collections, err := itemCollections(raw)
	if err != nil {
		return duplicateResolveItem{}, err
	}
	tags, err := itemDataTags(raw)
	if err != nil {
		return duplicateResolveItem{}, err
	}
	return duplicateResolveItem{
		Key:          key,
		Version:      duplicateResolveVersion(obj),
		Data:         dataObj,
		Collections:  collections,
		Tags:         tags,
		Completeness: duplicateResolveCompleteness(dataObj),
		HasDOI:       strings.TrimSpace(jsonStringFieldFromMap(obj, "DOI")) != "",
		HasPDF:       hasPDF,
		Deleted:      duplicateResolveDeleted(obj),
	}, nil
}

func duplicateResolveMaster(items []duplicateResolveItem, plannedDupes map[string]struct{}) (duplicateResolveItem, bool) {
	candidates := make([]duplicateResolveItem, 0, len(items))
	for _, item := range items {
		if _, planned := plannedDupes[item.Key]; !planned {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return duplicateResolveItem{}, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Completeness != right.Completeness {
			return left.Completeness > right.Completeness
		}
		if left.HasDOI != right.HasDOI {
			return left.HasDOI
		}
		if left.HasPDF != right.HasPDF {
			return left.HasPDF
		}
		return left.Key < right.Key
	})
	return candidates[0], true
}

func duplicateResolveChanges(master, dup duplicateResolveItem) []mutation.Change {
	_, missingCollections := duplicateResolveUnionStrings(master.Collections, dup.Collections)
	_, missingTags := duplicateResolveUnionTags(master.Tags, dup.Tags)
	changes := make([]mutation.Change, 0, 3)
	if len(missingCollections) > 0 {
		changes = append(changes, mutation.Change{Field: "collections", Add: missingCollections})
	}
	if len(missingTags) > 0 {
		changes = append(changes, mutation.Change{Field: "tags", Add: duplicateResolveTagNames(missingTags)})
	}
	if !dup.Deleted {
		changes = append(changes, mutation.Change{Field: "deleted", Add: 1})
	}
	return changes
}

// applyDuplicateResolve performs a merge's two writes: PATCH the merge target
// with the union of collections/tags, then PATCH the duplicate to trash it.
// kindSlot points at this op's own Kind field (see buildDuplicateResolveOps);
// it is left untouched on a clean outcome (success or a failure before any
// write landed) and is set to duplicateResolveMergePartialKind only for the
// half-applied case below.
func applyDuplicateResolve(flags *rootFlags, masterKey, dupKey string, kindSlot *string) (string, any, error) {
	c, err := flags.newWriteClient()
	if err != nil {
		return "failed", err.Error(), err
	}
	masterPath := replacePathParam("/items/{itemKey}", "itemKey", masterKey)
	dupPath := replacePathParam("/items/{itemKey}", "itemKey", dupKey)
	masterRaw, masterVersion, err := c.GetWithVersion(masterPath, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	dupRaw, dupVersion, err := c.GetWithVersion(dupPath, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	master, err := duplicateResolveItemFromRaw(masterKey, masterRaw, false)
	if err != nil {
		return "failed", err.Error(), err
	}
	dup, err := duplicateResolveItemFromRaw(dupKey, dupRaw, false)
	if err != nil {
		return "failed", err.Error(), err
	}

	nextCollections, missingCollections := duplicateResolveUnionStrings(master.Collections, dup.Collections)
	nextTags, missingTags := duplicateResolveUnionTags(master.Tags, dup.Tags)
	if len(missingCollections) == 0 && len(missingTags) == 0 && dup.Deleted {
		return "no_op", "duplicate already merged and trashed", nil
	}

	// targetUpdated tracks whether the merge-target PATCH below actually
	// committed. Once it has, this op has made a real, durable write against
	// masterKey — even if the trash PATCH that follows then fails, that write
	// must never be reported (or journaled) as if nothing happened.
	targetUpdated := false
	if len(missingCollections) > 0 || len(missingTags) > 0 {
		body := map[string]any{}
		if len(missingCollections) > 0 {
			body["collections"] = nextCollections
		}
		if len(missingTags) > 0 {
			body["tags"] = nextTags
		}
		if status, reason, patchErr := duplicateResolvePatch(c, masterPath, masterVersion, body); patchErr != nil {
			return status, reason, patchErr
		}
		targetUpdated = true
	}
	if !dup.Deleted {
		if status, reason, patchErr := duplicateResolvePatch(c, dupPath, dupVersion, map[string]any{"deleted": 1}); patchErr != nil {
			if !targetUpdated {
				// Nothing committed: report the clean failure exactly as before.
				return status, reason, patchErr
			}
			// Half-applied: the merge target already committed, so this run
			// made a real change and must land in the journal (Applied >= 1)
			// even though the duplicate is still live. Reporting "applied"
			// is what makes that happen; the flipped Kind (persisted to the
			// journal) and this reason (surfaced in live output via
			// duplicateResolveSurfacePartialWarnings and in Rows()) are what
			// keep the half-state from being silent. The next resolve run
			// naturally retries just the trash: the union merge is already
			// idempotent, so nothing here is re-applied incorrectly.
			*kindSlot = duplicateResolveMergePartialKind
			return "applied", fmt.Sprintf(
				"merge target %s updated (collections/tags merged), but trashing duplicate %s failed (%s): %v",
				masterKey, dupKey, status, reason,
			), nil
		}
	}
	return "applied", nil, nil
}

func duplicateResolvePatch(c *client.Client, path string, version int, body map[string]any) (string, any, error) {
	headers := map[string]string{}
	if version > 0 {
		headers["If-Unmodified-Since-Version"] = strconv.Itoa(version)
	}
	_, statusCode, err := c.PatchWithHeaders(path, body, headers)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
			return "conflict", apiErr.Body, err
		}
		return "failed", err.Error(), err
	}
	if statusCode < 200 || statusCode >= 300 {
		return "failed", fmt.Sprintf("HTTP %d", statusCode), fmt.Errorf("patch returned HTTP %d", statusCode)
	}
	return "applied", nil, nil
}

func duplicateResolveUnionStrings(base, add []string) ([]string, []string) {
	seen := make(map[string]struct{}, len(base)+len(add))
	out := make([]string, 0, len(base)+len(add))
	for _, value := range base {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	missing := []string{}
	for _, value := range add {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		missing = append(missing, value)
	}
	return out, missing
}

func duplicateResolveUnionTags(base, add []map[string]any) ([]map[string]any, []map[string]any) {
	seen := make(map[string]struct{}, len(base)+len(add))
	out := copyItemTags(base)
	for _, tagObj := range base {
		if tagName, ok := tagObj["tag"].(string); ok && tagName != "" {
			seen[tagName] = struct{}{}
		}
	}
	missing := []map[string]any{}
	for _, tagObj := range add {
		tagName, ok := tagObj["tag"].(string)
		if !ok || tagName == "" {
			continue
		}
		if _, ok := seen[tagName]; ok {
			continue
		}
		seen[tagName] = struct{}{}
		copyObj := copyItemTag(tagObj)
		out = append(out, copyObj)
		missing = append(missing, copyObj)
	}
	return out, missing
}

func duplicateResolveTagNames(tags []map[string]any) []string {
	names := make([]string, 0, len(tags))
	for _, tagObj := range tags {
		if tagName, ok := tagObj["tag"].(string); ok && tagName != "" {
			names = append(names, tagName)
		}
	}
	return names
}

func duplicateResolveCompleteness(data map[string]any) int {
	count := 0
	for _, value := range data {
		if duplicateResolveNonEmpty(value) {
			count++
		}
	}
	return count
}

func duplicateResolveNonEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

func duplicateResolveVersion(obj map[string]any) int {
	for _, value := range []any{obj["version"], obj["data"].(map[string]any)["version"]} {
		switch v := value.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if n, err := strconv.Atoi(v.String()); err == nil {
				return n
			}
		}
	}
	return 0
}

func duplicateResolveDeleted(obj map[string]any) bool {
	if duplicateResolveTruthy(obj["deleted"]) {
		return true
	}
	if dataObj, ok := obj["data"].(map[string]any); ok {
		return duplicateResolveTruthy(dataObj["deleted"])
	}
	return false
}

func duplicateResolveTruthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case int:
		return v != 0
	case string:
		return v == "1" || strings.EqualFold(v, "true")
	default:
		return false
	}
}

func itemsDuplicatesResolveSingleLine(env mutation.Envelope) string {
	if env.Mode == "apply" && env.Result != nil {
		return fmt.Sprintf("resolved %d duplicate(s); %d no-op; %d conflict(s); %d failed", env.Result.Summary.Applied, env.Result.Summary.NoOp, env.Result.Summary.Conflicts, env.Result.Summary.Failed)
	}
	return fmt.Sprintf("would resolve %d duplicate(s)", env.Plan.Summary.Planned)
}
