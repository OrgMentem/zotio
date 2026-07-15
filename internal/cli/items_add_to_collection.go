// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

// newItemsAddToCollectionCmd adds one item to a named top-level collection.
// The collection is resolved by exact name and created when absent; membership
// is delegated to items move so version guards and idempotency stay identical
// to the established collection mutation path.
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
			if !resolveMutationMode(flags).Apply {
				return fmt.Errorf("items add-to-collection creates a collection on demand; pass --yes to apply")
			}
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			collectionKey, err := findOrCreateCollectionByName(c, name)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return runItemsMoveMutation(cmd, flags, "", collectionKey, "", args)
		},
	}
	cmd.Flags().StringVar(&collectionName, "collection-name", "", "Exact collection name; create it when absent")
	return cmd
}

func findOrCreateCollectionByName(c *client.Client, name string) (string, error) {
	collections, err := collectionsByName(c)
	if err != nil {
		return "", err
	}
	if key := collections[name]; key != "" {
		return key, nil
	}

	created, _, err := c.Post("/collections", map[string]any{"name": name})
	if err != nil {
		return "", err
	}
	if key := createdCollectionKey(created); key != "" {
		return key, nil
	}

	// Zotero's Web API normally returns successful[index] -> collection key.
	// Re-read for compatible local/proxy responses that omit that envelope.
	collections, err = collectionsByName(c)
	if err != nil {
		return "", err
	}
	if key := collections[name]; key != "" {
		return key, nil
	}
	return "", fmt.Errorf("creating collection %q did not return a collection key", name)
}

func collectionsByName(c *client.Client) (map[string]string, error) {
	raw, err := c.Get("/collections", nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decoding collections: %w", err)
	}

	// A duplicate name is possible in Zotero. Choose the lexicographically first
	// key so repeated filings remain deterministic instead of creating another.
	byName := make(map[string][]string, len(rows))
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
	result := make(map[string]string, len(byName))
	for collectionName, keys := range byName {
		sort.Strings(keys)
		result[collectionName] = keys[0]
	}
	return result, nil
}

func createdCollectionKey(raw json.RawMessage) string {
	var response struct {
		Successful map[string]string `json:"successful"`
		Key        string            `json:"key"`
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
	if len(response.Successful) == 0 {
		return ""
	}
	keys := make([]string, 0, len(response.Successful))
	for _, key := range response.Successful {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}
