// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/text/unicode/norm"

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
annotation belongs to. --refresh searches live through the Zotero API:
each word or "quoted phrase" must appear, in any letter case, in the
annotation's text, comment or tags; stems and operators do not apply.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			// Zotero's default quick-search mode (titleCreatorYear) never looks
			// at annotation text, so search every field and keep only the
			// annotations whose own text, comment or tags match: "everything"
			// also returns annotations whose attachment's full text matches.
			// --scope, --color and the own-field match all filter client-side
			// before --limit, so fetch every hit; a capped fetch would drop
			// matches that sit past the cap.
			items, err := fetchZoteroItems(c, "/items", map[string]string{
				"itemType": "annotation",
				"q":        query,
				"qmode":    "everything",
			}, 0)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			terms := annotationQueryTerms(query)
			own := items[:0]
			for _, item := range items {
				if annotationMatchesTerms(item, terms) {
					own = append(own, item)
				}
			}
			annotations := annotationSummariesFromItems(own)
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

// fetchAttachmentParents maps each annotation's attachment key to the item
// that owns it, the key a scope names: the attachment's parentItem, or the
// attachment itself when it is a standalone file with no parent.
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
			if parent == "" {
				parent = key
			}
			if key != "" {
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

// annotationQueryTerms splits a query the way Zotero's quick search does:
// "quoted phrases" stay whole and other text splits on whitespace. Terms are
// normalized with zoteroSearchNormalize, as Zotero normalizes search terms.
func annotationQueryTerms(query string) []string {
	var terms []string
	for i, part := range strings.Split(query, `"`) {
		if i%2 == 1 {
			if phrase := zoteroSearchNormalize(strings.TrimSpace(part)); phrase != "" {
				terms = append(terms, phrase)
			}
			continue
		}
		for _, word := range strings.Fields(part) {
			if term := zoteroSearchNormalize(word); term != "" {
				terms = append(terms, term)
			}
		}
	}
	return terms
}

// zoteroSearchFormattingTags is Zotero's _searchFormattingTagRE: the
// rich-text tags its search strips, so literal angle brackets stay matchable.
var zoteroSearchFormattingTags = regexp.MustCompile(`</?(?:i|b|sub|sup)>|<span (?:style="font-variant:small-caps;"|class="nocase")>|</span>`)

// zoteroSearchFold is Zotero's _searchNormalizeMap (letters NFKD leaves
// non-ASCII) followed by its typographic quote and dash folding.
var zoteroSearchFold = strings.NewReplacer(
	"ø", "o", "œ", "oe", "æ", "ae", "ł", "l", "đ", "d", "ð", "d", "þ", "th", "ß", "ss", "ı", "i", "\u2044", "/",
	"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'", "\u2032", "'",
	"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u201f", `"`, "\u2033", `"`,
	"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-", "\u2212", "-",
)

// zoteroSearchNormalize ports Zotero.Utilities.Internal.normalizeForSearch,
// the form Zotero stores for annotation text and comments and applies to
// search terms: strip formatting tags, NFKD, drop combining diacritics,
// lower-case, fold the letters and punctuation above, then recompose (NFC).
// "seance" therefore matches "séance", "fi" matches the "ﬁ" ligature, and a
// straight apostrophe matches a curly one.
// https://github.com/zotero/zotero/blob/main/chrome/content/zotero/xpcom/utilities_internal.js
func zoteroSearchNormalize(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "<") {
		s = zoteroSearchFormattingTags.ReplaceAllString(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if r >= 0x0300 && r <= 0x036f {
			return -1
		}
		return r
	}, norm.NFKD.String(s))
	return norm.NFC.String(zoteroSearchFold.Replace(strings.ToLower(s)))
}

// annotationMatchesTerms reports whether every term occurs in the
// annotation's own text, comment or one of its tags, the annotation fields
// Zotero's quick search matches. Terms come from annotationQueryTerms.
func annotationMatchesTerms(item map[string]any, terms []string) bool {
	fields := []string{
		zoteroSearchNormalize(zoteroString(item, "annotationText")),
		zoteroSearchNormalize(zoteroString(item, "annotationComment")),
	}
	data := zoteroData(item)
	if data == nil {
		data = item
	}
	tags, _ := data["tags"].([]any)
	for _, raw := range tags {
		tag, _ := raw.(map[string]any)
		if name, _ := tag["tag"].(string); name != "" {
			fields = append(fields, zoteroSearchNormalize(name))
		}
	}
	for _, term := range terms {
		found := false
		for _, field := range fields {
			if strings.Contains(field, term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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
