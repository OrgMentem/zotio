// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written PDF full-text retrieval workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newItemsFulltextCmd(flags *rootFlags) *cobra.Command {
	var flagSearch string
	// PATCH(glean hhup): prefer locally-synced full text unless --refresh.
	var refresh bool

	cmd := &cobra.Command{
		Use:         "fulltext <itemKey>",
		Short:       "Get full text from an item's PDF attachment",
		Annotations: map[string]string{"pp:endpoint": "items.fulltext", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			itemKey := args[0]

			// PATCH(glean hhup): serve from the local store (sync --fulltext)
			// when present; --refresh forces the live API path below.
			if !refresh {
				if db, _ := openStoreForRead(cmd.Context(), "zotio"); db != nil {
					defer db.Close()
					if data, ok := localPDFFulltext(db, itemKey); ok {
						if flagSearch != "" {
							filtered, ferr := filterFulltextLines(data, flagSearch)
							if ferr != nil {
								return ferr
							}
							data = filtered
						}
						return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
					}
				}
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			childrenPath := "/items/{itemKey}/children"
			childrenPath = replacePathParam(childrenPath, "itemKey", itemKey)
			children, err := c.Get(childrenPath, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			pdfKey, err := findPDFAttachmentKey(children)
			if err != nil {
				return err
			}
			if pdfKey == "" {
				return fmt.Errorf("no PDF attachment found for item %s", itemKey)
			}

			fulltextPath := "/items/{pdfKey}/fulltext"
			fulltextPath = replacePathParam(fulltextPath, "pdfKey", pdfKey)
			data, err := c.Get(fulltextPath, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if flagSearch != "" {
				data, err = filterFulltextLines(data, flagSearch)
				if err != nil {
					return err
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagSearch, "search", "", "Return only full-text lines containing this string")
	// PATCH(glean hhup): bypass the local store and fetch live.
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Fetch live from the API instead of the local store")

	return cmd
}

// localPDFFulltext resolves an item's PDF full text from the local store.
// It first treats itemKey as an attachment key directly; failing that, it
// finds the item's PDF attachment among synced attachments and looks up its
// stored full text. Returns false when nothing is available locally.
// PATCH(glean hhup).
func localPDFFulltext(db *store.Store, itemKey string) (json.RawMessage, bool) {
	if ft, ok, _ := db.Fulltext(itemKey); ok {
		return ft, true
	}
	attachments, err := db.ItemsByType("attachment", 0)
	if err != nil {
		return nil, false
	}
	for _, raw := range attachments {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		if jsonStringFieldFromMap(obj, "parentItem") != itemKey {
			continue
		}
		if jsonStringFieldFromMap(obj, "contentType") != "application/pdf" {
			continue
		}
		key := jsonStringFieldFromMap(obj, "key")
		if key == "" {
			continue
		}
		if ft, ok, _ := db.Fulltext(key); ok {
			return ft, true
		}
	}
	return nil, false
}

func findPDFAttachmentKey(data json.RawMessage) (string, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return "", fmt.Errorf("parsing child items response: %w", err)
	}
	for _, item := range items {
		if jsonStringField(item, "itemType") != "attachment" {
			continue
		}
		if jsonStringField(item, "contentType") != "application/pdf" {
			continue
		}
		if key := jsonStringField(item, "key"); key != "" {
			return key, nil
		}
	}
	return "", nil
}

func filterFulltextLines(data json.RawMessage, query string) (json.RawMessage, error) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parsing fulltext response: %w", err)
	}
	content, ok := obj["content"].(string)
	if !ok {
		return data, nil
	}
	query = strings.ToLower(query)
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), query) {
			filtered = append(filtered, line)
		}
	}
	obj["content"] = strings.Join(filtered, "\n")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}
