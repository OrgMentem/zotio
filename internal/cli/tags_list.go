// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newTagsListCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagStart int
	var flagQ string

	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List all tags in the library",
		Example:     "  zotio tags list",
		Annotations: map[string]string{"zotio:endpoint": "tags.list", "zotio:method": "GET", "zotio:path": "/tags", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/tags"
			params := map[string]string{}
			if flagLimit != 0 {
				params["limit"] = fmt.Sprintf("%v", flagLimit)
			}
			if flagStart != 0 {
				params["start"] = fmt.Sprintf("%v", flagStart)
			}
			if flagQ != "" {
				params["q"] = fmt.Sprintf("%v", flagQ)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "tags", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Honor --limit when the API accepts but ignores ?limit=N.
			data = truncateJSONArray(data, flagLimit)
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
			//
			// Zotero's raw /tags response nests type and numItems under "meta",
			// which --plain/table treat as a structural wrapper and drop (see
			// plainStructuralFields), so a manual and an automatic instance of the
			// same tag name rendered as identical, inexplicable duplicate rows
			// (tags list counted 793 rows for the 786 distinct names tags audit
			// reports — 7 tags exist as both). Promote them to columns for the
			// human paths only; JSON callers already have meta.type.
			data = flattenTagMetaForDisplay(data)
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
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of tags to return")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset")
	cmd.Flags().StringVar(&flagQ, "query", "", "Filter tags by name substring")

	return cmd
}

// flattenTagMetaForDisplay promotes meta.type and meta.numItems to top-level
// fields so the shared plain/table renderer, which drops "meta" as a
// structural wrapper, can still show them as columns. Non-object entries and
// entries with no meta pass through unchanged.
func flattenTagMetaForDisplay(data json.RawMessage) json.RawMessage {
	var tags []map[string]any
	if err := json.Unmarshal(data, &tags); err != nil {
		return data
	}
	changed := false
	for _, tag := range tags {
		meta, ok := tag["meta"].(map[string]any)
		if !ok {
			continue
		}
		if tagType, ok := meta["type"]; ok {
			tag["type"] = tagType
			changed = true
		}
		if numItems, ok := meta["numItems"]; ok {
			tag["num_items"] = numItems
			changed = true
		}
	}
	if !changed {
		return data
	}
	flattened, err := json.Marshal(tags)
	if err != nil {
		return data
	}
	return flattened
}
