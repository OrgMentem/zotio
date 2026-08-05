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
// The collection is resolved by exact name and created when absent; membership
// is delegated to items move so version guards and idempotency stay identical
// to the established collection mutation path.
//
// Preview mode (no --yes) only reads: it fetches the item and looks up whether
// a same-named collection already exists, then reports what apply would do.
// Apply mode routes an on-demand collection create through the mutation engine
// so it is gated by --max-changes and journaled like every other write, then
// delegates the membership change to items move. Two journal entries for one
// invocation — a collection create plus an item move — is correct: two writes
// really did happen.
func newItemsAddToCollectionCmd(flags *rootFlags) *cobra.Command {
	var collectionName string

	cmd := &cobra.Command{
		Use:   "add-to-collection <itemKey>",
		Short: "Add an item to a named collection, creating it when needed",
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "false",
			"zotio:requires-allow-destructive": "false",
		},
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(collectionName)
			if name == "" {
				return fmt.Errorf("--collection-name is required")
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
			if _, err := itemCollections(itemData); err != nil {
				return err
			}
			collections, err := collectionsByName(c)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			existingKey := collections[name]

			mode := resolveMutationMode(flags)
			if !mode.Apply {
				collectionAction := "create"
				if existingKey != "" {
					collectionAction = "reuse"
				}
				report := map[string]any{
					"action":            "add-to-collection",
					"resource":          "items",
					"key":               args[0],
					"collection_name":   name,
					"collection_action": collectionAction,
					"status":            0,
					"success":           false,
					"dry_run":           true,
					"preview_reason":    mode.PreviewReason,
				}
				if existingKey != "" {
					report["collection_key"] = existingKey
				}
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}

			collectionKey := existingKey
			if collectionKey == "" {
				created, err := createCollectionThroughMutation(cmd, flags, c, name)
				if err != nil {
					return err
				}
				collectionKey = created
			}
			return runItemsMoveMutation(cmd, flags, "", collectionKey, "", args)
		},
	}
	cmd.Flags().StringVar(&collectionName, "collection-name", "", "Exact collection name; create it when absent")
	return cmd
}

// createCollectionThroughMutation creates the on-demand collection through the
// shared mutation engine instead of a bare HTTP POST, so the create is counted
// against --max-changes and journaled like any other write. Kind and Changes
// follow the same "collection_create" / non-string Add convention as
// collections create, so the mirror never mistakes a collection key for an
// item field and reverse.go correctly refuses to invert it.
func createCollectionThroughMutation(cmd *cobra.Command, flags *rootFlags, c *client.Client, name string) (string, error) {
	var (
		createdKey string
		applyErr   error
	)
	ops := []mutation.Op{{
		ID:      "items.add-to-collection:" + name,
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
			return "applied", nil, nil
		},
	}}
	if _, runErr := runMutation(cmd.Context(), flags, "items.add-to-collection", ops); runErr != nil {
		if applyErr != nil {
			return "", applyErr
		}
		return "", runErr
	}
	return createdKey, nil
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
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusPreconditionFailed {
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
