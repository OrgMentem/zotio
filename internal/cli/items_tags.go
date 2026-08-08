// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newItemsTagsCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "tags [itemKey]",
		Short: "Get or manage tags for a specific item",
		// Keep the legacy bare read as a deprecated alias while exposing read/write subcommands.
		Example: "  zotio items tags list ABC12345",
		// ArbitraryArgs lets the bare-read alias accept an itemKey even when the group is run without its parent (subcommands still match first).
		Args:        cobra.ArbitraryArgs,
		Annotations: map[string]string{"zotio:endpoint": "items.tags", "zotio:method": "GET", "zotio:path": "/items/{itemKey}/tags", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if len(args) > 1 {
				return fmt.Errorf("expected at most one item key")
			}
			fmt.Fprintln(cmd.ErrOrStderr(), `note: "items tags <key>" is deprecated; use "items tags list <key>"`)
			return runItemTagsRead(cmd, flags, args[0])
		},
	}
	// Register explicit read/write tag subcommands under the generated group.
	cmd.AddCommand(newItemsTagsListCmd(flags), newItemsTagsAddCmd(flags), newItemsTagsRemoveCmd(flags))

	return cmd
}

// newItemsTagsListCmd is an explicit read subcommand that reuses the generated tag reader.
func newItemsTagsListCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "list <itemKey>",
		Short:       "Get tags for a specific item",
		Example:     "  zotio items tags list ABC12345",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"zotio:endpoint": "items.tags.list", "zotio:method": "GET", "zotio:path": "/items/{itemKey}/tags", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runItemTagsRead(cmd, flags, args[0])
		},
	}
	return cmd
}

// runItemTagsRead is extracted from the generated command so list and the deprecated alias share one reader.
//
// It reads the item and projects data.tags rather than calling
// /items/<key>/tags: that endpoint exists on the Web API but NOT on the Zotero
// local desktop API, where reads are routed, so it returned 404 for every item.
// Under --data-source local it failed differently, the path being parsed as
// resource "items" with id "tags". Both planes carry the tags inside the item
// payload, so projecting them works everywhere and needs no fallback.
func runItemTagsRead(cmd *cobra.Command, flags *rootFlags, itemKey string) error {
	c, err := flags.newClient()
	if err != nil {
		return err
	}

	path := replacePathParam("/items/{itemKey}", "itemKey", itemKey)
	item, prov, err := resolveRead(cmd.Context(), c, flags, "items", false, path, map[string]string{}, nil)
	if err != nil {
		return classifyAPIError(err, flags)
	}
	data, err := itemTagsFromPayload(item, itemKey)
	if err != nil {
		return err
	}
	// Print provenance to stderr for human-facing output
	printProvenance(cmd, countResultItems(data), prov)
	// For JSON output, wrap with provenance envelope before passing through flags.
	// --select wins over --compact when both are set; --compact only runs when
	// no explicit fields were requested.
	if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
		filtered := data
		if flags.selectFields != "" {
			filtered = filterFields(filtered, flags.selectFields)
		} else if flags.compact {
			filtered = compactFields(filtered)
		}
		wrapped, wrapErr := wrapWithProvenance(filtered, prov)
		if wrapErr != nil {
			return wrapErr
		}
		return printOutput(cmd.OutOrStdout(), wrapped, true)
	}
	// For all other output modes (table, csv, plain, quiet), use the standard pipeline
	if wantsHumanTable(cmd.OutOrStdout(), flags) {
		var items []map[string]any
		if json.Unmarshal(data, &items) == nil && len(items) > 0 {
			if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
				return err
			}
			if len(items) >= 25 {
				fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
			}
			return nil
		}
	}
	return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
}

// itemTagsFromPayload projects an item's tags as a bare array, the shape both
// planes agree on. A single-item read may arrive as the object itself or (from
// the local store path) as a one-element array.
func itemTagsFromPayload(item json.RawMessage, itemKey string) (json.RawMessage, error) {
	var obj map[string]any
	if err := json.Unmarshal(item, &obj); err != nil {
		var arr []map[string]any
		if arrErr := json.Unmarshal(item, &arr); arrErr != nil || len(arr) == 0 {
			return nil, fmt.Errorf("reading tags for %s: unexpected item payload", itemKey)
		}
		obj = arr[0]
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		// Some paths return the item's data fields at the top level.
		dataObj = obj
	}
	tags, ok := dataObj["tags"]
	if !ok || tags == nil {
		// An item with no tags is an empty list, not an error.
		return json.RawMessage("[]"), nil
	}
	encoded, err := json.Marshal(tags)
	if err != nil {
		return nil, fmt.Errorf("encoding tags for %s: %w", itemKey, err)
	}
	return encoded, nil
}
