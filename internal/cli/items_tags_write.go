// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): item tag add/remove mutation commands.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"zotero-pp-cli/internal/client"
)

func newItemsTagsAddCmd(flags *rootFlags) *cobra.Command {
	var tagNames []string
	var keysFrom string

	cmd := &cobra.Command{
		Use:   "add --tag <tag> [itemKeys...]",
		Short: "Add one or more tags to items",
		Annotations: map[string]string{
			"mcp:read-only":                 "false",
			"pp:destructive":                "false",
			"pp:supports-dry-run":           "true",
			"pp:requires-allow-destructive": "false",
			"pp:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItemsTagsMutation(cmd, flags, "items.tags.add", "tag_add", tagNames, keysFrom, args, true)
		},
	}
	cmd.Flags().StringArrayVar(&tagNames, "tag", nil, "Tag to add (repeatable)")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	return cmd
}

func newItemsTagsRemoveCmd(flags *rootFlags) *cobra.Command {
	var tagNames []string
	var keysFrom string

	cmd := &cobra.Command{
		Use:   "remove --tag <tag> [itemKeys...]",
		Short: "Remove one or more tags from items",
		Annotations: map[string]string{
			"mcp:read-only":                 "false",
			"pp:destructive":                "false",
			"pp:supports-dry-run":           "true",
			"pp:requires-allow-destructive": "false",
			"pp:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItemsTagsMutation(cmd, flags, "items.tags.remove", "tag_remove", tagNames, keysFrom, args, false)
		},
	}
	cmd.Flags().StringArrayVar(&tagNames, "tag", nil, "Tag to remove (repeatable)")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")
	return cmd
}

func runItemsTagsMutation(cmd *cobra.Command, flags *rootFlags, operation, kind string, rawTags []string, keysFrom string, args []string, add bool) error {
	tagNames, err := normalizeTagNames(rawTags)
	if err != nil {
		return err
	}
	keys, err := resolveKeys(args, keysFrom, cmd.InOrStdin())
	if err != nil {
		return err
	}

	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}
	ops := make([]plannedOp, 0, len(keys))
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

		changes := tagMutationChanges(currentTags, tagNames, add)
		keyCopy := key
		pathCopy := path
		tagsCopy := append([]string(nil), tagNames...)
		op := plannedOp{
			ID:              operation + ":" + keyCopy,
			Key:             keyCopy,
			Kind:            kind,
			ExpectedVersion: version,
			Changes:         changes,
			Destructive:     false,
		}
		if add {
			op.apply = func() (string, any, error) {
				return applyItemTagAdd(c, pathCopy, tagsCopy)
			}
		} else {
			op.apply = func() (string, any, error) {
				return applyItemTagRemove(c, pathCopy, tagsCopy)
			}
		}
		ops = append(ops, op)
	}

	env, runErr := runMutation(cmd.Context(), flags, operation, ops)
	renderErr := renderMutation(cmd, flags, env, itemTagsSingleLine(add, tagNames))
	if renderErr != nil {
		return renderErr
	}
	return runErr
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

func tagMutationChanges(currentTags []map[string]any, tagNames []string, add bool) []mutationChange {
	changes := make([]mutationChange, 0, len(tagNames))
	for _, tagName := range tagNames {
		present := itemHasTag(currentTags, tagName)
		if add && !present {
			changes = append(changes, mutationChange{Field: "tags", Add: tagName})
		}
		if !add && present {
			changes = append(changes, mutationChange{Field: "tags", Remove: tagName})
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

func applyItemTagAdd(c *client.Client, path string, tagNames []string) (string, any, error) {
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentTags, err := itemDataTags(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	missing := make([]string, 0, len(tagNames))
	for _, tagName := range tagNames {
		if !itemHasTag(currentTags, tagName) {
			missing = append(missing, tagName)
		}
	}
	if len(missing) == 0 {
		return "no_op", "tag already present", nil
	}
	nextTags := copyItemTags(currentTags)
	for _, tagName := range missing {
		nextTags = append(nextTags, map[string]any{"tag": tagName})
	}
	return patchItemTags(c, path, currentVersion, nextTags)
}

func applyItemTagRemove(c *client.Client, path string, tagNames []string) (string, any, error) {
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentTags, err := itemDataTags(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	targets := make(map[string]struct{}, len(tagNames))
	for _, tagName := range tagNames {
		targets[tagName] = struct{}{}
	}
	nextTags := make([]map[string]any, 0, len(currentTags))
	removed := 0
	for _, tagObj := range currentTags {
		tagName, _ := tagObj["tag"].(string)
		if _, ok := targets[tagName]; ok {
			removed++
			continue
		}
		nextTags = append(nextTags, copyItemTag(tagObj))
	}
	if removed == 0 {
		return "no_op", "tag not present", nil
	}
	return patchItemTags(c, path, currentVersion, nextTags)
}

func patchItemTags(c *client.Client, path string, version int, tags []map[string]any) (string, any, error) {
	body := map[string]any{"tags": tags}
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

func itemTagsSingleLine(add bool, tagNames []string) func(mutationEnvelope) string {
	return func(env mutationEnvelope) string {
		status := "would update"
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
