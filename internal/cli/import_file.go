// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written bibliography file import workflow missing from the generated CLI.

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
)

const importFileBatchSize = 50

var bibTeXEntryStartPattern = regexp.MustCompile(`(?is)@([a-zA-Z]+)\s*\{`)
var bibTeXAuthorSplitPattern = regexp.MustCompile(`(?i)\s+and\s+`)

func newImportFileCmd(flags *rootFlags) *cobra.Command {
	var flagFormat string
	var flagCollection string

	cmd := &cobra.Command{
		Use:         "file <path>",
		Short:       "Import items from BibTeX, RIS, or CSL JSON",
		Annotations: map[string]string{"pp:method": "POST", "pp:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			filePath := args[0]
			content, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("reading import file: %w", err)
			}

			format := strings.ToLower(strings.TrimSpace(flagFormat))
			if format == "" {
				format = detectImportFileFormat(filePath)
			}

			if flags.via == "connector" {
				return importFileViaConnector(cmd, flags, filePath, content, format, flagCollection)
			}
			items, err := parseImportFileItems(string(content), format, flagCollection)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return fmt.Errorf("no items found in %s", filePath)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			for start := 0; start < len(items); start += importFileBatchSize {
				end := start + importFileBatchSize
				if end > len(items) {
					end = len(items)
				}
				_, _, err := c.Post("/items", items[start:end])
				if err != nil {
					return classifyAPIError(err, flags)
				}
			}

			if flags.asJSON || flags.csv || flags.plain {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"file":     filePath,
					"imported": len(items),
				}, flags)
			}
			if flags.quiet {
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Imported %d items from %s\n", len(items), filePath)
			return nil
		},
	}
	cmd.Flags().StringVar(&flagFormat, "format", "", "Input format (bibtex, ris, csljson)")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add imported items to")

	return cmd
}

func importFileViaConnector(cmd *cobra.Command, flags *rootFlags, filePath string, content []byte, format, collectionKey string) error {
	via, err := flags.resolveCreateVia(cmd.Context(), collectionKey != "" || strings.TrimSpace(flags.connectorTarget) != "")
	if err != nil {
		return preconditionErr(err)
	}
	if via != "connector" {
		return preconditionErr(fmt.Errorf("import file --via connector requires the desktop connector (local base URL + Zotero running)"))
	}
	conn, err := flags.newConnector()
	if err != nil {
		return err
	}
	target := strings.TrimSpace(flags.connectorTarget)
	if target == "" && strings.TrimSpace(collectionKey) != "" {
		target, err = resolveConnectorTarget(cmd.Context(), flags, conn, collectionKey)
		if err != nil {
			return err
		}
	}
	if flags.dryRun {
		return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"dry_run": true,
			"file":    filePath,
			"via":     "connector",
			"target":  target,
		}, flags)
	}
	sessionID, err := connector.NewID()
	if err != nil {
		return err
	}
	items, err := conn.Import(cmd.Context(), sessionID, content, connectorImportContentType(format))
	if err != nil {
		return err
	}
	if target != "" {
		if err := conn.UpdateSession(cmd.Context(), sessionID, target, nil, ""); err != nil {
			return err
		}
	}
	refreshItemsFromLocalAPI(cmd.Context(), flags)

	keys := connectorImportKeys(items)
	if flags.asJSON || flags.agent || flags.csv || flags.plain {
		return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
			"file":     filePath,
			"via":      "connector",
			"session":  sessionID,
			"imported": len(items),
			"keys":     keys,
			"target":   target,
		}, flags)
	}
	if flags.quiet {
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Imported %d items from %s via Zotero desktop\n", len(items), filePath)
	return nil
}

func connectorImportContentType(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "bibtex", "ris":
		return "text/plain"
	case "csljson":
		return "application/json"
	default:
		return "text/plain"
	}
}

func connectorImportKeys(items []json.RawMessage) []string {
	keys := make([]string, 0, len(items))
	for _, raw := range items {
		var item struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &item); err == nil && strings.TrimSpace(item.Key) != "" {
			keys = append(keys, strings.TrimSpace(item.Key))
		}
	}
	return keys
}

