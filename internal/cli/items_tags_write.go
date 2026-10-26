// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"zotio/internal/client"
	"zotio/internal/cliutil"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

func newItemsTagsAddCmd(flags *rootFlags) *cobra.Command {
	var tagNames []string
	var keysFrom string
	var automatic bool
	var batch bool

	cmd := &cobra.Command{
		Use:   "add --tag <tag> [itemKeys...]",
		Short: "Add one or more tags to items",
		Annotations: map[string]string{
			"mcp:read-only": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItemsTagsMutation(cmd, flags, "items.tags.add", "tag_add", tagNames, keysFrom, args, true, automatic, batch)
		},
	}
	cmd.Flags().StringArrayVar(&tagNames, "tag", nil, "Tag to add (repeatable)")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	cmd.Flags().BoolVar(&automatic, "automatic", false, "Write added tags as Zotero automatic tags (type 1)")
	cmd.Flags().BoolVar(&batch, "batch", false, batchTagFlagHelp)
	return cmd
}

func newItemsTagsRemoveCmd(flags *rootFlags) *cobra.Command {
	var tagNames []string
	var keysFrom string
	var automaticOnly bool
	var batch bool

	cmd := &cobra.Command{
		Use:   "remove --tag <tag> [itemKeys...]",
		Short: "Remove one or more tags from items",
		Annotations: map[string]string{
			"mcp:read-only": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItemsTagsMutation(cmd, flags, "items.tags.remove", "tag_remove", tagNames, keysFrom, args, false, automaticOnly, batch)
		},
	}
	cmd.Flags().StringArrayVar(&tagNames, "tag", nil, "Tag to remove (repeatable)")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	cmd.Flags().BoolVar(&automaticOnly, "automatic-only", false, "Remove only matching Zotero automatic tags (type 1)")
	cmd.Flags().BoolVar(&batch, "batch", false, batchTagFlagHelp)
	return cmd
}

// batchTagFlagHelp states the contract change up front: the speedup is real
// and so is the loss of fail-fast.
const batchTagFlagHelp = "Write up to 50 items per request instead of one request per item. " +
	"Much faster over large key sets, but every item in a request reaches Zotero together, " +
	"so the run always continues past a failure: --max-failures and --continue-on-error=false " +
	"are rejected rather than ignored. Each item still reports its own status and its own conflict."

func runItemsTagsMutation(cmd *cobra.Command, flags *rootFlags, operation, kind string, rawTags []string, keysFrom string, args []string, add, automatic, batch bool) error {
	tagNames, err := normalizeTagNames(rawTags)
	if err != nil {
		return err
	}
	keys, err := resolveKeys(args, keysFrom, cmd.InOrStdin())
	if err != nil {
		return usageErr(err)
	}
	return runTagsMutationForKeys(cmd, flags, operation, kind, tagNames, keys, add, automatic, batch)
}

