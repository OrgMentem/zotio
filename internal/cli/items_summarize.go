// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// items summarize assembles a bounded, synthesis-ready context bundle for an item
// or collection. It does not call a model: it gathers the highest-signal local
// context (citation, abstract, the reader's own annotations, a capped fulltext
// excerpt, metadata gaps) plus a synthesis prompt, and lets the host LLM do the
// writing. Reads are local only; nothing is written.

package cli

import (
	"encoding/json"
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
}

type summarizeAnnot struct {
	Page    string `json:"page,omitempty"`
	Type    string `json:"type,omitempty"`
	Text    string `json:"text,omitempty"`
	Comment string `json:"comment,omitempty"`
}

type summarizeTruncation struct {
	Fulltext         bool `json:"fulltext"`
	Annotations      bool `json:"annotations"`
	AnnotationsKept  int  `json:"annotations_kept,omitempty"`
	AnnotationsTotal int  `json:"annotations_total,omitempty"`
}

type summarizeBundle struct {
	Key         string              `json:"key"`
	Citation    string              `json:"citation"`
	ItemType    string              `json:"item_type,omitempty"`
	DOI         string              `json:"doi,omitempty"`
	URL         string              `json:"url,omitempty"`
	Abstract    string              `json:"abstract,omitempty"`
	Annotations []summarizeAnnot    `json:"annotations,omitempty"`
	Fulltext    string              `json:"fulltext_excerpt,omitempty"`
	Gaps        []string            `json:"gaps,omitempty"`
	Truncated   summarizeTruncation `json:"truncated"`
	Prompt      string              `json:"prompt,omitempty"`
}

type summarizeCollectionBundle struct {
	Collection string            `json:"collection"`
	ItemCount  int               `json:"item_count"`
	Items      []summarizeBundle `json:"items"`
	Prompt     string            `json:"prompt"`
}

const itemSynthesisPrompt = "Summarize this work for a literature review: core claim/contribution, method, key findings, and limitations. Ground every point in the abstract, annotations, and excerpt above — do not invent."

func collectionSynthesisPrompt(n int) string {
	return fmt.Sprintf("Synthesize across these %d works (cited by key): shared themes, points of agreement and contradiction, methodological patterns, and open gaps. Ground each claim in the items above and cite the relevant item keys.", n)
}

