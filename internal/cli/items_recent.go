// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

func newItemsRecentCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagDays int
	var flagType string

	cmd := &cobra.Command{
		Use:         "recent",
		Short:       "List recently added items",
		Annotations: map[string]string{"zotio:endpoint": "items.recent", "zotio:method": "GET", "zotio:path": "/items", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			data, prov, err := fetchRecentItems(cmd.Context(), c, flags, flagLimit, flagDays, flagType)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			printProvenance(cmd, countResultItems(data), prov)
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

func fetchRecentItems(ctx context.Context, c *client.Client, flags *rootFlags, limit, days int, itemType string) (json.RawMessage, DataProvenance, error) {
	const pageSize = 100

	params := map[string]string{
		"sort":      "dateAdded",
		"direction": "desc",
	}
	if itemType != "" {
		params["itemType"] = itemType
	}
	var cutoff time.Time
	if days > 0 {
		cutoff = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	}

	filtered := make([]map[string]any, 0)
	var provenance DataProvenance
	for start := 0; ; {
		pageParams := cloneStringMap(params)
		pageParams["start"] = strconv.Itoa(start)
		pageParams["limit"] = strconv.Itoa(pageSize)
		data, prov, err := resolveRead(ctx, c, flags, "items", false, "/items", pageParams, nil)
		if err != nil {
			return nil, DataProvenance{}, err
		}
		if start == 0 {
			provenance = prov
		}
		var items []map[string]any
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, DataProvenance{}, fmt.Errorf("parsing items response: %w", err)
		}
		for _, item := range items {
			if itemType != "" && jsonStringFieldFromMap(item, "itemType") != itemType {
				continue
			}
			if days > 0 {
				added, err := time.Parse(time.RFC3339, jsonStringFieldFromMap(item, "dateAdded"))
				if err != nil {
					continue
				}
				if added.Before(cutoff) {
					out, err := json.Marshal(filtered)
					return out, provenance, err
				}
			}
			filtered = append(filtered, item)
			if limit > 0 && len(filtered) >= limit {
				out, err := json.Marshal(filtered)
				return out, provenance, err
			}
		}
		if len(items) < pageSize {
			break
		}
		start += len(items)
	}
	out, err := json.Marshal(filtered)
	return out, provenance, err
}
