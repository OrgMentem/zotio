// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written Zotero annotation timeline workflow missing from the generated CLI.

package cli

import (
	"fmt"
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

	cmd := &cobra.Command{
		Use:   "timeline",
		Short: "List annotations sorted by creation date",
		Example: `  zotero-pp-cli annotations timeline --limit 50
  zotero-pp-cli annotations timeline --since 2024-01-01
  zotero-pp-cli annotations timeline --item ABCD1234 --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
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
				items, err := fetchZoteroItems(c, "/collections/"+flagCollection+"/items", nil, 0)
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
					children, err := c.Get("/items/"+key+"/children", map[string]string{"itemType": "annotation"})
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
				items, err := fetchZoteroItems(c, "/items", map[string]string{
					"itemType":  "annotation",
					"sort":      "dateAdded",
					"direction": "desc",
				}, fetchLimitForClientFilteredAnnotations(flagLimit, flagSince, flagItem))
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

	return cmd
}

func fetchLimitForClientFilteredAnnotations(limit int, since, item string) int {
	if strings.TrimSpace(since) == "" && strings.TrimSpace(item) == "" {
		return limit
	}
	if limit <= 0 {
		return 0
	}
	if limit < 100 {
		return 100
	}
	return limit
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
