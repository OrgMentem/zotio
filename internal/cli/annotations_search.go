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
	var flagScope string
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
			spec, err := parseScopeSpec(flagScope)
			if err != nil {
				return usageErr(err)
			}
			scoped := spec.Type != "library"
			// resolveCohort maps --scope to parent item keys. nil means the
			// whole library; an empty non-nil slice is an empty cohort.
			resolveCohort := func(db *store.Store) ([]string, error) {
				if !scoped {
					return nil, nil
				}
				result, rerr := resolveScopeLive(cmd.Context(), flags, localQueryStore{Store: db}, spec)
				if rerr != nil {
					return nil, rerr
				}
				if result.Precondition != "" {
					return nil, scopePreconditionErr(cmd.Context(), cmd.OutOrStdout(), flags, commandRegistryPath(cmd), result)
				}
				if result.Keys == nil {
					return []string{}, nil
				}
				return result.Keys, nil
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
					parentKeys, serr := resolveCohort(db)
					if serr != nil {
						return serr
					}
					hits, lerr := db.SearchAnnotationsContext(cmd.Context(), store.AnnotationSearch{
						Query:      query,
						Colors:     annotationColorSpellings(flagColor),
						Limit:      flagLimit,
						ParentKeys: parentKeys,
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

			// The live API has no grand-parent filter, so a scoped live search
			// resolves the cohort from the mirror and filters client-side.
			var cohort map[string]bool
			if scoped {
				db, oerr := openStoreForRead(cmd.Context(), "zotio")
				if oerr != nil {
					return fmt.Errorf("--scope %s needs the local store: %w\nRun 'zotio sync' first.", flagScope, oerr)
				}
				if db == nil {
					return fmt.Errorf("--scope %s needs the local store. Run 'zotio sync' first", flagScope)
				}
				keys, serr := resolveCohort(db)
				db.Close()
				if serr != nil {
					return serr
				}
				cohort = make(map[string]bool, len(keys))
				for _, key := range keys {
					cohort[key] = true
				}
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			fetchLimit := fetchLimitForAnnotationSearch(flagLimit, flagColor)
			if scoped {
				// The limit applies after the scope filter, so fetch every
				// match; a capped fetch would drop in-scope hits.
				fetchLimit = 0
			}
			items, err := fetchZoteroItems(c, "/items", map[string]string{
				"itemType": "annotation",
				"q":        query,
			}, fetchLimit)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			annotations := annotationSummariesFromItems(items)
			var attachmentParent map[string]string
			if scoped {
				attachmentParent, err = fetchAttachmentParents(c, annotations)
				if err != nil {
					return classifyAPIError(err, flags)
				}
			}
			filtered := make([]annotationSummary, 0, len(annotations))
			for _, annotation := range annotations {
				if scoped && !cohort[attachmentParent[annotation.ParentItem]] {
					continue
				}
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
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultLibrary, scopeFlagUsageDefaultLibrary)

	return cmd
}

// annotationAttachmentChunk is how many attachment keys go into one itemKey
// request; the Zotero API caps itemKey at 50 keys.
const annotationAttachmentChunk = 50

// fetchAttachmentParents maps each annotation's attachment key to that
// attachment's own parentItem, the bibliographic item a scope names.
func fetchAttachmentParents(c zoteroGetter, annotations []annotationSummary) (map[string]string, error) {
	seen := make(map[string]bool, len(annotations))
	keys := make([]string, 0, len(annotations))
	for _, annotation := range annotations {
		if annotation.ParentItem == "" || seen[annotation.ParentItem] {
			continue
		}
		seen[annotation.ParentItem] = true
		keys = append(keys, annotation.ParentItem)
	}
	parents := make(map[string]string, len(keys))
	for start := 0; start < len(keys); start += annotationAttachmentChunk {
		end := min(start+annotationAttachmentChunk, len(keys))
		items, err := fetchZoteroItems(c, "/items", map[string]string{
			"itemKey": strings.Join(keys[start:end], ","),
		}, 0)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			data, _ := item["data"].(map[string]any)
			if data == nil {
				data = item
			}
			key, _ := data["key"].(string)
			parent, _ := data["parentItem"].(string)
			if key != "" && parent != "" {
				parents[key] = parent
			}
		}
	}
	return parents, nil
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
