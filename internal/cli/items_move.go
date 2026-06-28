// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): support item collection add/remove/move mutations.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"
	"zotero-pp-cli/internal/client"
	"zotero-pp-cli/internal/mutation"
)

func newItemsMoveCmd(flags *rootFlags) *cobra.Command {
	var flagTo string
	var flagFrom string
	var keysFrom string

	cmd := &cobra.Command{
		Use:   "move [itemKey...] [--to <collectionKey>] [--from <collectionKey>]",
		Short: "Add, remove, or move item collection memberships",
		Annotations: map[string]string{
			"mcp:read-only":                 "false",
			"pp:destructive":                "false",
			"pp:supports-dry-run":           "true",
			"pp:requires-allow-destructive": "false",
			"pp:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItemsMoveMutation(cmd, flags, flagFrom, flagTo, keysFrom, args)
		},
	}
	cmd.Flags().StringVar(&flagTo, "to", "", "Collection key to add item into")
	cmd.Flags().StringVar(&flagFrom, "from", "", "Collection key to remove item from")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Read item keys from a file, '-' for stdin, or positional args when omitted")

	return cmd
}

func runItemsMoveMutation(cmd *cobra.Command, flags *rootFlags, fromCol, toCol, keysFrom string, args []string) error {
	if fromCol == "" && toCol == "" {
		return fmt.Errorf("at least one of --to or --from is required")
	}
	keys, err := resolveKeys(args, keysFrom, cmd.InOrStdin())
	if err != nil {
		return err
	}

	// PATCH(glean write-safety): plan all selected item collection changes before apply.
	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}
	ops := make([]mutation.Op, 0, len(keys))
	for _, key := range keys {
		path := replacePathParam("/items/{itemKey}", "itemKey", key)
		data, version, err := c.GetWithVersion(path, nil)
		if err != nil {
			return classifyAPIError(err, flags)
		}
		collections, err := itemCollections(data)
		if err != nil {
			return err
		}

		keyCopy := key
		pathCopy := path
		fromCopy := fromCol
		toCopy := toCol
		op := mutation.Op{
			ID:              "items.move:" + keyCopy,
			Key:             keyCopy,
			Kind:            itemCollectionMutationKind(fromCol, toCol),
			ExpectedVersion: version,
			Changes:         collectionMutationChanges(collections, fromCol, toCol),
			Destructive:     false,
			Apply: func() (string, any, error) {
				return applyItemCollectionMove(c, pathCopy, fromCopy, toCopy)
			},
		}
		ops = append(ops, op)
	}

	env, runErr := runMutation(cmd.Context(), flags, "items.move", ops)
	renderErr := renderMutation(cmd, flags, env, itemMoveSingleLine(fromCol, toCol))
	if renderErr != nil {
		return renderErr
	}
	return runErr
}

func itemCollectionMutationKind(fromCol, toCol string) string {
	if fromCol != "" && toCol != "" {
		return "collection_move"
	}
	if fromCol != "" {
		return "collection_remove"
	}
	return "collection_add"
}

func collectionMutationChanges(current []string, fromCol, toCol string) []mutation.Change {
	next, removed, added := nextItemCollections(current, fromCol, toCol)
	if sameStringSlice(current, next) {
		return nil
	}
	changes := make([]mutation.Change, 0, 2)
	if removed {
		changes = append(changes, mutation.Change{Field: "collections", Remove: fromCol})
	}
	if added {
		changes = append(changes, mutation.Change{Field: "collections", Add: toCol})
	}
	return changes
}

func applyItemCollectionMove(c *client.Client, path, fromCol, toCol string) (string, any, error) {
	// PATCH(glean write-safety): apply re-reads memberships and patches with version precondition.
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentCollections, err := itemCollections(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	nextCollections, _, _ := nextItemCollections(currentCollections, fromCol, toCol)
	if sameStringSlice(currentCollections, nextCollections) {
		return "no_op", itemCollectionNoOpReason(fromCol, toCol), nil
	}
	return patchItemCollections(c, path, currentVersion, nextCollections)
}

func patchItemCollections(c *client.Client, path string, version int, collections []string) (string, any, error) {
	body := map[string]any{"collections": collections}
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

func nextItemCollections(current []string, fromCol, toCol string) ([]string, bool, bool) {
	if fromCol != "" && fromCol == toCol {
		return append([]string(nil), current...), false, false
	}
	next := make([]string, 0, len(current)+1)
	removed := false
	for _, collection := range current {
		if fromCol != "" && collection == fromCol {
			removed = true
			continue
		}
		next = append(next, collection)
	}

	added := false
	if toCol != "" && !stringSliceContains(next, toCol) {
		next = append(next, toCol)
		added = true
	}
	return next, removed, added
}

func stringSliceContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func itemCollectionNoOpReason(fromCol, toCol string) string {
	switch {
	case fromCol != "" && toCol != "":
		return "collection membership already matches requested move"
	case fromCol != "":
		return "item not in source collection"
	default:
		return "already in target collection"
	}
}

func itemMoveSingleLine(fromCol, toCol string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		status := "would move"
		if fromCol != "" && toCol == "" {
			status = "would remove"
		}
		if env.Mode == "apply" {
			status = "moved"
			if fromCol != "" && toCol == "" {
				status = "removed"
			}
			if env.Result != nil && len(env.Result.Items) == 1 {
				switch env.Result.Items[0].Status {
				case "no_op":
					return itemMoveNoOpLine(env, fromCol, toCol)
				case "conflict", "failed", "not_attempted", "skipped":
					status = env.Result.Items[0].Status
				}
			}
		} else if len(env.Plan.Operations) == 1 && len(env.Plan.Operations[0].Changes) == 0 {
			return itemMoveNoOpLine(env, fromCol, toCol)
		}

		key := "item"
		if len(env.Plan.Operations) == 1 {
			key = env.Plan.Operations[0].Key
		}
		if fromCol != "" && toCol != "" {
			return fmt.Sprintf("%s %s: %s → %s", status, key, fromCol, toCol)
		}
		if fromCol != "" {
			return fmt.Sprintf("%s %s from %s", status, key, fromCol)
		}
		return fmt.Sprintf("%s %s → %s", status, key, toCol)
	}
}

func itemMoveNoOpLine(env mutation.Envelope, fromCol, toCol string) string {
	key := "item"
	if len(env.Plan.Operations) == 1 {
		key = env.Plan.Operations[0].Key
	}
	if fromCol != "" && toCol != "" {
		return fmt.Sprintf("%s already matches %s → %s", key, fromCol, toCol)
	}
	if fromCol != "" {
		return fmt.Sprintf("%s already absent from %s", key, fromCol)
	}
	return fmt.Sprintf("%s already in %s", key, toCol)
}

func itemCollections(data json.RawMessage) ([]string, error) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parsing item response: %w", err)
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("item response missing data object")
	}
	rawCollections, ok := dataObj["collections"].([]any)
	if !ok {
		return []string{}, nil
	}
	collections := make([]string, 0, len(rawCollections))
	for _, raw := range rawCollections {
		if collection, ok := raw.(string); ok && collection != "" {
			collections = append(collections, collection)
		}
	}
	return collections, nil
}
