// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"zotio/internal/client"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

// newItemsAddToCollectionCmd adds one item to a named top-level collection.
// The collection is resolved by exact name and created when absent.
//
// Preview and apply run the same reads (the item and the collection list) and
// build the same plan, so a preview predicts the apply: one mutation envelope
// with an optional collection create followed by the item's membership change.
// The create is counted against --max-changes and journaled like every other
// write; the membership op reads the created key when it runs. Two operations
// for one invocation is correct: two writes really happen.
func newItemsAddToCollectionCmd(flags *rootFlags) *cobra.Command {
	var collectionName string

	cmd := &cobra.Command{
		Use:   "add-to-collection <itemKey>",
		Short: "Add an item to a named collection, creating it when needed",
		Long: `Add an item to a top-level collection identified by name, creating the
collection when no collection of that name exists.

Prefer this when you know the collection by name. When you already have the
collection key, or need to move many items at once or remove a membership, use
'zotio items move --to <collectionKey>' (also --from, --keys-from); this command
resolves the name and then applies the same membership change.

The plan holds two operations when the collection has to be created -- a
collection create plus an item membership change -- because two writes happen.`,
		Example: `  zotio items add-to-collection ABCD2345 --collection-name "Reading list"
  zotio items add-to-collection ABCD2345 --collection-name "Reading list" --yes`,
		Annotations: map[string]string{"mcp:read-only": "false"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(collectionName)
			if name == "" {
				return usageErr(fmt.Errorf("--collection-name is required"))
			}
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			itemPath := replacePathParam("/items/{itemKey}", "itemKey", args[0])
			itemData, _, err := c.GetWithVersion(itemPath, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			current, err := itemCollections(itemData)
			if err != nil {
				return err
			}
			collections, err := collectionsByName(c)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			ops, applyErr := addToCollectionOps(c, flags, itemPath, args[0], name, collections[name], current)
			env, runErr := runMutation(cmd.Context(), flags, "items.add-to-collection", ops)
			return renderMutationResult(cmd, flags, env, preferApplyErr(runErr, *applyErr), addToCollectionLine(name))
		},
	}
	cmd.Flags().StringVar(&collectionName, "collection-name", "", "Exact collection name; create it when absent")
	return cmd
}

// addToCollectionOps plans the writes: a collection create when no collection
// of that name exists (existingKey is empty), then the item's membership
// change. The returned error pointer receives the classified error of the write
// that failed.
func addToCollectionOps(c *client.Client, flags *rootFlags, itemPath, itemKey, name, existingKey string, current []string) ([]mutation.Op, *error) {
	var (
		createdKey string
		applyErr   error
	)
	ops := make([]mutation.Op, 0, 2)
	if existingKey == "" {
		// Kind and Changes follow the same "collection_create" / non-string Add
		// convention as collections create, so the mirror never mistakes a
		// collection key for an item field and reverse.go correctly refuses to
		// invert it.
		ops = append(ops, mutation.Op{
			ID:      "items.add-to-collection:create:" + name,
			Key:     name,
			Kind:    "collection_create",
			Changes: []mutation.Change{{Field: "collection", Add: map[string]any{"name": name}}},
			Apply: func() (string, any, error) {
				key, err := createCollectionByName(c, name)
				if err != nil {
					applyErr = classifyAPIError(err, flags)
					return "failed", nil, applyErr
				}
				createdKey = key
				return "applied", map[string]any{"key": key}, nil
			},
		})
	}

	member := mutation.Op{
		ID:   "items.add-to-collection:" + itemKey,
		Key:  itemKey,
		Kind: "collection_add",
		Apply: func() (string, any, error) {
			target := existingKey
			if target == "" {
				target = createdKey
			}
			if target == "" {
				err := fmt.Errorf("collection %q has no key to add the item to", name)
				applyErr = err
				return "failed", err.Error(), err
			}
			status, reason, err := applyItemCollectionMove(c, itemPath, "", target)
			if err != nil {
				applyErr = classifyAPIError(err, flags)
			}
			return status, reason, err
		},
	}
	if existingKey != "" {
		member.Changes = collectionMutationChanges(current, "", existingKey)
		member.NoOpReason = itemCollectionNoOpReason("", existingKey)
	} else {
		// The key does not exist yet: plan the membership by name, then report
		// (Effective) and journal (Revert) the key the create returned.
		member.Changes = []mutation.Change{{Field: "collections", Add: map[string]any{"name": name}}}
		member.Effective = func() []mutation.Change {
			if createdKey == "" {
				return nil
			}
			return []mutation.Change{{Field: "collections", Add: createdKey}}
		}
		member.Revert = func() []mutation.Change {
			if createdKey == "" {
				return nil
			}
			return []mutation.Change{{Field: "collections", Remove: createdKey}}
		}
	}
	ops = append(ops, member)
	return ops, &applyErr
}

// addToCollectionLine words the one-operation case (an existing collection)
// for a terminal; the two-operation plan prints the standard summary.
func addToCollectionLine(name string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		key := ""
		if len(env.Plan.Operations) == 1 {
			key = env.Plan.Operations[0].Key
		}
		if env.Mode == "apply" && env.Result != nil && len(env.Result.Items) == 1 {
			switch status := env.Result.Items[0].Status; status {
			case "applied":
				return fmt.Sprintf("added %s → %s", key, name)
			default:
				return fmt.Sprintf("%s %s → %s", status, key, name)
			}
		}
		if len(env.Plan.Operations) == 1 && len(env.Plan.Operations[0].Changes) == 0 {
			return fmt.Sprintf("no_op %s → %s", key, name)
		}
		return fmt.Sprintf("would add %s → %s", key, name)
	}
}