// runTagsMutationForKeys plans and applies one tag add/remove op per key. It is
// the shared builder behind `items tags add|remove`, `tags delete` and
// `tags prune`, so preview, --yes, --max-changes, --batch, journaling and
// mirror write-through cannot drift between them. tagNames is already
// normalised.
func runTagsMutationForKeys(cmd *cobra.Command, flags *rootFlags, operation, kind string, tagNames, keys []string, add, automatic, batch bool) error {
	var err error
	// Checked before the write route is resolved, so a preview cannot succeed
	// for a flag set the apply refuses, and the refusal cannot be pre-empted by
	// a route or auth error.
	if batch {
		if flags.maxFailures > 0 {
			return usageErr(fmt.Errorf("--max-failures cannot be honoured with --batch: up to %d items travel in one request, so a failure cannot stop the items sent alongside it; drop one of the two flags", zoteroBatchWriteMax))
		}
		if cmd.Flags().Changed("continue-on-error") && !flags.continueOnError {
			return usageErr(fmt.Errorf("--continue-on-error=false cannot be honoured with --batch: up to %d items travel in one request, so the run cannot stop before the items already sent; drop one of the two flags", zoteroBatchWriteMax))
		}
	}
	// Preview and apply run the same reads: each item is read through the write
	// plane so the plan reports real no-ops and a missing key exits 3 in both.
	// The client resolves the Web route without persisting or announcing it
	// unless this run applies.
	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}
	// One list request per 50 keys replaces a GET per item, and the list is
	// never served from the response cache: a stale version would turn every
	// write into a 412.
	c.NoCache = true
	snapshots, err := fetchItemSnapshots(c, keys, flags)
	if err != nil {
		return err
	}
	ops := make([]mutation.Op, 0, len(keys))
	batchObjects := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		path := replacePathParam("/items/{itemKey}", "itemKey", key)
		snapshot := snapshots[key]
		version := snapshot.version
		currentTags, err := itemDataTags(snapshot.data)
		if err != nil {
			return err
		}

		changes := tagMutationChanges(currentTags, tagNames, add, automatic)
		keyCopy := key
		pathCopy := path
		op := mutation.Op{
			ID:              operation + ":" + keyCopy,
			Key:             keyCopy,
			Kind:            kind,
			ExpectedVersion: version,
			Changes:         changes,
			Destructive:     false,
		}
		if add && len(changes) == 0 {
			op.NoOpReason = map[string]any{"tag_types": observedTagTypes(currentTags, tagNames)}
		}
		// The write sends the tag array computed from the planned read and
		// carries that read's version as its precondition, so a concurrent edit
		// loses the precondition and is reported as that item's conflict.
		nextTags := nextTagsForAdd(currentTags, tagNames, automatic)
		if !add {
			nextTags = nextTagsForRemove(currentTags, tagNames, automatic)
		}
		switch {
		case len(changes) == 0:
			// The engine never calls Apply for a no-op op, so an item with
			// nothing to change must not consume a batch slot either.
		case nextTags == nil:
			op.Changes = nil
		case batch:
			if version <= 0 {
				// Same fail-closed rule as patchWithVersionGuard, and the
				// same blast radius: the item fails, the run continues. An
				// unversioned item must not discard every planning read
				// already paid for.
				op.Apply = func() (string, any, error) {
					err := fmt.Errorf("no write-plane version for %s; refusing to write without a version precondition", pathCopy)
					return "failed", err.Error(), err
				}
				ops = append(ops, op)
				continue
			}
			batchObjects = append(batchObjects, map[string]any{
				"key":     keyCopy,
				"version": version,
				"tags":    nextTags,
			})
		default:
			op.Apply = func() (string, any, error) {
				return patchItemTags(c, pathCopy, version, nextTags)
			}
		}
		ops = append(ops, op)
	}

	var updater *batchItemUpdater
	runOptions := []func(*mutation.Options){}
	if batch {
		updater = newBatchItemUpdater(c, "/items", operation, batchObjects)
		slot := 0
		for i := range ops {
			// An op that already has an Apply is the unversioned-item
			// refusal above; it holds no batch slot.
			if len(ops[i].Changes) == 0 || ops[i].Apply != nil {
				continue
			}
			index := slot
			slot++
			ops[i].Apply = func() (string, any, error) {
				return updater.outcome(index)
			}
			ops[i].Dispatched = func() bool { return updater.dispatched(index) }
		}
		// A rejected object travelled to Zotero alongside its batch-mates and
		// cannot un-send them, so stopping early would report applied items as
		// not_attempted — the same lie collections create refuses to tell.
		runOptions = append(runOptions, func(o *mutation.Options) { o.ContinueOnError = true })
	}

	env, runErr := runMutation(cmd.Context(), flags, operation, ops, runOptions...)
	// A request-level failure carries information the engine's generic error
	// cannot express. It must win over runErr, because the same request failure
	// also marks its items failed and therefore always makes runErr non-nil.
	// Per-object rejections never enter updater.Err(), so their own conflict or
	// failed status still uses the engine's exit contract.
	if updater != nil {
		if batchErr := updater.Err(); batchErr != nil {
			// The mutation envelope already reports every item as failed, and
			// renderMutationResult marks the error rendered so no second JSON
			// document follows it.
			if cliutil.ClassifyHTTPError(batchErr.Error()) == cliutil.HTTPErrConflict {
				runErr = apiErr(redactedAPIError(batchErr, batchErr.Error(), liveSecrets(flags), ""))
			} else {
				runErr = classifyAPIError(batchErr, flags)
			}
		}
	}
	return renderMutationResult(cmd, flags, env, runErr, itemTagsSingleLine(add, tagNames))
}

