// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written Zotero annotation text search workflow missing from the generated CLI.

package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

func newAnnotationsSearchCmd(flags *rootFlags) *cobra.Command {
	var flagColor string
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "search <query>",
		Short:       "Search annotations by text",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			query := strings.Join(args, " ")
			items, err := fetchZoteroItems(c, "/items", map[string]string{
				"itemType": "annotation",
				"q":        query,
			}, fetchLimitForAnnotationSearch(flagLimit, flagColor))
			if err != nil {
				return classifyAPIError(err, flags)
			}
			annotations := annotationSummariesFromItems(items)
			filtered := make([]annotationSummary, 0, len(annotations))
			for _, annotation := range annotations {
				if flagColor != "" && !annotationColorMatches(annotation.Color, flagColor) {
					continue
				}
				filtered = append(filtered, annotation)
				if flagLimit > 0 && len(filtered) >= flagLimit {
					break
				}
			}
			return printCommandJSON(cmd.OutOrStdout(), filtered, flags)
		},
	}
	cmd.Flags().StringVar(&flagColor, "color", "", "Filter by annotation color (yellow, red, green, blue, purple, orange)")
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of annotations to return")

	return cmd
}

func fetchLimitForAnnotationSearch(limit int, color string) int {
	if strings.TrimSpace(color) == "" {
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

func annotationColorMatches(actual, requested string) bool {
	actual = strings.ToLower(strings.TrimSpace(actual))
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return true
	}
	if actual == requested {
		return true
	}
	return actual == annotationColorHex(requested)
}

func annotationColorHex(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "yellow":
		return "#ffd400"
	case "red":
		return "#ff6666"
	case "green":
		return "#5fb236"
	case "blue":
		return "#2ea8e5"
	case "purple":
		return "#a28ae5"
	case "orange":
		return "#f19837"
	default:
		return name
	}
}