func newItemsSummarizeCmd(flags *rootFlags) *cobra.Command {
	var (
		flagCollection     string
		flagMaxChars       int
		flagMaxAnnotations int
		flagNoFulltext     bool
	)
	cmd := &cobra.Command{
		Use:   "summarize [<itemKey>]",
		Short: "Assemble a bounded, synthesis-ready context bundle for an item or collection",
		Long: `Gather the highest-signal local context for an item (or every item in a
collection) into one bounded bundle — citation, abstract, your annotations, a
capped fulltext excerpt, and known metadata gaps — plus a synthesis prompt.

This command never calls a model: it does the assembly and budgeting the host LLM
is bad at, then hands off. Reads are local only. With --agent/--json it emits the
structured bundle; otherwise a readable Markdown brief you can paste into any LLM.`,
		Example: `  zotio items summarize 9UXV5R7L
  zotio items summarize 9UXV5R7L --agent --max-chars 6000
  zotio items summarize --collection MAR7RFQN --no-fulltext`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			db, _ := openStoreForRead(cmd.Context(), "zotio")
			if db == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer db.Close()

			opts := summarizeOpts{
				maxChars:       flagMaxChars,
				maxAnnotations: flagMaxAnnotations,
				noFulltext:     flagNoFulltext,
			}

			if flagCollection != "" {
				return runSummarizeCollection(cmd, db, flagCollection, opts, flags)
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			raw, err := db.Get("items", args[0])
			if err != nil {
				return fmt.Errorf("reading item: %w", err)
			}
			if raw == nil {
				return fmt.Errorf("item %s not found locally; run 'zotio sync' (or check the key)", args[0])
			}

			annByKey, _ := db.AnnotationsForItems([]string{args[0]})
			fulltext := ""
			if !opts.noFulltext {
				if ft, ok := localPDFFulltext(db, args[0]); ok {
					fulltext = fulltextContent(ft)
				}
			}
			bundle := buildItemBundle(raw, annByKey[args[0]], fulltext, opts)

			if flags.asJSON {
				data, merr := json.Marshal(bundle)
				if merr != nil {
					return merr
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			fmt.Fprint(cmd.OutOrStdout(), renderBundleMarkdown(bundle, 1, true))
			return nil
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Summarize every item in this collection key")
	cmd.Flags().IntVar(&flagMaxChars, "max-chars", 8000, "Max characters of fulltext excerpt per item")
	cmd.Flags().IntVar(&flagMaxAnnotations, "max-annotations", 40, "Max annotations included per item")
	cmd.Flags().BoolVar(&flagNoFulltext, "no-fulltext", false, "Omit the fulltext excerpt (abstract + annotations only)")
	return cmd
}

func runSummarizeCollection(cmd *cobra.Command, db *store.Store, collKey string, opts summarizeOpts, flags *rootFlags) error {
	items, err := db.QueryItems(store.ItemQuery{
		Collection: collKey,
		TopOnly:    true,
		Sort:       "title",
		Direction:  "asc",
	})
	if err != nil {
		return fmt.Errorf("querying collection items: %w", err)
	}

	keys := make([]string, 0, len(items))
	for _, raw := range items {
		keys = append(keys, vaultItemMeta(raw).Key)
	}
	annByKey, _ := db.AnnotationsForItems(keys)
	// Batch fulltext once (parent item key -> content) so a collection does not
	// re-scan the attachment table per item.
	var ftByItem map[string]string
	if !opts.noFulltext {
		ftByItem = fulltextByParentItem(db)
	}

	cb := summarizeCollectionBundle{Collection: collKey, ItemCount: len(items)}
	for _, raw := range items {
		key := vaultItemMeta(raw).Key
		cb.Items = append(cb.Items, buildItemBundle(raw, annByKey[key], ftByItem[key], opts))
	}
	cb.Prompt = collectionSynthesisPrompt(len(items))

	if flags.asJSON {
		data, merr := json.Marshal(cb)
		if merr != nil {
			return merr
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}
	fmt.Fprint(cmd.OutOrStdout(), renderCollectionMarkdown(cb))
	return nil
}

// buildItemBundle assembles the bounded bundle from already-fetched inputs (pure;
// no store access) so it is easy to test and reuse. fulltext is "" when omitted
// or unavailable.
func buildItemBundle(raw json.RawMessage, annRows []json.RawMessage, fulltext string, opts summarizeOpts) summarizeBundle {
	meta := vaultItemMeta(raw)
	b := summarizeBundle{
		Key:      meta.Key,
		Citation: summarizeCitation(meta, extractVenue(raw)),
		ItemType: meta.ItemType,
		DOI:      meta.DOI,
		URL:      meta.URL,
		Abstract: meta.Abstract,
		Prompt:   itemSynthesisPrompt,
	}

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
		b.Annotations = append(b.Annotations, summarizeAnnot{Page: a.Page, Type: a.Type, Text: a.Text, Comment: a.Comment})
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

// fulltextByParentItem scans the attachment table once and maps each parent item
// key to its PDF's stored full text, avoiding a per-item rescan in collection mode.
func fulltextByParentItem(db *store.Store) map[string]string {
	attachments, err := db.ItemsByType("attachment", 0)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(attachments))
	for _, raw := range attachments {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		data, ok := obj["data"].(map[string]any)
		if !ok {
			continue
		}
		if ct, _ := stringValue(data["contentType"]); ct != "application/pdf" {
			continue
		}
		parent, _ := stringValue(data["parentItem"])
		akey, _ := stringValue(data["key"])
		if akey == "" {
			akey, _ = stringValue(obj["key"])
		}
		if parent == "" || akey == "" {
			continue
		}
		if ft, ok, _ := db.Fulltext(akey); ok {
			if c := strings.TrimSpace(fulltextContent(ft)); c != "" {
				out[parent] = c
			}
		}
	}
	return out
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

// truncateRunes caps s at max bytes without splitting a UTF-8 rune; the bool
// reports whether anything was cut.
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	b := []byte(s)
	i := max
	for i > 0 && !utf8.RuneStart(b[i]) {
		i--
	}
	return string(b[:i]), true
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
	fmt.Fprintf(&sb, "# Collection `%s` — %d item(s)\n", cb.Collection, cb.ItemCount)
	for _, item := range cb.Items {
		sb.WriteString("\n")
		sb.WriteString(renderBundleMarkdown(item, 2, false))
	}
	if cb.Prompt != "" {
		fmt.Fprintf(&sb, "\n---\n**Synthesis prompt:** %s\n", cb.Prompt)
	}
	return sb.String()
}
