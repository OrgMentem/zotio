// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"
)

func newAnnotationsSearchCmd(flags *rootFlags) *cobra.Command {
	var flagColor string
	var flagLimit int
	// prefer the local store unless --refresh.
	var refresh bool

	cmd := &cobra.Command{
		Use:         "search <query>",
		Short:       "Search annotations by text",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			query := strings.Join(args, " ")

			// search the local annotation store when
			// present; --refresh forces the live API path below. The API `q`
			// param has no local equivalent, so text matching runs in memory.
			if !refresh {
				if db, _ := openStoreForRead(cmd.Context(), "zotio"); db != nil {
					defer db.Close()
					rows, lerr := db.ItemsByType("annotation", 0)
					if lerr == nil && len(rows) > 0 {
						items := make([]map[string]any, 0, len(rows))
						for _, raw := range rows {
							var obj map[string]any
							if json.Unmarshal(raw, &obj) == nil {
								items = append(items, obj)
							}
						}
						annotations := annotationSummariesFromItems(items)
						filtered := filterAnnotationSummaries(annotations, query, flagColor, flagLimit)
						return printCommandJSON(cmd.OutOrStdout(), filtered, flags)
					}
				}
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
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
	// bypass the local store and fetch live.
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Fetch live from the API instead of the local store")

	return cmd
}

// filterAnnotationSummaries applies an in-memory text + color + limit filter,
// used for local-store annotation search where the API `q` parameter is not
// available. An empty query matches everything.
func filterAnnotationSummaries(annotations []annotationSummary, query, color string, limit int) []annotationSummary {
	q := strings.ToLower(strings.TrimSpace(query))
	filtered := make([]annotationSummary, 0, len(annotations))
	for _, a := range annotations {
		if color != "" && !annotationColorMatches(a.Color, color) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(a.Text), q) && !strings.Contains(strings.ToLower(a.Comment), q) {
			continue
		}
		filtered = append(filtered, a)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
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
