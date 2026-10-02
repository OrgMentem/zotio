// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// items summarize assembles a bounded, synthesis-ready context bundle for an item
// or collection. It does not call a model: it gathers the highest-signal local
// context (citation, abstract, the reader's own annotations, a capped fulltext
// excerpt, metadata gaps) plus a synthesis prompt, and lets the host LLM do the
// writing. Reads are local only; nothing is written.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

type summarizeOpts struct {
	maxChars       int
	maxAnnotations int
	noFulltext     bool
	// focus holds the --focus search terms; empty keeps the first-pages
	// excerpt and the page-ordered annotations.
	focus string
}

type summarizeTruncation struct {
	Fulltext         bool `json:"fulltext"`
	Annotations      bool `json:"annotations"`
	AnnotationsKept  int  `json:"annotations_kept,omitempty"`
	AnnotationsTotal int  `json:"annotations_total,omitempty"`
}

type summarizeBundle struct {
	Key      string `json:"key"`
	Citation string `json:"citation"`
	ItemType string `json:"item_type,omitempty"`
	DOI      string `json:"doi,omitempty"`
	URL      string `json:"url,omitempty"`
	Abstract string `json:"abstract,omitempty"`
	// Annotations carry the shared annotationSummary (annotations_export.go),
	// not a private shape. The private one dropped the key, the source item,
	// the date and the colour, so a bundle could not cite the highlight it
	// quoted and disagreed with what annotations export emitted for the
	// same item.
	Annotations []annotationSummary `json:"annotations,omitempty"`
	Fulltext    string              `json:"fulltext_excerpt,omitempty"`
	Focus       *summarizeFocus     `json:"focus,omitempty"`
	Gaps        []string            `json:"gaps,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`
	Truncated   summarizeTruncation `json:"truncated"`
	Prompt      string              `json:"prompt,omitempty"`
}

type summarizeCollectionBundle struct {
	// Scope is the resolved cohort expression. Collection stays populated for a
	// collection cohort under either spelling, so `--scope collection:KEY` and
	// `--collection KEY` emit the same payload. It is omitted when the bundle was
	// not assembled from a scope expression (collections_bundle.go).
	Scope      string            `json:"scope,omitempty"`
	Collection string            `json:"collection"`
	ItemCount  int               `json:"item_count"`
	Items      []summarizeBundle `json:"items"`
	Prompt     string            `json:"prompt"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// Fulltext status values of a --focus section. Each says what the PDF-text
// search could see for the item, so a missing index is never reported as
// "searched, nothing matched".
const (
	focusFulltextMatched    = "matched"     // at least one indexed PDF matched
	focusFulltextNoMatch    = "no_match"    // indexed PDF text was searched; nothing matched
	focusFulltextNotIndexed = "not_indexed" // no synced PDF text under the item; nothing was searched
	focusFulltextSkipped    = "skipped"     // --no-fulltext
	focusFulltextError      = "error"       // the search failed; see warnings
)

// summarizeFocus is the --focus section of one item's bundle: ranked passages
// that match the focus terms, drawn only from this item's indexed PDF text and
// its annotations. It replaces the first-pages excerpt and the page-ordered
// annotation list, which ignore the question being asked.
type summarizeFocus struct {
	Terms    string `json:"terms"`
	Fulltext string `json:"fulltext"`
	// NoHit is true only when every search ran and none matched. A failed
	// search leaves it false and adds a warning instead.
	NoHit    bool                    `json:"no_hit"`
	Passages []summarizeFocusPassage `json:"passages"`
}

// summarizeFocusPassage is one attributable piece of evidence. Annotation
// passages come first in FTS rank order, then PDF-text passages in FTS rank
// order; each PDF-text passage is the best-matching snippet of one attachment.
type summarizeFocusPassage struct {
	Source        string `json:"source"` // "annotation" or "fulltext"
	ItemKey       string `json:"item_key"`
	AttachmentKey string `json:"attachment_key"`
	AnnotationKey string `json:"annotation_key,omitempty"`
	Page          string `json:"page,omitempty"`
	Text          string `json:"text"`
	Comment       string `json:"comment,omitempty"`
}

const itemSynthesisPrompt = "Summarize this work for a literature review: core claim/contribution, method, key findings, and limitations. Ground every point in the abstract, annotations, and excerpt above — do not invent."

func collectionSynthesisPrompt(n int) string {
	return fmt.Sprintf("Synthesize across these %d works (cited by key): shared themes, points of agreement and contradiction, methodological patterns, and open gaps. Ground each claim in the items above and cite the relevant item keys.", n)
}

func newItemsSummarizeCmd(flags *rootFlags) *cobra.Command {
	var (
		flagCollection     string
		flagScope          string
		flagMaxChars       int
		flagMaxAnnotations int
		flagNoFulltext     bool
		flagFocus          string
	)
	cmd := &cobra.Command{
		Use:   "summarize [<itemKey>]",
		Short: "Assemble a bounded, synthesis-ready context bundle for an item or collection",
		Long: `Gather the highest-signal local context for an item (or every item in a
collection) into one bounded bundle — citation, abstract, your annotations, a
capped fulltext excerpt, and known metadata gaps — plus a synthesis prompt.

With --focus TERMS, each item instead carries a "focus" section: the passages
that match TERMS in that item's indexed PDF text (one ranked snippet per
attachment, within --max-chars) and in its annotations (within
--max-annotations), each with its item and attachment key. Only the selected
items are searched, and no full PDF text is loaded. An item with no synced PDF
text says so (fulltext: not_indexed) rather than reporting "no match".

