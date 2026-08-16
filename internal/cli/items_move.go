// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"

	"zotio/internal/client"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

func newItemsMoveCmd(flags *rootFlags) *cobra.Command {
	var flagTo string
	var flagFrom string
	var keysFrom string

	cmd := &cobra.Command{
		Use:   "move [itemKey...] [--to <collectionKey>] [--from <collectionKey>]",
		Short: "Add, remove, or move item collection memberships",
		Long: `Add, remove, or move item collection memberships by collection key.

Use --to to add, --from to remove, and both together to move between collections.
Accepts many item keys, or --keys-from to read them from a file or stdin.

To file an item into a collection you know by name rather than by key (creating
the collection when it does not exist), use 'zotio items add-to-collection
<itemKey> --collection-name <name>'. That command resolves the name to a key and
then delegates the membership change here, so guards and idempotency are shared.`,
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
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
	if flags.dryRun {
		ops := make([]mutation.Op, 0, len(keys))
		for _, key := range keys {
			changes := make([]mutation.Change, 0, 2)
			if fromCol != "" {
				changes = append(changes, mutation.Change{Field: "collections", Remove: fromCol})
			}
			if toCol != "" {
				changes = append(changes, mutation.Change{Field: "collections", Add: toCol})
			}
			ops = append(ops, mutation.Op{
				ID:      "items.move:" + key,
				Key:     key,
				Kind:    itemCollectionMutationKind(fromCol, toCol),
				Changes: changes,
			})
		}
		env, runErr := runMutation(cmd.Context(), flags, "items.move", ops)
		if renderErr := renderMutation(cmd, flags, env, itemMoveSingleLine(fromCol, toCol)); renderErr != nil {
			return renderErr
		}
		return runErr
	}

	// Plan all selected item collection changes before apply.
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
			// An op planned with no changes never reaches Apply, so the reason
			// has to travel with the op: a bare {"status":"no_op"} is
			// indistinguishable from a missing item or collection.
			NoOpReason: itemCollectionNoOpReason(fromCol, toCol),
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
	// Apply by re-reading memberships and using a version precondition.
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
	// Fail closed when the version read returned 0 — see patchItemTags for the
	// hazard. Pass through to the shared guard so 412/428 map to "conflict"
	// and the If-Unmodified-Since-Version header is always present on success.
	body := map[string]any{"collections": collections}
	status, reason, err := patchWithVersionGuard(c, path, body, version)
	if err != nil {
		return status, reason, err
	}
	return status, reason, nil
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

// itemCollectionNoOpReason explains why a move changed nothing. The code is the
// stable part agents branch on; the message is the human line.
func itemCollectionNoOpReason(fromCol, toCol string) map[string]any {
	switch {
	case fromCol != "" && toCol != "":
		return map[string]any{
			"code":    "already_moved",
			"message": "collection membership already matches requested move",
			"from":    fromCol,
			"to":      toCol,
		}
	case fromCol != "":
		return map[string]any{
			"code":    "not_in_source_collection",
			"message": "item not in source collection",
			"from":    fromCol,
		}
	default:
		return map[string]any{
			"code":    "already_member",
			"message": "already in target collection",
			"to":      toCol,
		}
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
