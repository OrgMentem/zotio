// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newSchemaItemTypeCreatorTypesCmd(flags *rootFlags) *cobra.Command {
	var flagItemType string

	cmd := &cobra.Command{
		Use:         "item-type-creator-types",
		Short:       "List valid creator types for an item type",
		Example:     "  zotio schema item-type-creator-types --item-type journalArticle",
		Annotations: map[string]string{"zotio:endpoint": "schema.item-type-creator-types", "zotio:method": "GET", "zotio:path": "/itemTypeCreatorTypes", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// --dry-run has no effect on this read; the flag is validated first.
			if !cmd.Flags().Changed("item-type") || strings.TrimSpace(flagItemType) == "" {
				return usageErr(fmt.Errorf("required flag \"item-type\" not set"))
			}
			// schema endpoints are global; use newSchemaClient (strips library prefix).
			c, err := newSchemaClient(flags)
			if err != nil {
				return err
			}

			path := "/itemTypeCreatorTypes"
			params := map[string]string{}
			if flagItemType != "" {
				params["itemType"] = fmt.Sprintf("%v", flagItemType)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "schema", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Print provenance to stderr for human-facing output
			printProvenance(cmd, countResultItems(data), prov)
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered, err := applyFieldFlags(data, flags)
				if err != nil {
					return err
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
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Item type (e.g. journalArticle)")

	return cmd
}
