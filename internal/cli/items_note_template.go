// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

var dateYearPattern = regexp.MustCompile(`\b(1[5-9]\d{2}|20\d{2})\b`)

type itemNoteMetadata struct {
	Title    string
	Authors  []string
	Year     string
	DOI      string
	Abstract string
	CiteKey  string
}

func newItemsNoteTemplateCmd(flags *rootFlags) *cobra.Command {
	var flagFormat string

	cmd := &cobra.Command{
		Use:   "note-template <itemKey>",
		Short: "Generate a markdown reading-note template for an item",
		Example: `  zotio items note-template ABCD1234
  zotio items note-template ABCD1234 --format obsidian
  zotio items note-template ABCD1234 --format logseq`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])
			data, provenance, err := resolveRead(cmd.Context(), c, flags, "items", false, path, nil, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			meta, err := noteMetadataFromItem(data)
			if err != nil {
				return err
			}

			anns, err := noteTemplateAnnotations(cmd.Context(), c, flags, args[0], provenance)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			var out string
			switch strings.ToLower(strings.TrimSpace(flagFormat)) {
			case "", "standard":
				out = renderStandardNoteTemplate(meta, anns, false, time.Now())
			case "obsidian":
				out = renderStandardNoteTemplate(meta, anns, true, time.Now())
			case "logseq":
				out = renderLogseqNoteTemplate(meta, anns, time.Now())
			default:
				return fmt.Errorf("invalid --format value %q: must be standard, obsidian, or logseq", flagFormat)
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&flagFormat, "format", "standard", "Template format: standard, obsidian, or logseq")

	return cmd
}

// noteTemplateAnnotations pins the annotation read to the source that supplied
// the item metadata. In auto mode this prevents a live item from being combined
// with stale mirror annotations, while preserving local fallback when the item
// read already fell back to the mirror.
func noteTemplateAnnotations(ctx context.Context, c *client.Client, flags *rootFlags, itemKey string, provenance DataProvenance) ([]annotationSummary, error) {
	switch provenance.Source {
	case "local":
		db, err := openStoreForRead(ctx, "zotio")
		if err != nil {
			return nil, fmt.Errorf("opening local database: %w\nRun 'zotio sync' first.", err)
		}
		if db == nil {
			return nil, fmt.Errorf("no local data. Run 'zotio sync' first")
		}
		defer db.Close()
		annByKey, err := db.AnnotationsForItems([]string{itemKey})
		if err != nil {
			return nil, fmt.Errorf("querying annotations: %w", err)
		}
		return annotationSummariesSorted(annByKey[itemKey]), nil

	case "live":
		liveFlags := *flags
		liveFlags.dataSource = "live"
		attachments, _, err := fetchResolvedZoteroItems(
			ctx,
			c,
			&liveFlags,
			"/items/"+url.PathEscape(itemKey)+"/children",
			map[string]string{"itemType": "attachment"},
			0,
		)
		if err != nil {
			return nil, fmt.Errorf("reading attachment children for %s: %w", itemKey, err)
		}

		var annotationItems []map[string]any
		for _, attachment := range attachments {
			if zoteroString(attachment, "itemType") != "attachment" {
				continue
			}
			attachmentKey := zoteroString(attachment, "key")
			if attachmentKey == "" {
				continue
			}
			items, _, err := fetchResolvedZoteroItems(
				ctx,
				c,
				&liveFlags,
				"/items/"+url.PathEscape(attachmentKey)+"/children",
				map[string]string{"itemType": "annotation"},
				0,
			)
			if err != nil {
				return nil, fmt.Errorf("reading annotation children for %s: %w", attachmentKey, err)
			}
			annotationItems = append(annotationItems, items...)
		}
		annotations := annotationSummariesFromItems(annotationItems)
		sort.SliceStable(annotations, func(i, j int) bool {
			pi, pj := annotationPageNum(annotations[i].Page), annotationPageNum(annotations[j].Page)
			if pi != pj {
				return pi < pj
			}
			return annotations[i].DateAdded < annotations[j].DateAdded
		})
		return annotations, nil

	default:
		return nil, fmt.Errorf("unsupported note-template data source %q", provenance.Source)
	}
}

func noteMetadataFromItem(raw json.RawMessage) (itemNoteMetadata, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return itemNoteMetadata{}, fmt.Errorf("parsing item response: %w", err)
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		dataObj = obj
	}

	title, _ := stringValue(dataObj["title"])
	doi, _ := stringValue(dataObj["DOI"])
	abstractNote, _ := stringValue(dataObj["abstractNote"])
	extra, _ := stringValue(dataObj["extra"])
	date, _ := stringValue(dataObj["date"])

	meta := itemNoteMetadata{
		Title:    title,
		Authors:  noteAuthors(dataObj["creators"]),
		Year:     yearFromDate(date),
		DOI:      doi,
		Abstract: strings.TrimSpace(abstractNote),
		// One parser for "what is this item's citekey": resolveCiteKey reads
		// the Better BibTeX citationKey field first and falls back to both
		// Extra spellings. This callsite has no field to offer, only Extra.
		CiteKey: resolveCiteKey("", extra),
	}
	return meta, nil
}

