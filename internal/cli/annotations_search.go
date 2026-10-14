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
	// bypass the mirror fallback and search live only.
	var refresh bool

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search annotations by text",
		Long: `Search annotation text, comments and tags.

By default the search uses the local store's full-text index: word stems match
("trust" finds "trusted"), "quoted phrases" match exactly, AND, OR, NOT and
parentheses combine terms, and results are ranked by relevance. Live search has
none of that, which is why this command reads the local store first, unlike
annotations timeline and annotations export, which read live first. With no
synced local store the search runs live instead: each word or "quoted phrase"
must appear, in any letter case, in the annotation's text, comment or tags;
stems and operators do not apply. meta.source and meta.reason say which path
answered.

--data-source local searches the index only. --data-source live (or --refresh)
searches live only.

Results include the key and title of the top-level item each annotation
belongs to. --scope names top-level items, as it does for annotations export
and annotations timeline.`,
		Example: `  zotio annotations search "working memory"
  zotio annotations search trust --scope collection:ABCD2345 --color yellow
  zotio annotations search "effect size" --data-source local --limit 20`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")

			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			if refresh && flags.dataSource == "local" {
				return usageErr(fmt.Errorf("--refresh cannot be used with --data-source local"))
			}
			spec, err := parseScopeSpec(flagScope)
			if err != nil {
				return err
			}
			if err := validateScopeKeys(spec); err != nil {
				return err
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

			// searchLocal searches the mirror's full-text index. With
			// autoFallback it reports served=false instead of failing when
			// there is no synced mirror, so auto mode can search live.
			searchLocal := func(reason string, autoFallback bool) (bool, error) {
				db, derr := openStoreForRead(cmd.Context(), "zotio")
				if derr != nil {
					return true, fmt.Errorf("opening local database: %w", derr)
				}
				if db == nil {
					if autoFallback {
						return false, nil
					}
					return true, errUnsyncedStoreFor(cmd, flags)
				}
				defer db.Close()
				if autoFallback {
					if herr := requireLocalResourceHydrated(cmd.Context(), db, "items"); herr != nil {
						return false, nil
					}
				}
				parentKeys, serr := resolveCohort(db)
				if serr != nil {
					return true, serr
				}
				hits, lerr := db.SearchAnnotationsContext(cmd.Context(), store.AnnotationSearch{
					Query:      query,
					Colors:     annotationColorSpellings(flagColor),
					Limit:      flagLimit,
					ParentKeys: parentKeys,
				})
				if lerr != nil {
					return true, fmt.Errorf("querying local annotations: %w", lerr)
				}
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
				prov := localProvenance(db, "annotations", reason)
				noteEmptyLocalAnnotations(cmd, prov, len(results))
				return true, printCommandJSONEnvelope(cmd.OutOrStdout(), results, flags, prov)
			}

			// searchLive searches through the Zotero API. It returns the raw
			// failure so auto mode can tell an unreachable Zotero from a
			// rejected request.
			searchLive := func(reason string, withOwners bool) error {
				// The live API has no grand-parent filter, so a scoped live
				// search resolves the cohort from the mirror and filters
				// client-side.
				var cohort map[string]bool
				if scoped {
					db, oerr := openStoreForRead(cmd.Context(), "zotio")
					if oerr != nil {
						return fmt.Errorf("--scope %s needs the local store: %w", flagScope, oerr)
					}
					if db == nil {
						return errUnsyncedStoreFor(cmd, flags)
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

				c, cerr := flags.newClient()
				if cerr != nil {
					return cerr
				}
				// Zotero's default quick-search mode (titleCreatorYear) never
				// looks at annotation text, so search every field and keep only
				// the annotations whose own text, comment or tags match:
				// "everything" also returns annotations whose attachment's full
				// text matches. --scope, --color and the own-field match all
				// filter client-side before --limit, so fetch every hit; a
				// capped fetch would drop matches that sit past the cap.
				items, ferr := fetchZoteroItems(c, "/items", map[string]string{
					"itemType": "annotation",
					"q":        query,
					"qmode":    "everything",
				}, 0)
				if ferr != nil {
					return ferr
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
					var perr error
					attachmentParent, perr = fetchAttachmentParents(c, annotations)
					if perr != nil {
						return perr
					}
				}
				filtered := make([]annotationSummary, 0, len(annotations))
				for _, annotation := range annotations {
					if scoped {
						owner := attachmentParent[annotation.ParentItem]
						if !cohort[owner] {
							continue
						}
						annotation.ItemKey = owner
					}
					if flagColor != "" && !annotationColorMatches(annotation.Color, flagColor) {
						continue
					}
					filtered = append(filtered, annotation)
					if flagLimit > 0 && len(filtered) >= flagLimit {
						break
					}
				}
				if withOwners {
					// Fill item_key and item_title from the shared owner lookup,
					// so a search served live carries the same fields as one
					// served from the index.
					rows := make([]annotationRow, 0, len(filtered))
					for _, annotation := range filtered {
						rows = append(rows, annotationRow{Summary: annotation})
					}
					liveFlags := *flags
					liveFlags.dataSource = "live"
					var oerr error
					if rows, oerr = newAnnotationReader(cmd, flags, &liveFlags, c).attachOwners(cmd.Context(), rows); oerr != nil {
						return oerr
					}
					for i, row := range rows {
						filtered[i] = row.Summary
					}
				}
				return printCommandJSONEnvelope(cmd.OutOrStdout(), filtered, flags, DataProvenance{Source: "live", Reason: reason, ResourceType: "annotations"})
			}

			switch {
			case flags.dataSource == "local":
				_, err := searchLocal("user_requested", false)
				return err
			case flags.dataSource == "live" || refresh:
				if err := searchLive("user_requested", false); err != nil {
					return classifyAPIError(err, flags)
				}
				return nil
			}
			// Auto: the local index first, because its stemming, Boolean
			// operators, and ranking have no live equivalent. Live serves the
			// search only when there is no synced mirror to search.
			served, err := searchLocal("local_first", true)
			if served || err != nil {
				return err
			}
			if err := searchLive("no_local_store", true); err != nil {
				return classifyAPIError(err, flags)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&flagColor, "color", "", "Filter by annotation color (yellow, red, green, blue, purple, orange)")
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of annotations to return (0 = no limit)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Search live only, skipping the local index (same as --data-source live)")
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
