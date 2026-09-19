// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"zotio/internal/client"
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
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
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
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
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
		return err
	}
	// Checked before the dry-run branch and before the write route is
	// resolved, so a preview cannot succeed for a flag set the apply refuses,
	// and the refusal cannot be pre-empted by a route or auth error.
	if batch {
		if flags.maxFailures > 0 {
			return fmt.Errorf("--max-failures cannot be honoured with --batch: up to %d items travel in one request, so a failure cannot stop the items sent alongside it; drop one of the two flags", zoteroBatchWriteMax)
		}
		if cmd.Flags().Changed("continue-on-error") && !flags.continueOnError {
			return fmt.Errorf("--continue-on-error=false cannot be honoured with --batch: up to %d items travel in one request, so the run cannot stop before the items already sent; drop one of the two flags", zoteroBatchWriteMax)
		}
	}
	if flags.dryRun {
		ops := make([]mutation.Op, 0, len(keys))
		for _, key := range keys {
			changes := make([]mutation.Change, 0, len(tagNames))
			for _, tagName := range tagNames {
				change := mutation.Change{Field: "tags"}
				if add {
					change.Add = tagName
					if automatic {
						change.TagType = 1
					}
				} else {
					change.Remove = tagName
					if automatic {
						change.TagType = 1
					}
				}
				changes = append(changes, change)
			}
			ops = append(ops, mutation.Op{
				ID:      operation + ":" + key,
				Key:     key,
				Kind:    kind,
				Changes: changes,
			})
		}
		env, runErr := runMutation(cmd.Context(), flags, operation, ops)
		if renderErr := renderMutation(cmd, flags, env, itemTagsSingleLine(add, tagNames)); renderErr != nil {
			return renderErr
		}
		return runErr
	}

	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}
	ops := make([]mutation.Op, 0, len(keys))
	batchObjects := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		path := replacePathParam("/items/{itemKey}", "itemKey", key)
		data, version, err := c.GetWithVersion(path, nil)
		if err != nil {
			return classifyAPIError(err, flags)
		}
		currentTags, err := itemDataTags(data)
		if err != nil {
			return err
		}

		changes := tagMutationChanges(currentTags, tagNames, add, automatic)
		keyCopy := key
		pathCopy := path
		tagsCopy := append([]string(nil), tagNames...)
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
		switch {
		case batch:
			// The engine never calls Apply for a no-op op, so an item with
			// nothing to change must not consume a batch slot either.
			if len(changes) == 0 {
				break
			}
			nextTags := nextTagsForAdd(currentTags, tagNames, automatic)
			if !add {
				nextTags = nextTagsForRemove(currentTags, tagNames, automatic)
			}
			if nextTags == nil {
				op.Changes = nil
				break
			}
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
		case add:
			op.Apply = func() (string, any, error) {
				return applyItemTagAdd(c, pathCopy, tagsCopy, automatic)
			}
		default:
			op.Apply = func() (string, any, error) {
				return applyItemTagRemove(c, pathCopy, tagsCopy, automatic)
			}
		}
		ops = append(ops, op)
	}

	var updater *batchItemUpdater
	runOptions := []func(*mutation.Options){}
	opObjectIndex := make(map[int]int, len(batchObjects))
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
			opObjectIndex[i] = index
			ops[i].Apply = func() (string, any, error) {
				return updater.outcome(index)
			}
		}
		// A rejected object travelled to Zotero alongside its batch-mates and
		// cannot un-send them, so stopping early would report applied items as
		// not_attempted — the same lie collections create refuses to tell.
		runOptions = append(runOptions, func(o *mutation.Options) { o.ContinueOnError = true })
	}

	env, runErr := runMutation(cmd.Context(), flags, operation, ops, runOptions...)
	if updater != nil {
		correctDispatchedNotAttempted(&env, ops, opObjectIndex, updater)
	}
	renderErr := renderMutation(cmd, flags, env, itemTagsSingleLine(add, tagNames))
	if renderErr != nil {
		return renderErr
	}
	// A request-level failure carries information the engine's generic error
	// cannot express. It must win over runErr, because the same request failure
	// also marks its items failed and therefore always makes runErr non-nil.
	// Per-object rejections never enter updater.Err(), so their own conflict or
	// failed status still uses the engine's exit contract.
	if updater != nil {
		if batchErr := updater.Err(); batchErr != nil {
			return classifyAPIError(batchErr, flags)
		}
	}
	if runErr != nil {
		return runErr
	}
	return nil
}

// correctDispatchedNotAttempted rewrites the engine's not_attempted verdict for
// any item whose request had already been sent.
//
// The engine stops between operations when the context is cancelled, but in
// batch mode the first op of a chunk dispatches all 50 objects. Everything
// after the cancellation point in that chunk already reached Zotero and may
// well have been applied, so reporting it as never attempted is false and
// invites the duplicating retry --batch exists to avoid.
func correctDispatchedNotAttempted(env *mutation.Envelope, ops []mutation.Op, opObjectIndex map[int]int, updater *batchItemUpdater) {
	if env.Result == nil {
		return
	}
	byOpID := make(map[string]int, len(ops))
	for i := range ops {
		byOpID[ops[i].ID] = i
	}
	for i := range env.Result.Items {
		item := &env.Result.Items[i]
		if item.Status != "not_attempted" {
			continue
		}
		opIndex, ok := byOpID[item.OpID]
		if !ok {
			continue
		}
		objectIndex, ok := opObjectIndex[opIndex]
		if !ok || !updater.dispatched(objectIndex) {
			continue
		}
		item.Status = "failed"
		item.Reason = "the request carrying this item was already sent when the run stopped; its outcome is unknown, so verify before retrying"
		env.Result.Summary.NotAttempted--
		env.Result.Summary.Failed++
		env.OK = false
	}
}

func normalizeTagNames(rawTags []string) ([]string, error) {
	if len(rawTags) == 0 {
		return nil, fmt.Errorf("required flag %q not set", "tag")
	}
	seen := make(map[string]struct{}, len(rawTags))
	tagNames := make([]string, 0, len(rawTags))
	for _, tagName := range rawTags {
		if tagName == "" {
			return nil, fmt.Errorf("tag must not be empty")
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

func applyItemTagAdd(c *client.Client, path string, tagNames []string, automatic bool) (string, any, error) {
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentTags, err := itemDataTags(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	nextTags := nextTagsForAdd(currentTags, tagNames, automatic)
	if nextTags == nil {
		return "no_op", map[string]any{"tag_types": observedTagTypes(currentTags, tagNames)}, nil
	}
	return patchItemTags(c, path, currentVersion, nextTags)
}

func applyItemTagRemove(c *client.Client, path string, tagNames []string, automaticOnly bool) (string, any, error) {
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentTags, err := itemDataTags(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	nextTags := nextTagsForRemove(currentTags, tagNames, automaticOnly)
	if nextTags == nil {
		return "no_op", "tag not present", nil
	}
	return patchItemTags(c, path, currentVersion, nextTags)
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