func detectImportFileFormat(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".bib"):
		return "bibtex"
	case strings.HasSuffix(lower, ".ris"):
		return "ris"
	case strings.HasSuffix(lower, ".json"):
		return "csljson"
	default:
		return ""
	}
}

func parseImportFileItems(content, format, collection string) ([]map[string]any, error) {
	switch format {
	case "bibtex":
		return parseBibTeXItems(content, collection)
	case "ris":
		return parseRISItems(content, collection)
	case "csljson":
		var items []map[string]any
		if err := json.Unmarshal([]byte(content), &items); err != nil {
			return nil, fmt.Errorf("parsing CSL JSON: %w", err)
		}
		addImportCollectionToItems(items, collection)
		return items, nil
	default:
		return nil, fmt.Errorf("unknown format: use --format bibtex, ris, or csljson")
	}
}

func parseBibTeXItems(content, collection string) ([]map[string]any, error) {
	items := make([]map[string]any, 0)
	for offset := 0; offset < len(content); {
		loc := bibTeXEntryStartPattern.FindStringSubmatchIndex(content[offset:])
		if loc == nil {
			break
		}

		entryType := strings.ToLower(content[offset+loc[2] : offset+loc[3]])
		openBrace := offset + loc[1] - 1
		closeBrace, err := findMatchingBrace(content, openBrace)
		if err != nil {
			return nil, err
		}

		fields := parseBibTeXFields(content[openBrace+1 : closeBrace])
		item := bibTeXItemFromFields(entryType, fields)
		addImportCollection(item, collection)
		items = append(items, item)
		offset = closeBrace + 1
	}
	return items, nil
}

func findMatchingBrace(s string, open int) (int, error) {
	if open < 0 || open >= len(s) || s[open] != '{' {
		return -1, fmt.Errorf("invalid BibTeX entry")
	}
	depth := 0
	inQuote := false
	escaped := false
	for i := open; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return -1, fmt.Errorf("unterminated BibTeX entry")
}

func parseBibTeXFields(body string) map[string]string {
	parts := splitTopLevel(body, ',')
	fields := make(map[string]string)
	for _, part := range parts[1:] {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		fields[name] = cleanBibTeXValue(value)
	}
	return fields
}