func noteAuthors(raw any) []string {
	creators, ok := raw.([]any)
	if !ok {
		return nil
	}
	authors := make([]string, 0, 3)
	for _, creator := range creators {
		creatorObj, ok := creator.(map[string]any)
		if !ok {
			continue
		}
		lastName, _ := stringValue(creatorObj["lastName"])
		firstName, _ := stringValue(creatorObj["firstName"])
		name, _ := stringValue(creatorObj["name"])
		displayName := formatCreatorDisplayName(lastName, firstName, name)
		if displayName == "" {
			continue
		}
		authors = append(authors, displayName)
		if len(authors) == 3 {
			break
		}
	}
	return authors
}

func yearFromDate(date string) string {
	return dateYearPattern.FindString(date)
}

func renderStandardNoteTemplate(meta itemNoteMetadata, anns []annotationSummary, obsidian bool, now time.Time) string {
	authors := meta.Authors
	if obsidian {
		authors = wikilinkAuthors(authors)
	}
	abstract := sanitizeManagedFenceMarkers(meta.Abstract)
	if abstract == "" {
		abstract = "(no abstract)"
	}

	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", strconv.Quote(meta.Title))
	fmt.Fprintf(&b, "authors: %s\n", yamlStringArray(authors))
	if meta.Year == "" {
		b.WriteString("year:\n")
	} else {
		fmt.Fprintf(&b, "year: %s\n", strconv.Quote(meta.Year))
	}
	// Quote Zotero-derived frontmatter scalars so DOI/citekey punctuation cannot
	// alter YAML.
	fmt.Fprintf(&b, "doi: %s\n", strconv.Quote(meta.DOI))
	fmt.Fprintf(&b, "cite_key: %s\n", strconv.Quote(meta.CiteKey))
	b.WriteString("tags: []\n")
	fmt.Fprintf(&b, "date_read: %s\n", now.Format("2006-01-02"))
	b.WriteString("---\n\n")
	b.WriteString("## Abstract\n\n")
	b.WriteString(abstract)
	b.WriteString("\n\n## Key Points\n\n-\n\n")
	b.WriteString("## Annotations\n\n")
	b.WriteString(renderNoteTemplateAnnotationBlock(anns, ""))
	b.WriteString("\n\n## Notes\n")
	return b.String()
}

func renderLogseqNoteTemplate(meta itemNoteMetadata, anns []annotationSummary, now time.Time) string {
	abstract := sanitizeManagedFenceMarkers(meta.Abstract)
	if abstract == "" {
		abstract = "(no abstract)"
	}
	authors := wikilinkAuthors(meta.Authors)

	var b strings.Builder
	// Logseq properties are one-line fields, so collapse Zotero-derived property
	// values.
	fmt.Fprintf(&b, "- title:: %s\n", logseqPropScalar(meta.Title))
	if len(authors) > 0 {
		fmt.Fprintf(&b, "- authors:: %s\n", logseqPropScalar(strings.Join(authors, ", ")))
	} else {
		b.WriteString("- authors::\n")
	}
	fmt.Fprintf(&b, "- year:: %s\n", logseqPropScalar(meta.Year))
	fmt.Fprintf(&b, "- doi:: %s\n", logseqPropScalar(meta.DOI))
	fmt.Fprintf(&b, "- cite_key:: %s\n", logseqPropScalar(meta.CiteKey))
	b.WriteString("- tags::\n")
	fmt.Fprintf(&b, "- date_read:: %s\n", now.Format("2006-01-02"))
	b.WriteString("- ## Abstract\n")
	fmt.Fprintf(&b, "  - %s\n", abstract)
	b.WriteString("- ## Key Points\n")
	b.WriteString("  - \n")
	b.WriteString("- ## Annotations\n")
	b.WriteString(renderNoteTemplateAnnotationBlock(anns, "  "))
	b.WriteString("\n- ## Notes\n")
	b.WriteString("  - \n")
	return b.String()
}

func renderNoteTemplateAnnotationBlock(anns []annotationSummary, prefix string) string {
	block := vaultAnnBegin + "\n" + vaultAnnEnd
	if len(anns) > 0 {
		block = renderAnnotationBlock(anns)
	}
	if prefix == "" {
		return block
	}
	return prefix + strings.ReplaceAll(block, "\n", "\n"+prefix)
}

func yamlStringArray(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func wikilinkAuthors(authors []string) []string {
	out := make([]string, 0, len(authors))
	for _, author := range authors {
		out = append(out, "[["+author+"]]")
	}
	return out
}