func normalizeTagNames(rawTags []string) ([]string, error) {
	if len(rawTags) == 0 {
		return nil, usageErr(fmt.Errorf("required flag %q not set", "tag"))
	}
	seen := make(map[string]struct{}, len(rawTags))
	tagNames := make([]string, 0, len(rawTags))
	for _, tagName := range rawTags {
		if tagName == "" {
			return nil, usageErr(fmt.Errorf("tag must not be empty"))
		}
		if _, ok := seen[tagName]; ok {
			continue
		}
		seen[tagName] = struct{}{}
		tagNames = append(tagNames, tagName)
	}
	return tagNames, nil
}

func itemDataTags(data json.RawMessage) ([]map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parsing item response: %w", err)
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("item response missing data object")
	}
	rawTags, ok := dataObj["tags"]
	if !ok || rawTags == nil {
		return []map[string]any{}, nil
	}
	tagItems, ok := rawTags.([]any)
	if !ok {
		return nil, fmt.Errorf("item response data.tags is not an array")
	}
	tags := make([]map[string]any, 0, len(tagItems))
	for _, raw := range tagItems {
		tagObj, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item response data.tags contains a non-object entry")
		}
		tags = append(tags, tagObj)
	}
	return tags, nil
}

func tagMutationChanges(currentTags []map[string]any, tagNames []string, add, automatic bool) []mutation.Change {
	changes := make([]mutation.Change, 0, len(tagNames))
	for _, tagName := range tagNames {
		present := itemHasTag(currentTags, tagName)
		if add && !present {
			change := mutation.Change{Field: "tags", Add: tagName}
			if automatic {
				change.TagType = 1
			}
			changes = append(changes, change)
		}
		if !add && present && (!automatic || itemHasTagType(currentTags, tagName, 1)) {
			change := mutation.Change{Field: "tags", Remove: tagName}
			if automatic {
				change.TagType = 1
			}
			changes = append(changes, change)
		}
	}
	return changes
}

func itemHasTag(tags []map[string]any, tagName string) bool {
	for _, tagObj := range tags {
		if currentTag, ok := tagObj["tag"].(string); ok && currentTag == tagName {
			return true
		}
	}
	return false
}

func itemHasTagType(tags []map[string]any, tagName string, tagType int) bool {
	for _, tagObj := range tags {
		if currentTag, ok := tagObj["tag"].(string); ok && currentTag == tagName && itemTagType(tagObj) == tagType {
			return true
		}
	}
	return false
}

