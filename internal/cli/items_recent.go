// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written recent-items workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newItemsRecentCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagDays int
	var flagType string

	cmd := &cobra.Command{
		Use:         "recent",
		Short:       "List recently added items",
		Annotations: map[string]string{"pp:endpoint": "items.recent", "pp:method": "GET", "pp:path": "/items", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items"
			params := map[string]string{"sort": "dateAdded", "direction": "desc"}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "items", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			data, err = filterRecentItems(data, flagLimit, flagDays, flagType)
			if err != nil {
				return err
			}
			{
				var items []json.RawMessage
				_ = json.Unmarshal(data, &items)
				printProvenance(cmd, len(items), prov)
			}
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
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of items to return")
	cmd.Flags().IntVar(&flagDays, "days", 0, "Return only items added in the last N days")
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by item type")

	return cmd
}

func filterRecentItems(data json.RawMessage, limit, days int, itemType string) (json.RawMessage, error) {
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parsing items response: %w", err)
	}
	filtered := make([]map[string]any, 0, len(items))
	var cutoff time.Time
	if days > 0 {
		cutoff = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	}
	for _, item := range items {
		if itemType != "" && jsonStringFieldFromMap(item, "itemType") != itemType {
			continue
		}
		if days > 0 {
			added, err := time.Parse(time.RFC3339, jsonStringFieldFromMap(item, "dateAdded"))
			if err != nil || added.Before(cutoff) {
				continue
			}
		}
		filtered = append(filtered, item)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	out, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	return out, nil
}
