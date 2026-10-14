// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func newItemsChildrenCmd(flags *rootFlags) *cobra.Command {
	var flagItemType string

	cmd := &cobra.Command{
		Use:   "children <itemKey>",
		Short: "Get child items (attachments and notes) for an item",
		// Use an item key placeholder, not a token.
		Example:     "  zotio items children ABCD2345",
		Annotations: map[string]string{"zotio:endpoint": "items.children", "zotio:method": "GET", "zotio:path": "/items/{itemKey}/children", "mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			if err := requireParentItem(cmd.Context(), c, flags, args[0]); err != nil {
				return err
			}
			path := "/items/{itemKey}/children"
			path = replacePathParam(path, "itemKey", args[0])
			params := map[string]string{}
			if flagItemType != "" {
				params["itemType"] = fmt.Sprintf("%v", flagItemType)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "items", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Print provenance to stderr for human-facing output
			printProvenance(cmd, countResultItems(data), prov)
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				if flags.quiet {
					return nil
				}
				filtered, ferr := applyFieldFlags(data, flags)
				if ferr != nil {
					return ferr
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
		},
	}
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Filter by type (attachment or note)")

	return cmd
}

// requireParentItem verifies that an item exists before its children are read.
// The Zotero local API answers /items/<unknown>/children with the whole
// library, so a missing parent must be detected with a separate read (404 ->
// exit 3).
func requireParentItem(ctx context.Context, c *client.Client, flags *rootFlags, itemKey string) error {
	path := replacePathParam("/items/{itemKey}", "itemKey", itemKey)
	if _, _, err := resolveRead(ctx, c, flags, "items", false, path, nil, nil); err != nil {
		return classifyAPIError(err, flags)
	}
	return nil
}
