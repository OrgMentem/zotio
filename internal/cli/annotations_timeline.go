// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newAnnotationsTimelineCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagSince string
	var flagItem string
	var flagCollection string
	// prefer the local store unless --refresh.
	var refresh bool

	cmd := &cobra.Command{
		Use:   "timeline",
		Short: "List annotations sorted by creation date",
		Example: `  zotio annotations timeline --limit 50
  zotio annotations timeline --since 2024-01-01
  zotio annotations timeline --item ABCD1234 --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if refresh && flags.dataSource == "local" {
				return usageErr(fmt.Errorf("--refresh cannot be used with --data-source local"))
			}
			readFlags := flags
			if refresh {
				override := *flags
				override.dataSource = "live"
				readFlags = &override
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			since, hasSince, err := parseAnnotationSince(flagSince)
			if err != nil {
				return err
			}

			var annotations []annotationSummary
			if flagCollection != "" {
				path := "/collections/" + url.PathEscape(flagCollection) + "/items"
				items, err := fetchResolvedZoteroItems(cmd.Context(), c, readFlags, path, nil, 0)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				for _, item := range items {
					if !zoteroItemHasChildren(item) {
						continue
					}
					key := zoteroString(item, "key")
					if key == "" {
						continue
					}
					childPath := "/items/" + url.PathEscape(key) + "/children"
					children, _, err := resolveRead(cmd.Context(), c, readFlags, "items", false, childPath, map[string]string{"itemType": "annotation"}, nil)
					if err != nil {
						return classifyAPIError(err, flags)
					}
					childItems, err := decodeZoteroItems(children)
					if err != nil {
						return fmt.Errorf("parsing annotation children for %s: %w", key, err)
					}
					annotations = append(annotations, annotationSummariesFromItems(childItems)...)
				}
			} else {
				// The requested limit is applied after client-side --item/--since
				// filtering below, so page the full set when a filter is active
				// rather than capping the fetch at the first page.
				maxItems := flagLimit
				if flagItem != "" || hasSince {
					maxItems = 0
				}
				items, err := fetchResolvedZoteroItems(cmd.Context(), c, readFlags, "/items", map[string]string{
					"itemType":  "annotation",
					"sort":      "dateAdded",
					"direction": "desc",
				}, maxItems)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				annotations = annotationSummariesFromItems(items)
			}

			filtered := make([]annotationSummary, 0, len(annotations))
			for _, annotation := range annotations {
				if flagItem != "" && annotation.ParentItem != flagItem {
					continue
				}
				if hasSince {
					added, err := parseZoteroTime(annotation.DateAdded)
					if err != nil || !added.After(since) {
						continue
					}
				}
				filtered = append(filtered, annotation)
			}
			sort.Slice(filtered, func(i, j int) bool {
				return filtered[i].DateAdded > filtered[j].DateAdded
			})
			if flagLimit > 0 && len(filtered) > flagLimit {
				filtered = filtered[:flagLimit]
			}
			return printCommandJSON(cmd.OutOrStdout(), filtered, flags)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of annotations to return")
	cmd.Flags().StringVar(&flagSince, "since", "", "ISO date string; include annotations after this date")
	cmd.Flags().StringVar(&flagItem, "item", "", "Scope to annotations of a specific parent item key")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Scope to items in this collection key")
	// bypass the local store and fetch live.
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Fetch live from the API instead of the local store")

	return cmd
}

func parseAnnotationSince(value string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	parsed, err := parseZoteroTime(value)
	if err != nil {
		return time.Time{}, false, usageErr(fmt.Errorf("invalid --since value %q: expected ISO date or RFC3339 timestamp", value))
	}
	return parsed, true, nil
}

func parseZoteroTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q", value)
}