This command never calls a model: it does the assembly and budgeting the host LLM
is bad at, then hands off. Reads are local only. With --agent/--json it emits the
structured bundle; otherwise a readable Markdown brief you can paste into any LLM.`,
		Example: `  zotio items summarize 9UXV5R7L
  zotio items summarize 9UXV5R7L --agent --max-chars 6000
  zotio items summarize --collection MAR7RFQN --no-fulltext
  zotio items summarize --scope tag:to-read --agent
  zotio items summarize --scope collection:MAR7RFQN --focus "transfer learning" --max-chars 2000`,
		// Profile values are applied in PersistentPreRunE. Validate the input
		// and enforce the declared store requirement in RunE so missing input
		// remains a usage error even when no mirror exists.
		Annotations: map[string]string{"mcp:read-only": "true", preflightAnnotationKey: preflightAnnotationSkip},
		Args:        cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && strings.TrimSpace(flagCollection) == "" && strings.TrimSpace(flagScope) == "" {
				return positionalArgUsageErr(cmd, errors.New("missing <itemKey>: pass an item key, --collection <key>, or --scope <spec>"))
			}
			if cmd.Flags().Changed("focus") && strings.TrimSpace(flagFocus) == "" {
				return usageErr(errors.New("--focus needs search terms"))
			}
			effectiveScope, err := reconcileScopeFlags(flagScope, scopeSugarFor("collection", "collection", flagCollection))
			if err != nil {
				return err
			}
			if err := runDeclaredCapabilityPreflight(cmd, flags); err != nil {
				return err
			}
			db, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if db == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer db.Close()
			// database/sql opens SQLite lazily. Force the read-only connection now
			// so a corrupt existing database is reported as an open failure on every
			// path, not as an error from whichever item or cohort query touches it
			// first.
			//
			// An absent row is a successful probe: the point is that the connection
			// opened and the schema is readable, and the empty key never matches.
			if _, err := db.Get("items", ""); err != nil && !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("opening local database: %w", err)
			}

			opts := summarizeOpts{
				maxChars:       flagMaxChars,
				maxAnnotations: flagMaxAnnotations,
				noFulltext:     flagNoFulltext,
				focus:          strings.TrimSpace(flagFocus),
			}

			if effectiveScope != "" {
				sel, serr := resolveScopeSelection(cmd, flags, "items summarize", localQueryStore{db}, effectiveScope)
				if serr != nil {
					return serr
				}
				return runSummarizeCohort(cmd.Context(), cmd, db, sel, opts, flags)
			}
			if flagCollection != "" {
				return runSummarizeCollection(cmd.Context(), cmd, db, flagCollection, opts, flags)
			}
			raw, err := db.Get("items", args[0])
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("item %s not found locally; run 'zotio sync' (or check the key)", args[0])
			}
			if err != nil {
				return fmt.Errorf("reading item: %w", err)
			}

			var warnings []string
			if opts.focus != "" {
				focus, focusWarnings := loadSummarizeFocus(cmd.Context(), db, []string{args[0]}, "item "+args[0], opts)
				bundle := buildFocusBundle(raw, focus[args[0]], opts)
				bundle.Warnings = focusWarnings
				return finishItemSummary(cmd, bundle, flags)
			}
			annByKey, err := db.AnnotationsForItems([]string{args[0]})
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("reading annotations for item %s: %v", args[0], err))
			}
			fulltext := ""
			if !opts.noFulltext {
				fulltext, err = fulltextForItem(cmd.Context(), db, args[0])
				if err != nil {
					warnings = append(warnings, fmt.Sprintf("reading fulltext for item %s: %v", args[0], err))
				}
			}
			bundle := buildItemBundle(raw, annByKey[args[0]], fulltext, opts)
			bundle.Warnings = warnings
			return finishItemSummary(cmd, bundle, flags)
		},
	}
	// --collection predates --scope and stays supported; it lowers to
	// --scope collection:KEY (see reconcileScopeFlags).
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Summarize every item in this collection key")
	cmd.Flags().StringVar(&flagScope, "scope", scopeFlagDefaultUnset, scopeFlagUsageDefaultLibrary)
	cmd.Flags().IntVar(&flagMaxChars, "max-chars", 8000, "Max characters of fulltext per item: the excerpt, or with --focus the matched PDF-text passages")
	cmd.Flags().IntVar(&flagMaxAnnotations, "max-annotations", 40, "Max annotations included per item")
	cmd.Flags().BoolVar(&flagNoFulltext, "no-fulltext", false, "Omit the fulltext excerpt (abstract + annotations only)")
	cmd.Flags().StringVar(&flagFocus, "focus", "", "Select ranked passages matching these terms from each item's indexed PDF text and annotations, instead of the first-pages excerpt")
	return cmd
}

// runSummarizeCollection keeps --collection on its indexed SQL path: the
// collection predicate is pushed into the query rather than resolved to a key
// set first, which matters because this command already scans the attachment
// table once for fulltext.
func runSummarizeCollection(ctx context.Context, cmd *cobra.Command, db *store.Store, collKey string, opts summarizeOpts, flags *rootFlags) error {
	items, err := db.QueryItemsContext(ctx, store.ItemQuery{
		Collection: collKey,
		TopOnly:    true,
		Sort:       "title",
		Direction:  "asc",
	})
	if err != nil {
		return fmt.Errorf("querying collection items: %w", err)
	}
	return summarizeCohort(ctx, cmd, db, scopeSelection{Expr: "collection:" + collKey, Type: "collection", Value: collKey}, items, opts, flags)
}

// runSummarizeCohort is the --scope arm. A cohort is a key set with no SQL
// predicate behind it, so the items are read in the store's own order and then
// narrowed. Re-deriving the ordering in Go would be a second implementation of
// the query planner's ORDER BY, and `--scope collection:KEY` would stop matching
// `--collection KEY` the moment either changed.
func runSummarizeCohort(ctx context.Context, cmd *cobra.Command, db *store.Store, sel scopeSelection, opts summarizeOpts, flags *rootFlags) error {
	items, err := db.QueryItemsContext(ctx, store.ItemQuery{
		TopOnly:   true,
		Sort:      "title",
		Direction: "asc",
	})
	if err != nil {
		return fmt.Errorf("querying scoped items: %w", err)
	}
	scoped := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		if sel.allows(vaultItemMeta(raw).Key) {
			scoped = append(scoped, raw)
		}
	}
	return summarizeCohort(ctx, cmd, db, sel, scoped, opts, flags)
}

// summarizeCohort assembles the multi-item bundle for an already-selected set,
// so both spellings produce the same payload from the same code.
func summarizeCohort(ctx context.Context, cmd *cobra.Command, db *store.Store, sel scopeSelection, items []json.RawMessage, opts summarizeOpts, flags *rootFlags) error {
	keys := make([]string, 0, len(items))
	for _, raw := range items {
		keys = append(keys, vaultItemMeta(raw).Key)
	}
	var warnings []string
	var focus map[string]*summarizeFocusHits
	var annByKey map[string][]json.RawMessage
	var ftByItem map[string]string
	if opts.focus != "" {
		// Focus mode reads only ranked snippets and matching annotations for
		// these keys; it never loads the cohort's full PDF text.
		focus, warnings = loadSummarizeFocus(ctx, db, keys, "scope "+sel.Expr, opts)
	} else {
		var err error
		annByKey, err = db.AnnotationsForItems(keys)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("reading annotations for scope %s: %v", sel.Expr, err))
		}
		// Batch fulltext once (parent item key -> content) so a cohort does not
		// re-scan the attachment table per item.
		if !opts.noFulltext {
			ftByItem, err = fulltextByParentItemWithErr(ctx, db)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("reading fulltext for scope %s: %v", sel.Expr, err))
			}
		}
	}

	cb := summarizeCollectionBundle{Scope: sel.Expr, ItemCount: len(items), Warnings: warnings}
	if sel.Type == "collection" {
		cb.Collection = sel.Value
	}
	for _, raw := range items {
		key := vaultItemMeta(raw).Key
		if opts.focus != "" {
			cb.Items = append(cb.Items, buildFocusBundle(raw, focus[key], opts))
			continue
		}
		cb.Items = append(cb.Items, buildItemBundle(raw, annByKey[key], ftByItem[key], opts))
	}
	cb.Prompt = collectionSynthesisPrompt(len(items))
	return finishCollectionSummary(cmd, cb, flags)
}

func finishItemSummary(cmd *cobra.Command, bundle summarizeBundle, flags *rootFlags) error {
	if flags.asJSON {
		data, err := json.Marshal(bundle)
		if err != nil {
			return err
		}
		if err := printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags); err != nil {
			return err
		}
	} else {
		fmt.Fprint(cmd.OutOrStdout(), renderBundleMarkdown(bundle, 1, true))
	}
	return summarizeWarnings(cmd, "items summarize", bundle.Warnings, flags)
}

func finishCollectionSummary(cmd *cobra.Command, bundle summarizeCollectionBundle, flags *rootFlags) error {
	if flags.asJSON {
		data, err := json.Marshal(bundle)
		if err != nil {
			return err
		}
		if err := printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags); err != nil {
			return err
		}
	} else {
		fmt.Fprint(cmd.OutOrStdout(), renderCollectionMarkdown(bundle))
	}
	return summarizeWarnings(cmd, "items summarize", bundle.Warnings, flags)
}

func summarizeWarnings(cmd *cobra.Command, command string, warnings []string, flags *rootFlags) error {
	if len(warnings) == 0 {
		return nil
	}
	if !flags.asJSON {
		for _, warning := range warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
		}
	}
	return degradedErr(fmt.Errorf("%s: %d warnings; results incomplete", command, len(warnings)))
}

// buildItemBundle assembles the bounded bundle from already-fetched inputs (pure;
// no store access) so it is easy to test and reuse. fulltext is "" when omitted
// or unavailable.
func buildItemBundle(raw json.RawMessage, annRows []json.RawMessage, fulltext string, opts summarizeOpts) summarizeBundle {
	b, meta := newSummarizeBundle(raw)

	anns := annotationSummariesSorted(annRows)
	total := len(anns)
	if opts.maxAnnotations > 0 && total > opts.maxAnnotations {
		anns = anns[:opts.maxAnnotations]
		b.Truncated.Annotations = true
	}
	for _, a := range anns {
		if strings.TrimSpace(a.Text) == "" && strings.TrimSpace(a.Comment) == "" {
			continue
		}
		b.Annotations = append(b.Annotations, a)
	}
	if b.Truncated.Annotations {
		b.Truncated.AnnotationsKept = len(b.Annotations)
		b.Truncated.AnnotationsTotal = total
	}

	fulltext = strings.TrimSpace(fulltext)
	hasFulltext := fulltext != ""
	if hasFulltext {
		excerpt, cut := truncateRunes(fulltext, opts.maxChars)
		b.Fulltext = excerpt
		b.Truncated.Fulltext = cut
	}

	b.Gaps = itemGaps(meta, hasFulltext, opts.noFulltext)
	return b
}

// newSummarizeBundle fills the item identity both bundle shapes share.
func newSummarizeBundle(raw json.RawMessage) (summarizeBundle, vaultMeta) {
	meta := vaultItemMeta(raw)
	return summarizeBundle{
		Key:      meta.Key,
		Citation: summarizeCitation(meta, extractVenue(raw)),
		ItemType: meta.ItemType,
		DOI:      meta.DOI,
		URL:      meta.URL,
		Abstract: meta.Abstract,
		Prompt:   itemSynthesisPrompt,
	}, meta
}

// summarizeFocusHits is what --focus found for one item before budgeting:
// matching annotations and PDF-text snippets, each in FTS rank order.
type summarizeFocusHits struct {
	fulltextStatus string
	annotationsOK  bool
	annotations    []annotationSummary
	fulltext       []store.FulltextSearchResult
}

// loadSummarizeFocus runs the --focus searches once for the whole selection.
// The store restricts both searches to keys in SQL, so no other item's text is
// read, and only bounded FTS snippets are returned, never full PDF text. label
// names the selection in warnings. Every key gets an entry.
func loadSummarizeFocus(ctx context.Context, db *store.Store, keys []string, label string, opts summarizeOpts) (map[string]*summarizeFocusHits, []string) {
	hits := make(map[string]*summarizeFocusHits, len(keys))
	for _, key := range keys {
		hits[key] = &summarizeFocusHits{fulltextStatus: focusFulltextSkipped, annotationsOK: true}
	}
	if len(keys) == 0 {
		return hits, nil
	}
	var warnings []string

	annHits, err := db.SearchAnnotationsContext(ctx, store.AnnotationSearch{Query: opts.focus, ParentKeys: keys})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("searching annotations for %s: %v", label, err))
		for _, h := range hits {
			h.annotationsOK = false
		}
	} else {
		for _, hit := range annHits {
			var obj map[string]any
			if json.Unmarshal(hit.Data, &obj) != nil {
				continue
			}
			summaries := annotationSummariesFromItems([]map[string]any{obj})
			if len(summaries) == 0 {
				continue
			}
			// The store matches a standalone attachment by its own key, and
			// then reports no parent item.
			owner := hit.ItemKey
			if owner == "" {
				owner = summaries[0].ParentItem
			}
			if h, ok := hits[owner]; ok {
				h.annotations = append(h.annotations, summaries[0])
			}
		}
	}
	if opts.noFulltext {
		return hits, warnings
	}

	indexed, err := db.FulltextIndexedParentsContext(ctx, keys)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("checking indexed fulltext for %s: %v", label, err))
		for _, h := range hits {
			h.fulltextStatus = focusFulltextError
		}
		return hits, warnings
	}
	for key, h := range hits {
		h.fulltextStatus = focusFulltextNotIndexed
		if indexed[key] {
			h.fulltextStatus = focusFulltextNoMatch
		}
	}
	matches, err := db.SearchFulltextContext(ctx, store.FulltextSearch{Query: opts.focus, Limit: -1, ParentKeys: keys})
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("searching fulltext for %s: %v", label, err))
		for _, h := range hits {
			if h.fulltextStatus == focusFulltextNoMatch {
				h.fulltextStatus = focusFulltextError
			}
		}
		return hits, warnings
	}
	for _, m := range matches {
		if h, ok := hits[m.ItemKey]; ok {
			h.fulltext = append(h.fulltext, m)
			h.fulltextStatus = focusFulltextMatched
		}
	}
	return hits, warnings
}

// buildFocusBundle assembles an item's bundle in --focus mode (pure; no store
// access). The matched annotations replace the page-ordered list and are capped
// by --max-annotations; the PDF-text snippets replace the first-pages excerpt
// and share the --max-chars budget, so the passage that would overflow it is
// cut and the rest dropped. Both caps report through the existing truncation
// fields.
func buildFocusBundle(raw json.RawMessage, hits *summarizeFocusHits, opts summarizeOpts) summarizeBundle {
	b, meta := newSummarizeBundle(raw)
	if hits == nil {
		hits = &summarizeFocusHits{fulltextStatus: focusFulltextSkipped, annotationsOK: true}
	}
	focus := &summarizeFocus{
		Terms:    opts.focus,
		Fulltext: hits.fulltextStatus,
		Passages: make([]summarizeFocusPassage, 0),
	}

	anns := make([]annotationSummary, 0, len(hits.annotations))
	for _, a := range hits.annotations {
		if strings.TrimSpace(a.Text) != "" || strings.TrimSpace(a.Comment) != "" {
			anns = append(anns, a)
		}
	}
	if total := len(anns); opts.maxAnnotations > 0 && total > opts.maxAnnotations {
		anns = anns[:opts.maxAnnotations]
		b.Truncated.Annotations = true
		b.Truncated.AnnotationsKept = len(anns)
		b.Truncated.AnnotationsTotal = total
	}
	for _, a := range anns {
		focus.Passages = append(focus.Passages, summarizeFocusPassage{
			Source:        "annotation",
			ItemKey:       b.Key,
			AttachmentKey: a.ParentItem,
			AnnotationKey: a.Key,
			Page:          a.Page,
			Text:          a.Text,
			Comment:       a.Comment,
		})
	}

	remaining := opts.maxChars
	for _, m := range hits.fulltext {
		text := strings.TrimSpace(m.Snippet)
		if text == "" {
			continue
		}
		if opts.maxChars > 0 {
			if remaining <= 0 {
				b.Truncated.Fulltext = true
				break
			}
			excerpt, cut := truncateRunes(text, remaining)
			if cut {
				b.Truncated.Fulltext = true
			}
			remaining -= utf8.RuneCountInString(excerpt)
			text = excerpt
		}
		focus.Passages = append(focus.Passages, summarizeFocusPassage{
			Source:        "fulltext",
			ItemKey:       m.ItemKey,
			AttachmentKey: m.AttachmentKey,
			Text:          text,
		})
	}

	focus.NoHit = len(focus.Passages) == 0 && hits.annotationsOK && hits.fulltextStatus != focusFulltextError
	b.Focus = focus
	indexed := hits.fulltextStatus == focusFulltextMatched || hits.fulltextStatus == focusFulltextNoMatch
	// A failed search proves nothing about the item, so it claims no gap.
	b.Gaps = itemGaps(meta, indexed, opts.noFulltext || hits.fulltextStatus == focusFulltextError)
	return b
}

func itemGaps(meta vaultMeta, hasFulltext, fulltextSkipped bool) []string {
	var gaps []string
	if strings.TrimSpace(meta.Abstract) == "" {
		gaps = append(gaps, "no abstract")
	}
	switch meta.ItemType {
	case "journalArticle", "conferencePaper", "preprint":
		if strings.TrimSpace(meta.DOI) == "" {
			gaps = append(gaps, "no DOI")
		}
	}
	if !hasFulltext && !fulltextSkipped {
		gaps = append(gaps, "no fulltext")
	}
	return gaps
}

// fulltextByParentItem streams stored attachment full text once and maps each
// parent item key to its PDF's stored full text, avoiding a per-item rescan in
// collection mode.

func fulltextByParentItemWithErr(ctx context.Context, db *store.Store) (map[string]string, error) {
	out := make(map[string]string)
	err := db.VisitSimilarityFulltextDocumentsContext(ctx, func(doc store.SimilarityFulltextDocument) error {
		if c := strings.TrimSpace(fulltextContent(doc.Data)); c != "" {
			out[doc.ParentItemKey] = c
		}
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("listing stored attachment full text: %w", err)
	}
	return out, nil
}

func fulltextForItem(ctx context.Context, db *store.Store, itemKey string) (string, error) {
	ft, ok, err := db.Fulltext(itemKey)
	if err != nil {
		return "", fmt.Errorf("reading item %s: %w", itemKey, err)
	}
	if ok {
		return fulltextContent(ft), nil
	}
	ft, ok, err = fulltextForPDFAttachment(ctx, db, itemKey)
	if err != nil {
		return "", err
	}
	if ok {
		return fulltextContent(ft), nil
	}
	return "", nil
}

// fulltextForPDFAttachment finds the first PDF child with stored full text.
// QueryItems scopes the attachment scan to one parent rather than loading every
// attachment in the local mirror.
func fulltextForPDFAttachment(ctx context.Context, db *store.Store, itemKey string) (json.RawMessage, bool, error) {
	attachments, err := db.QueryItemsContext(ctx, store.ItemQuery{
		ItemType: "attachment",
		Parent:   itemKey,
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing PDF attachments: %w", err)
	}
	for _, raw := range attachments {
		if jsonStringField(raw, "contentType") != "application/pdf" {
			continue
		}
		key := jsonStringField(raw, "key")
		if key == "" {
			continue
		}
		ft, ok, err := db.Fulltext(key)
		if err != nil {
			return nil, false, fmt.Errorf("reading attachment %s: %w", key, err)
		}
		if ok {
			return ft, true, nil
		}
	}
	return nil, false, nil
}

func fulltextContent(raw json.RawMessage) string {
	var obj struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	return obj.Content
}

func extractVenue(raw json.RawMessage) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	data, ok := obj["data"].(map[string]any)
	if !ok {
		data = obj
	}
	for _, k := range []string{"publicationTitle", "bookTitle", "proceedingsTitle", "publisher", "institution", "university"} {
		if v, _ := stringValue(data[k]); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func summarizeCitation(meta vaultMeta, venue string) string {
	var head []string
	if a := citationAuthors(meta.Authors); a != "" {
		head = append(head, a)
	}
	if meta.Year != "" {
		head = append(head, "("+meta.Year+")")
	}
	cite := strings.Join(head, " ")
	if t := strings.TrimSpace(meta.Title); t != "" {
		if cite != "" {
			cite += ". "
		}
		cite += t
	}
	if venue != "" {
		cite += ". " + venue
	}
	cite = strings.TrimSpace(cite)
	if cite == "" {
		return meta.Key
	}
	return cite
}

func citationAuthors(authors []string) string {
	if len(authors) == 0 {
		return ""
	}
	if len(authors) <= 3 {
		return strings.Join(authors, "; ")
	}
	return authors[0] + " et al."
}

// truncateRunes caps s at max characters (runes) without splitting a UTF-8
// rune; the bool reports whether anything was cut.
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s, false
	}
	// Advance by rune index to honor --max-chars as a character contract;
	// avoid allocating a full []rune for large limits.
	idx := 0
	for pos := range s {
		if idx == max {
			return s[:pos], true
		}
		idx++
	}
	return s, false
}

func renderBundleMarkdown(b summarizeBundle, level int, withPrompt bool) string {
	h := strings.Repeat("#", level)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n", h, b.Citation)

	var meta []string
	if b.Key != "" {
		meta = append(meta, "`"+b.Key+"`")
	}
	if b.DOI != "" {
		meta = append(meta, "doi:"+b.DOI)
	}
	if b.URL != "" {
		meta = append(meta, b.URL)
	}
	if len(meta) > 0 {
		sb.WriteString(strings.Join(meta, " · ") + "\n")
	}

	if b.Abstract != "" {
		fmt.Fprintf(&sb, "\n**Abstract**\n\n%s\n", summarizeFence(b.Abstract))
	}

	if len(b.Annotations) > 0 {
		count := fmt.Sprintf("%d", len(b.Annotations))
		if b.Truncated.Annotations {
			count = fmt.Sprintf("%d of %d", b.Truncated.AnnotationsKept, b.Truncated.AnnotationsTotal)
		}
		fmt.Fprintf(&sb, "\n**Annotations (%s)**\n\n", count)
		for _, a := range b.Annotations {
			sb.WriteString("- ")
			if a.Page != "" {
				fmt.Fprintf(&sb, "p.%s ", a.Page)
			}
			if a.Type != "" {
				fmt.Fprintf(&sb, "[%s] ", a.Type)
			}
			if a.Text != "" {
				fmt.Fprintf(&sb, "%s", summarizeInlineQuote(a.Text))
			}
			if a.Comment != "" {
				sb.WriteString(" — " + summarizeInlineQuote(a.Comment))
			}
			sb.WriteString("\n")
		}
	}

	if f := b.Focus; f != nil {
		fmt.Fprintf(&sb, "\n**Focus passages for %s** (PDF text: %s", summarizeInlineQuote(f.Terms), f.Fulltext)
		if b.Truncated.Fulltext || b.Truncated.Annotations {
			sb.WriteString("; truncated")
		}
		sb.WriteString(")\n\n")
		switch {
		case f.NoHit:
			sb.WriteString("No passage matched.\n")
		case len(f.Passages) == 0:
			sb.WriteString("No passage found; a search failed (see warnings).\n")
		}
		for _, p := range f.Passages {
			sb.WriteString("- [" + p.Source)
			if p.AttachmentKey != "" {
				fmt.Fprintf(&sb, " `%s`", p.AttachmentKey)
			}
			if p.Page != "" {
				fmt.Fprintf(&sb, " p.%s", p.Page)
			}
			sb.WriteString("] ")
			if p.Text != "" {
				sb.WriteString(summarizeInlineQuote(p.Text))
			}
			if p.Comment != "" {
				sb.WriteString(" — " + summarizeInlineQuote(p.Comment))
			}
			sb.WriteString("\n")
		}
	}

	if b.Fulltext != "" {
		label := "**Fulltext excerpt**"
		if b.Truncated.Fulltext {
			label = "**Fulltext excerpt** (truncated)"
		}
		fmt.Fprintf(&sb, "\n%s\n\n%s\n", label, summarizeFence(b.Fulltext))
	}

	if len(b.Gaps) > 0 {
		fmt.Fprintf(&sb, "\n**Gaps:** %s\n", strings.Join(b.Gaps, ", "))
	}

	if withPrompt && b.Prompt != "" {
		fmt.Fprintf(&sb, "\n---\n**Synthesis prompt:** %s\n", b.Prompt)
	}
	return sb.String()
}

func summarizeInlineQuote(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", " / ")
	return fmt.Sprintf("%q", s)
}

func summarizeFence(s string) string {
	// Zotero document content is untrusted prompt input. Delimit it with a fence
	// longer than any embedded backtick run so content cannot escape into the
	// surrounding instructions.
	maxRun, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", maxRun+3)
	return fence + "\n" + s + "\n" + fence
}

func renderCollectionMarkdown(cb summarizeCollectionBundle) string {
	var sb strings.Builder
	// A collection cohort keeps its "Collection <key>" heading under either
	// spelling; a tag/query/item cohort has no collection key to print, so it is
	// headed by the scope expression that produced it.
	if cb.Collection != "" {
		fmt.Fprintf(&sb, "# Collection `%s` — %d item(s)\n", cb.Collection, cb.ItemCount)
	} else {
		fmt.Fprintf(&sb, "# Scope `%s` — %d item(s)\n", cb.Scope, cb.ItemCount)
	}
	for _, item := range cb.Items {
		sb.WriteString("\n")
		sb.WriteString(renderBundleMarkdown(item, 2, false))
	}
	if cb.Prompt != "" {
		fmt.Fprintf(&sb, "\n---\n**Synthesis prompt:** %s\n", cb.Prompt)
	}
	return sb.String()
}