func splitTopLevel(s string, sep byte) []string {
	parts := make([]string, 0)
	start := 0
	depth := 0
	inQuote := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote {
			switch ch {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 {
					depth--
				}
			}
		}
		if ch == sep && depth == 0 && !inQuote {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func cleanBibTeXValue(value string) string {
	value = strings.TrimSpace(value)
	for len(value) >= 2 {
		if value[0] == '{' && value[len(value)-1] == '}' {
			if close, err := findMatchingBrace(value, 0); err == nil && close == len(value)-1 {
				value = strings.TrimSpace(value[1 : len(value)-1])
				continue
			}
		}
		if value[0] == '"' && value[len(value)-1] == '"' {
			value = strings.TrimSpace(value[1 : len(value)-1])
			continue
		}
		break
	}
	value = strings.ReplaceAll(value, `\"`, `"`)
	value = strings.ReplaceAll(value, `\{`, `{`)
	value = strings.ReplaceAll(value, `\}`, `}`)
	return value
}

func bibTeXItemFromFields(entryType string, fields map[string]string) map[string]any {
	item := map[string]any{"itemType": bibTeXItemType(entryType)}
	setImportString(item, "title", fields["title"])
	if creators := parseImportCreators(fields["author"]); len(creators) > 0 {
		item["creators"] = creators
	}
	setImportString(item, "date", fields["year"])
	setImportString(item, "publicationTitle", fields["journal"])
	setImportString(item, "DOI", fields["doi"])
	setImportString(item, "ISBN", fields["isbn"])
	setImportString(item, "abstractNote", fields["abstract"])
	setImportString(item, "publisher", fields["publisher"])
	setImportString(item, "pages", fields["pages"])
	setImportString(item, "volume", fields["volume"])
	setImportString(item, "issue", fields["number"])
	setImportString(item, "url", fields["url"])
	return item
}

func bibTeXItemType(entryType string) string {
	switch entryType {
	case "article":
		return "journalArticle"
	case "book":
		return "book"
	case "incollection", "inbook":
		return "bookSection"
	case "inproceedings", "conference":
		return "conferencePaper"
	case "phdthesis", "mastersthesis":
		return "thesis"
	default:
		return "document"
	}
}

func parseRISItems(content, collection string) ([]map[string]any, error) {
	items := make([]map[string]any, 0)
	var current map[string]any
	var creators []map[string]any

	flush := func() {
		if current == nil {
			return
		}
		if len(creators) > 0 {
			current["creators"] = creators
		}
		addImportCollection(current, collection)
		items = append(items, current)
		current = nil
		creators = nil
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		tag, value, ok := parseRISLine(scanner.Text())
		if !ok {
			continue
		}
		if tag == "TY" {
			flush()
			current = map[string]any{"itemType": risItemType(value)}
			continue
		}
		if tag == "ER" {
			flush()
			continue
		}
		if current == nil {
			current = map[string]any{"itemType": "document"}
		}
		switch tag {
		case "TI", "T1":
			setImportString(current, "title", value)
		case "AU", "A1":
			creator := parseImportCreator(value)
			if len(creator) > 1 {
				creators = append(creators, creator)
			}
		case "PY", "Y1":
			setImportString(current, "date", risDate(value))
		case "JO", "JF", "T2":
			setImportString(current, "publicationTitle", value)
		case "DO":
			setImportString(current, "DOI", value)
		case "AB", "N2":
			setImportString(current, "abstractNote", value)
		case "UR":
			setImportString(current, "url", value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading RIS content: %w", err)
	}
	flush()
	return items, nil
}

func parseRISLine(line string) (string, string, bool) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "@"))
	if len(line) < 2 {
		return "", "", false
	}
	tag := strings.ToUpper(strings.TrimSpace(line[:2]))
	if tag == "" {
		return "", "", false
	}
	value := strings.TrimSpace(line[2:])
	value = strings.TrimSpace(strings.TrimPrefix(value, "-"))
	value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
	return tag, value, true
}

func risItemType(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "JOUR", "JFULL", "EJOUR":
		return "journalArticle"
	case "BOOK":
		return "book"
	case "CHAP":
		return "bookSection"
	case "CONF", "CPAPER":
		return "conferencePaper"
	case "THES":
		return "thesis"
	case "RPRT":
		return "report"
	case "ELEC", "WEB":
		return "webpage"
	default:
		return "document"
	}
}

func risDate(value string) string {
	value = strings.TrimSpace(value)
	if before, _, ok := strings.Cut(value, "/"); ok {
		return strings.TrimSpace(before)
	}
	return value
}

func parseImportCreators(raw string) []map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := bibTeXAuthorSplitPattern.Split(raw, -1)
	creators := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		creator := parseImportCreator(part)
		if len(creator) > 1 {
			creators = append(creators, creator)
		}
	}
	return creators
}

func parseImportCreator(raw string) map[string]any {
	raw = strings.TrimSpace(cleanBibTeXValue(raw))
	creator := map[string]any{"creatorType": "author"}
	if raw == "" {
		return creator
	}
	if strings.Contains(raw, ",") {
		parts := strings.SplitN(raw, ",", 2)
		setImportString(creator, "lastName", parts[0])
		setImportString(creator, "firstName", parts[1])
		return creator
	}
	parts := strings.Fields(raw)
	if len(parts) == 1 {
		creator["lastName"] = parts[0]
		return creator
	}
	creator["firstName"] = strings.Join(parts[:len(parts)-1], " ")
	creator["lastName"] = parts[len(parts)-1]
	return creator
}

func setImportString(item map[string]any, field, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		item[field] = value
	}
}

func addImportCollectionToItems(items []map[string]any, collection string) {
	for _, item := range items {
		addImportCollection(item, collection)
	}
}

func addImportCollection(item map[string]any, collection string) {
	collection = strings.TrimSpace(collection)
	if collection == "" {
		return
	}
	switch rawCollections := item["collections"].(type) {
	case []any:
		for _, rawCollection := range rawCollections {
			if existing, ok := rawCollection.(string); ok && existing == collection {
				return
			}
		}
		item["collections"] = append(rawCollections, collection)
	case []string:
		for _, existing := range rawCollections {
			if existing == collection {
				return
			}
		}
		item["collections"] = append(rawCollections, collection)
	default:
		item["collections"] = []string{collection}
	}
}
