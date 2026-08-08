// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newItemsTrashCmd(flags *rootFlags) *cobra.Command {
	var flagLimit, flagStart int

	cmd := &cobra.Command{
		Use:         "trash",
		Short:       "List items in the trash",
		Example:     "  zotio items trash",
		Annotations: map[string]string{"zotio:endpoint": "items.trash", "zotio:method": "GET", "zotio:path": "/items/trash", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items/trash"
			params := map[string]string{}
			if flagLimit != 0 {
				params["limit"] = fmt.Sprintf("%v", flagLimit)
			}
			if flagStart != 0 {
				params["start"] = fmt.Sprintf("%v", flagStart)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "items-trash", true, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// The read plane does not learn that an item was trashed until Zotero
			// syncs the write down from zotero.org, and it returned an empty trash
			// for items the web plane already reported as deleted. So immediately
			// after `items delete`, this command could not show what was just
			// trashed — while the mirror, normally the less current source, could.
			// Union the two: live catches items trashed in the Zotero UI, the mirror
			// catches zotio's own trashes that have not propagated yet.
			if flags.dataSource != "local" {
				data, prov = unionMirroredTrash(cmd.Context(), cmd, flags, data, prov)
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
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset (zero-based)")

	return cmd
}

// unionMirroredTrash appends items the local mirror lists as trashed but the read
// plane has not reported yet, de-duplicated by key.
//
// Neither source alone is right: the read plane (the Zotero desktop local API)
// only learns about a trash once Zotero syncs it down from zotero.org, and the
// mirror only knows what the last sync or a write-through recorded. The union is
// the honest answer, and it self-heals as the read plane catches up.
//
// Best-effort: an unreadable mirror leaves the live result untouched.
func unionMirroredTrash(ctx context.Context, cmd *cobra.Command, flags *rootFlags, live json.RawMessage, prov DataProvenance) (json.RawMessage, DataProvenance) {
	var liveItems []json.RawMessage
	if err := json.Unmarshal(live, &liveItems); err != nil {
		// Not a list (an error envelope, say); nothing to union.
		return live, prov
	}
	seen := make(map[string]bool, len(liveItems))
	for _, entry := range liveItems {
		if key := jsonStringField(entry, "key"); key != "" {
			seen[key] = true
		}
	}

	mirrored, _, err := resolveLocal(ctx, "items-trash", true, "/items/trash", map[string]string{}, "trash_reconciliation")
	if err != nil {
		return live, prov
	}
	var mirroredItems []json.RawMessage
	if err := json.Unmarshal(mirrored, &mirroredItems); err != nil {
		return live, prov
	}

	added := 0
	for _, entry := range mirroredItems {
		key := jsonStringField(entry, "key")
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		liveItems = append(liveItems, entry)
		added++
	}
	if added == 0 {
		return live, prov
	}
	merged, err := json.Marshal(liveItems)
	if err != nil {
		return live, prov
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"note: %d trashed item(s) came from the local mirror; the Zotero read API has not caught up with them yet\n", added)
	prov.Source = "live+local"
	return merged, prov
}
