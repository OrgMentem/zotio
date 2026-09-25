// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

func newAnnotationsSearchCmd(flags *rootFlags) *cobra.Command {
	var flagColor string
	var flagLimit int
	// prefer the local store unless --refresh.
	var refresh bool

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search annotations by text",
		Long: `Search annotation text, comments and tags.

With the local store (the default), matching uses the full-text index:
word stems match ("trust" finds "trusted"), "quoted phrases" match
exactly, and AND, OR, NOT and parentheses combine terms. Results are
ranked by relevance and include the key and title of the item the
annotation belongs to. --refresh searches live through the Zotero API.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			query := strings.Join(args, " ")

			if refresh && flags.dataSource == "local" {
				return usageErr(fmt.Errorf("--refresh cannot be used with --data-source local"))
			}

			// Search the local annotation store only when local data was requested.
			// A completed local store with no matches is a valid empty result.
			useLocal := flags.dataSource == "local" || (flags.dataSource == "auto" && !refresh)
			if useLocal {
				db, err := openStoreForRead(cmd.Context(), "zotio")
				if err != nil || db == nil {
					if flags.dataSource == "local" {
						if err != nil {
							return fmt.Errorf("opening local database: %w\nRun 'zotio sync' first.", err)
						}
						return fmt.Errorf("no local data. Run 'zotio sync' first")
					}
				} else {
					defer db.Close()
					hits, lerr := db.SearchAnnotationsContext(cmd.Context(), store.AnnotationSearch{
						Query:  query,
						Colors: annotationColorSpellings(flagColor),
						Limit:  flagLimit,
					})
					if lerr != nil {
						if flags.dataSource == "local" {
							return fmt.Errorf("querying local annotations: %w", lerr)
						}
					} else {
						results := make([]annotationSummary, 0, len(hits))
						for _, hit := range hits {
							var obj map[string]any
							if json.Unmarshal(hit.Data, &obj) != nil {
								continue
							}
							summaries := annotationSummariesFromItems([]map[string]any{obj})
							if len(summaries) == 0 {
								continue
							}
							summary := summaries[0]
							summary.ItemKey = hit.ItemKey
							summary.ItemTitle = hit.ItemTitle
							results = append(results, summary)
						}
						prov := localProvenance(db, "annotations", "local_only")
						return printCommandJSONEnvelope(cmd.OutOrStdout(), results, flags, prov)
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
			return printCommandJSONEnvelope(cmd.OutOrStdout(), filtered, flags, DataProvenance{Source: "live", ResourceType: "annotations"})
		},
	}
	cmd.Flags().StringVar(&flagColor, "color", "", "Filter by annotation color (yellow, red, green, blue, purple, orange)")
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of annotations to return")
	// bypass the local store and fetch live.
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Fetch live from the API instead of the local store")

	return cmd
}

// annotationColorSpellings returns every stored spelling that one requested
// color may take: Zotero stores hex, users type names or hex.
func annotationColorSpellings(requested string) []string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return nil
	}
	if hex := annotationColorHex(requested); hex != requested {
		return []string{requested, hex}
	}
	return []string{requested}
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
