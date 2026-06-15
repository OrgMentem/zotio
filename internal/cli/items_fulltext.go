// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written PDF full-text retrieval workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsFulltextCmd(flags *rootFlags) *cobra.Command {
	var flagSearch string

	cmd := &cobra.Command{
		Use:         "fulltext <itemKey>",
		Short:       "Get full text from an item's PDF attachment",
		Annotations: map[string]string{"pp:endpoint": "items.fulltext", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			itemKey := args[0]
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

	return cmd
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
