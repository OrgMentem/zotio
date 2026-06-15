// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written Zotero annotation markdown/JSON export workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type annotationExportItem struct {
	Key         string              `json:"key"`
	Title       string              `json:"title"`
	Year        string              `json:"year,omitempty"`
	Authors     []string            `json:"authors,omitempty"`
	DOI         string              `json:"doi,omitempty"`
	Annotations []annotationSummary `json:"annotations"`
}

type annotationSummary struct {
	Key        string `json:"key"`
	ParentItem string `json:"parent_item"`
	DateAdded  string `json:"date_added"`
	Color      string `json:"color"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	Comment    string `json:"comment"`
	Page       string `json:"page"`
}

type zoteroGetter interface {
	Get(path string, params map[string]string) (json.RawMessage, error)
}

func newAnnotationsExportCmd(flags *rootFlags) *cobra.Command {
	var flagCollection string
	var flagTag string
	var flagOutput string
	var flagFormat string
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "export",
		Short:       "Export annotations as markdown or JSON",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagCollection != "" && flagTag != "" {
				return usageErr(fmt.Errorf("use only one of --collection or --tag"))
			}
			format := strings.ToLower(strings.TrimSpace(flagFormat))
			if flags.asJSON && !cmd.Flags().Changed("format") {
				format = "json"
			}
			switch format {
			case "markdown", "json":
			default:
				return usageErr(fmt.Errorf("invalid --format value %q: must be markdown or json", flagFormat))
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items/top"
			params := map[string]string{}
			if flagCollection != "" {
				path = "/collections/" + flagCollection + "/items"
			} else if flagTag != "" {
				path = "/items"
				params["tag"] = flagTag
			}

			items, err := fetchZoteroItems(c, path, params, flagLimit)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			exports := make([]annotationExportItem, 0)
			for _, item := range items {
				if !zoteroItemHasChildren(item) {
					continue
				}
				key := zoteroString(item, "key")
				if key == "" {
					continue
				}
				children, err := c.Get("/items/"+key+"/children", map[string]string{"itemType": "annotation"})
				if err != nil {
					return classifyAPIError(err, flags)
				}
				childItems, err := decodeZoteroItems(children)
				if err != nil {
					return fmt.Errorf("parsing annotation children for %s: %w", key, err)
				}
				annotations := annotationSummariesFromItems(childItems)
				if len(annotations) == 0 {
					continue
				}
				exports = append(exports, annotationExportItem{
					Key:         key,
					Title:       zoteroString(item, "title"),
					Year:        zoteroItemYear(item),
					Authors:     zoteroItemAuthors(item),
					DOI:         zoteroString(item, "DOI"),
					Annotations: annotations,
				})
			}

			var out []byte
			if format == "json" {
				out, err = json.MarshalIndent(exports, "", "  ")
				if err != nil {
					return err
				}
				out = append(out, '\n')
			} else {
				out = []byte(formatAnnotationExportMarkdown(exports))
			}
			if flagOutput != "" {
				return os.WriteFile(flagOutput, out, 0o644)
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Scope to items in this collection key")
	cmd.Flags().StringVar(&flagTag, "tag", "", "Scope to items with this tag")
	cmd.Flags().StringVar(&flagOutput, "output", "", "Output file path (default: stdout)")
	cmd.Flags().StringVar(&flagFormat, "format", "markdown", "Output format (markdown or json)")
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of items to process")

	return cmd
}

func fetchZoteroItems(c zoteroGetter, path string, params map[string]string, maxItems int) ([]map[string]any, error) {
	all := make([]map[string]any, 0)
	start := 0
	pageSize := 100
	for {
		if maxItems > 0 {
			remaining := maxItems - len(all)
			if remaining <= 0 {
				break
			}
			if remaining < pageSize {
				pageSize = remaining
			} else {
				pageSize = 100
			}
		}

		pageParams := cloneStringMap(params)
		pageParams["limit"] = fmt.Sprintf("%d", pageSize)
		pageParams["start"] = fmt.Sprintf("%d", start)

		data, err := c.Get(path, pageParams)
		if err != nil {
			return nil, err
		}
		items, err := decodeZoteroItems(data)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		all = append(all, items...)
		if len(items) < pageSize {
			break
		}
		start += len(items)
	}
	if maxItems > 0 && len(all) > maxItems {
		all = all[:maxItems]
	}
	return all, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func decodeZoteroItems(data json.RawMessage) ([]map[string]any, error) {
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err == nil {
		return items, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("expected Zotero item array: %w", err)
	}
	for _, key := range []string{"data", "items", "results"} {
		if raw, ok := envelope[key]; ok {
			if err := json.Unmarshal(raw, &items); err == nil {
				return items, nil
			}
		}
	}
	return nil, fmt.Errorf("expected Zotero item array")
}

func annotationSummariesFromItems(items []map[string]any) []annotationSummary {
	annotations := make([]annotationSummary, 0, len(items))
	for _, item := range items {
		if itemType := zoteroString(item, "itemType"); itemType != "" && itemType != "annotation" {
			continue
		}
		annotations = append(annotations, annotationSummary{
			Key:        zoteroString(item, "key"),
			ParentItem: zoteroString(item, "parentItem"),
			DateAdded:  zoteroString(item, "dateAdded"),
			Color:      zoteroString(item, "annotationColor"),
			Type:       zoteroString(item, "annotationType"),
			Text:       zoteroString(item, "annotationText"),
			Comment:    zoteroString(item, "annotationComment"),
			Page:       zoteroString(item, "annotationPageLabel"),
		})
	}
	return annotations
}

func zoteroString(item map[string]any, field string) string {
	return strings.TrimSpace(jsonStringFieldFromMap(item, field))
}

func zoteroData(item map[string]any) map[string]any {
	data, _ := item["data"].(map[string]any)
	return data
}

func zoteroItemHasChildren(item map[string]any) bool {
	if meta, ok := item["meta"].(map[string]any); ok {
		if sqlIntValue(meta["numChildren"]) > 0 {
			return true
		}
	}
	if data := zoteroData(item); data != nil {
		if sqlIntValue(data["numChildren"]) > 0 {
			return true
		}
	}
	if links, ok := item["links"].(map[string]any); ok {
		_, ok := links["children"]
		return ok
	}
	return false
}

func zoteroItemAuthors(item map[string]any) []string {
	data := zoteroData(item)
	if data == nil {
		return nil
	}
	creators, _ := data["creators"].([]any)
	authors := make([]string, 0, len(creators))
	fallback := make([]string, 0, len(creators))
	for _, raw := range creators {
		creator, _ := raw.(map[string]any)
		if creator == nil {
			continue
		}
		name := zoteroCreatorName(creator)
		if name == "" {
			continue
		}
		fallback = append(fallback, name)
		if strings.EqualFold(strings.TrimSpace(fmt.Sprintf("%v", creator["creatorType"])), "author") {
			authors = append(authors, name)
		}
	}
	if len(authors) > 0 {
		return authors
	}
	return fallback
}

func zoteroFirstAuthor(item map[string]any) string {
	authors := zoteroItemAuthors(item)
	if len(authors) == 0 {
		return ""
	}
	return authors[0]
}

func zoteroCreatorName(creator map[string]any) string {
	if name := strings.TrimSpace(fmt.Sprintf("%v", creator["name"])); name != "" && name != "<nil>" {
		return name
	}
	first := strings.TrimSpace(fmt.Sprintf("%v", creator["firstName"]))
	last := strings.TrimSpace(fmt.Sprintf("%v", creator["lastName"]))
	if first == "<nil>" {
		first = ""
	}
	if last == "<nil>" {
		last = ""
	}
	return strings.TrimSpace(strings.Join([]string{first, last}, " "))
}

func zoteroItemYear(item map[string]any) string {
	return firstFourDigitYear(zoteroString(item, "date"))
}

func firstFourDigitYear(value string) string {
	for i := 0; i+4 <= len(value); i++ {
		candidate := value[i : i+4]
		if candidate[0] >= '1' && candidate[0] <= '2' &&
			candidate[1] >= '0' && candidate[1] <= '9' &&
			candidate[2] >= '0' && candidate[2] <= '9' &&
			candidate[3] >= '0' && candidate[3] <= '9' {
			return candidate
		}
	}
	return ""
}

func formatAnnotationExportMarkdown(items []annotationExportItem) string {
	var b strings.Builder
	for _, item := range items {
		title := item.Title
		if title == "" {
			title = item.Key
		}
		if item.Year != "" {
			fmt.Fprintf(&b, "# %s (%s)\n", title, item.Year)
		} else {
			fmt.Fprintf(&b, "# %s\n", title)
		}
		if len(item.Authors) > 0 {
			fmt.Fprintf(&b, "**Authors:** %s\n", strings.Join(item.Authors, ", "))
		}
		fmt.Fprintf(&b, "**Key:** %s\n", item.Key)
		if item.DOI != "" {
			fmt.Fprintf(&b, "**DOI:** %s\n", item.DOI)
		}
		b.WriteString("\n## Annotations\n\n")
		for _, annotation := range item.Annotations {
			if annotation.Text != "" {
				fmt.Fprintf(&b, "> %s", annotation.Text)
				if annotation.Page != "" {
					fmt.Fprintf(&b, " (p. %s)", annotation.Page)
				}
				b.WriteString("\n")
			}
			if annotation.Comment != "" {
				fmt.Fprintf(&b, "*%s*\n", annotation.Comment)
			}
			b.WriteString("\n")
		}
		b.WriteString("---\n\n")
	}
	return b.String()
}

func printCommandJSON(w io.Writer, v any, flags *rootFlags) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	jsonFlags := *flags
	jsonFlags.asJSON = true
	jsonFlags.compact = false
	jsonFlags.csv = false
	jsonFlags.plain = false
	return printOutputWithFlags(w, json.RawMessage(data), &jsonFlags)
}
