// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

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
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])
			data, _, err := resolveRead(cmd.Context(), c, flags, "items", false, path, nil, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			meta, err := noteMetadataFromItem(data)
			if err != nil {
				return err
			}

			var out string
			switch strings.ToLower(strings.TrimSpace(flagFormat)) {
			case "", "standard":
				out = renderStandardNoteTemplate(meta, false, time.Now())
			case "obsidian":
				out = renderStandardNoteTemplate(meta, true, time.Now())
			case "logseq":
				out = renderLogseqNoteTemplate(meta, time.Now())
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
		CiteKey:  citationKeyFromExtra(extra),
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
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return ""
	}
	year := date[:4]
	for _, r := range year {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return year
}

func citationKeyFromExtra(extra string) string {
	for _, line := range strings.Split(extra, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Citation Key: ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "Citation Key: "))
		}
	}
	return ""
}

func renderStandardNoteTemplate(meta itemNoteMetadata, obsidian bool, now time.Time) string {
	authors := meta.Authors
	if obsidian {
		authors = wikilinkAuthors(authors)
	}
	abstract := meta.Abstract
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
	b.WriteString("<!-- Export annotations with: zotio items annotations <itemKey> -->\n\n")
	b.WriteString("## Notes\n")
	return b.String()
}

func renderLogseqNoteTemplate(meta itemNoteMetadata, now time.Time) string {
	abstract := meta.Abstract
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
	b.WriteString("  - Export annotations with: zotio items annotations <itemKey>\n")
	b.WriteString("- ## Notes\n")
	b.WriteString("  - \n")
	return b.String()
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
