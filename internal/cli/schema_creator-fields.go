// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newSchemaCreatorFieldsCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:         "creator-fields",
		Short:       "List all creator fields (firstName, lastName, name)",
		Example:     "  zotio schema creator-fields",
		Annotations: map[string]string{"pp:endpoint": "schema.creator-fields", "pp:method": "GET", "pp:path": "/creatorFields", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			// PATCH: schema endpoints are global; use newSchemaClient (strips library prefix).
			c, err := newSchemaClient(flags)
			if err != nil {
				return err
			}

			path := "/creatorFields"
			params := map[string]string{}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "schema", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Print provenance to stderr for human-facing output
			{
				var countItems []json.RawMessage
				_ = json.Unmarshal(data, &countItems)
				printProvenance(cmd, len(countItems), prov)
			}
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
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
		},
	}

	return cmd
}