func itemTagType(tagObj map[string]any) int {
	switch value := tagObj["type"].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func observedTagTypes(currentTags []map[string]any, tagNames []string) map[string]int {
	types := make(map[string]int, len(tagNames))
	for _, tagName := range tagNames {
		for _, tagObj := range currentTags {
			if currentTag, _ := tagObj["tag"].(string); currentTag == tagName {
				types[tagName] = itemTagType(tagObj)
				break
			}
		}
	}
	return types
}

// nextTagsForAdd returns the tag array with every missing tag appended, or nil
// when the item already carries all of them. Both the per-item and the batched
// path compute the new array with this, so they cannot drift.
func nextTagsForAdd(currentTags []map[string]any, tagNames []string, automatic bool) []map[string]any {
	missing := make([]string, 0, len(tagNames))
	for _, tagName := range tagNames {
		if !itemHasTag(currentTags, tagName) {
			missing = append(missing, tagName)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	nextTags := copyItemTags(currentTags)
	for _, tagName := range missing {
		tag := map[string]any{"tag": tagName}
		if automatic {
			tag["type"] = 1
		}
		nextTags = append(nextTags, tag)
	}
	return nextTags
}

// nextTagsForRemove returns the tag array with every targeted tag dropped, or
// nil when none of them were present.
func nextTagsForRemove(currentTags []map[string]any, tagNames []string, automaticOnly bool) []map[string]any {
	targets := make(map[string]struct{}, len(tagNames))
	for _, tagName := range tagNames {
		targets[tagName] = struct{}{}
	}
	nextTags := make([]map[string]any, 0, len(currentTags))
	removed := 0
	for _, tagObj := range currentTags {
		tagName, _ := tagObj["tag"].(string)
		if _, targeted := targets[tagName]; targeted && (!automaticOnly || itemTagType(tagObj) == 1) {
			removed++
			continue
		}
		nextTags = append(nextTags, copyItemTag(tagObj))
	}
	if removed == 0 {
		return nil
	}
	return nextTags
}

// itemSnapshot is one item as read while planning: its raw JSON (for the tags)
// and the version a write must carry as its precondition.
type itemSnapshot struct {
	data    json.RawMessage
	version int
}

// fetchItemSnapshots reads every key with GET /items?itemKey=K1,K2,... (at most
// zoteroBatchWriteMax keys per request) instead of one GET per item, taking
// each item's version from the object itself. Preview and apply planning share
// it, so both cost ceil(N/50) requests. A key the library does not return is
// absent from the answer; it fails exactly like a single-item 404 (exit 3).
func fetchItemSnapshots(c *client.Client, keys []string, flags *rootFlags) (map[string]itemSnapshot, error) {
	snapshots := make(map[string]itemSnapshot, len(keys))
	for start := 0; start < len(keys); start += zoteroBatchWriteMax {
		end := min(start+zoteroBatchWriteMax, len(keys))
		chunk := keys[start:end]
		body, err := c.Get("/items", map[string]string{
			"itemKey":        strings.Join(chunk, ","),
			"limit":          strconv.Itoa(len(chunk)),
			"includeTrashed": "1",
		})
		if err != nil {
			return nil, classifyAPIError(err, flags)
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, apiErr(fmt.Errorf("parsing item list response: %w", err))
		}
		for _, row := range rows {
			var head struct {
				Key     string `json:"key"`
				Version any    `json:"version"`
				Data    struct {
					Version any `json:"version"`
				} `json:"data"`
			}
			if err := json.Unmarshal(row, &head); err != nil || head.Key == "" {
				continue
			}
			version := snapshotVersion(head.Version)
			if version == 0 {
				version = snapshotVersion(head.Data.Version)
			}
			snapshots[head.Key] = itemSnapshot{data: row, version: version}
		}
		for _, key := range chunk {
			if _, ok := snapshots[key]; !ok {
				return nil, classifyAPIError(fmt.Errorf("GET /items/%s returned HTTP 404: Not found", key), flags)
			}
		}
	}
	return snapshots, nil
}

// snapshotVersion reads a Zotero version that may arrive as a number, a numeric
// string, or the empty string the local API reports for never-pushed items.
func snapshotVersion(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		parsed, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return parsed
	}
	return 0
}

func patchItemTags(c *client.Client, path string, version int, tags []map[string]any) (string, any, error) {
	// Fail closed when the version read returned 0 — sending a key-based PATCH
	// without If-Unmodified-Since-Version would either be rejected with an
	// opaque 428 or, on a permissive server, overwrite a concurrent edit with
	// no conflict detection. The wording matches the established shape in
	// creators_audit_fix.go / tags_rename.go.
	body := map[string]any{"tags": tags}
	status, reason, err := patchWithVersionGuard(c, path, body, version)
	if err != nil {
		return status, reason, err
	}
	// patchWithVersionGuard handles 412/428→conflict mapping and normal error
	// classification internally; preserve the success reason shape.
	return status, reason, nil
}

func copyItemTags(tags []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(tags))
	for _, tagObj := range tags {
		out = append(out, copyItemTag(tagObj))
	}
	return out
}

func copyItemTag(tagObj map[string]any) map[string]any {
	copyObj := make(map[string]any, len(tagObj))
	for key, value := range tagObj {
		copyObj[key] = value
	}
	return copyObj
}

func itemTagsSingleLine(add bool, tagNames []string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		var status string
		if add {
			status = "would add"
		} else {
			status = "would remove"
		}
		if env.Mode == "apply" {
			if add {
				status = "added"
			} else {
				status = "removed"
			}
			if env.Result != nil && len(env.Result.Items) == 1 {
				switch env.Result.Items[0].Status {
				case "no_op":
					if add {
						status = "already present"
					} else {
						status = "already absent"
					}
				case "conflict", "failed", "not_attempted", "skipped":
					status = env.Result.Items[0].Status
				}
			}
		} else if len(env.Plan.Operations) == 1 && len(env.Plan.Operations[0].Changes) == 0 {
			if add {
				status = "already present"
			} else {
				status = "already absent"
			}
		}
		key := "item"
		if len(env.Plan.Operations) == 1 {
			key = env.Plan.Operations[0].Key
		}
		return fmt.Sprintf("%s %s on %s", status, strings.Join(tagNames, ", "), key)
	}
}