// createCollectionByName POSTs a new top-level collection and returns its key.
// The caller has already confirmed no same-named collection exists.
func createCollectionByName(c *client.Client, name string) (string, error) {
	created, _, err := c.PostWithHeaders(
		"/collections",
		[]map[string]any{{"name": name}},
		map[string]string{"Zotero-Write-Token": writeToken("zotio.items.add-to-collection", name)},
	)
	if err != nil {
		var apiErr *client.APIError
		if !client.IsAmbiguousWriteError(err) &&
			(!errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusPreconditionFailed) {
			return "", err
		}

		// A precondition failure can mean the client retried after the server
		// accepted the create but lost its response. Reconcile the write token
		// replay against the collection list instead of attempting another POST.
		collections, readErr := collectionsByName(c)
		if readErr != nil {
			return "", readErr
		}
		if key := collections[name]; key != "" {
			return key, nil
		}
		return "", err
	}
	if key := createdCollectionKey(created); key != "" {
		return key, nil
	}

	// Zotero's Web API normally returns successful[index] -> collection key.
	// Re-read for compatible local/proxy responses that omit that envelope.
	collections, err := collectionsByName(c)
	if err != nil {
		return "", err
	}
	if key := collections[name]; key != "" {
		return key, nil
	}
	return "", fmt.Errorf("creating collection %q did not return a collection key", name)
}

func collectionsByName(c *client.Client) (map[string]string, error) {
	type collectionRow struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}

	// Zotero caps collection listings. Read every page before deciding that a
	// same-named collection is absent, otherwise a later-page match can create
	// a duplicate.
	byName := make(map[string][]string)
	for start := 0; ; start += 100 {
		raw, err := c.Get("/collections", map[string]string{"limit": "100", "start": fmt.Sprintf("%d", start)})
		if err != nil {
			return nil, err
		}
		var rows []collectionRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("decoding collections: %w", err)
		}
		for _, row := range rows {
			name := strings.TrimSpace(row.Data.Name)
			if name == "" {
				name = strings.TrimSpace(row.Name)
			}
			key := strings.TrimSpace(row.Key)
			if name != "" && key != "" {
				byName[name] = append(byName[name], key)
			}
		}
		if len(rows) < 100 {
			break
		}
	}
	result := make(map[string]string, len(byName))
	for collectionName, keys := range byName {
		sort.Strings(keys)
		result[collectionName] = keys[0]
	}
	return result, nil
}

func createdCollectionKey(raw json.RawMessage) string {
	var response struct {
		// Zotero array-write envelope: keys live in "success"; "successful"
		// maps indices to full collection objects.
		Success    map[string]string          `json:"success"`
		Successful map[string]json.RawMessage `json:"successful"`
		Key        string                     `json:"key"`
		Data       struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return ""
	}
	if response.Key != "" {
		return response.Key
	}
	if response.Data.Key != "" {
		return response.Data.Key
	}
	var keys []string
	for _, key := range response.Success {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	for _, rawObj := range response.Successful {
		var obj struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(rawObj, &obj) == nil && strings.TrimSpace(obj.Key) != "" {
			keys = append(keys, strings.TrimSpace(obj.Key))
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}
