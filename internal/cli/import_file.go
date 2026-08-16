// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

const importFileBatchSize = 50

func importFileFailureIndexes(failures map[string]batchWriteFailure, offset int) map[string]batchWriteFailure {
	if offset == 0 || len(failures) == 0 {
		return failures
	}

	offsetFailures := make(map[string]batchWriteFailure, len(failures))
	for index, failure := range failures {
		itemIndex, err := strconv.Atoi(index)
		if err != nil {
			offsetFailures[index] = failure
			continue
		}
		offsetFailures[strconv.Itoa(itemIndex+offset)] = failure
	}
	return offsetFailures
}

var bibTeXEntryStartPattern = regexp.MustCompile(`(?is)@([a-zA-Z]+)\s*\{`)
var bibTeXAuthorSplitPattern = regexp.MustCompile(`(?i)\s+and\s+`)

func newImportFileCmd(flags *rootFlags) *cobra.Command {
	var flagFormat string
	var flagCollection string

	cmd := &cobra.Command{
		Use:   "file <path>",
		Short: "Import items from BibTeX, RIS, or CSL JSON",
		Long: `Import items from a BibTeX, RIS, or CSL JSON file.

The import previews by default and writes only under --yes; --dry-run always
wins over --yes. Every parsed record counts against --max-changes.

Records are posted in batches, so a record Zotero rejects cannot un-submit the
records sent alongside it. Every record therefore reports its own outcome
instead of the run stopping at the first rejection.`,
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
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

			batch := newImportFileBatch(flags, items)
			ops := make([]mutation.Op, 0, len(items))
			for i := range items {
				index := i
				ops = append(ops, mutation.Op{
					ID:      fmt.Sprintf("import.file:%03d", index+1),
					Key:     importFileRecordKey(items[index], index),
					Kind:    "item_create",
					Changes: []mutation.Change{{Field: "item", Add: items[index]}},
					Apply: func() (string, any, error) {
						return batch.apply(index)
					},
				})
			}

			// Records travel to Zotero in batches, so stopping at the first
			// rejection would report already-submitted records as unattempted
			// and invite a duplicating re-import. Every record reports instead.
			env, runErr := runMutation(cmd.Context(), flags, "import.file", ops, func(o *mutation.Options) {
				o.ContinueOnError = true
			})
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if runErr != nil && env.Result != nil && env.Result.Summary.Failed > 0 {
				// Zotero answers a batched write with HTTP 200 even when it
				// rejected elements; that stays exit 13, not a hard failure.
				return degradedErr(runErr)
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&flagFormat, "format", "", "Input format (bibtex, ris, csljson; csljson requires --via connector)")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add imported items to")

	return cmd
}

// importFileRecordKey labels an operation with the record's title so a preview
// is readable; parsed records have no Zotero key yet.
func importFileRecordKey(item map[string]any, index int) string {
	if title, ok := item["title"].(string); ok && strings.TrimSpace(title) != "" {
		return strings.TrimSpace(title)
	}
	return fmt.Sprintf("record %d", index+1)
}

// importFileBatch posts parsed records in Zotero's batch size while the mutation
// engine still reports one result per record: the first record of a batch issues
// the request, and the rest read their outcome from the cached response. Zotero
// reports per-element rejections inside an HTTP 200, so a rejected record is
// mapped back onto exactly the record that produced it.
type importFileBatch struct {
	flags    *rootFlags
	items    []map[string]any
	client   importApplyPoster
	executed map[int]bool
	failed   map[int]batchWriteFailure
	fatal    map[int]error
}

func newImportFileBatch(flags *rootFlags, items []map[string]any) *importFileBatch {
	return &importFileBatch{
		flags:    flags,
		items:    items,
		executed: make(map[int]bool),
		failed:   make(map[int]batchWriteFailure),
		fatal:    make(map[int]error),
	}
}

func (b *importFileBatch) apply(index int) (string, any, error) {
	start := index - index%importFileBatchSize
	if !b.executed[start] {
		b.executed[start] = true
		b.runBatch(start)
	}
	if err := b.fatal[index]; err != nil {
		return "failed", nil, err
	}
	if failure, ok := b.failed[index]; ok {
		return "failed", fmt.Sprintf("index %d: code %d: %s", index, failure.Code, failure.Message), nil
	}
	return "applied", nil, nil
}

func (b *importFileBatch) runBatch(start int) {
	end := min(start+importFileBatchSize, len(b.items))
	if b.client == nil {
		c, err := b.flags.newClient()
		if err != nil {
			b.failRange(start, end, err)
			return
		}
		b.client = c
	}
	data, _, err := b.client.Post("/items", b.items[start:end])
	if err != nil {
		b.failRange(start, end, classifyAPIError(err, b.flags))
		return
	}
	for key, failure := range importFileFailureIndexes(decodeBatchWriteResponse(data).Failed, start) {
		index, convErr := strconv.Atoi(key)
		if convErr != nil {
			// Zotero returned a non-numeric element index; charge it to the
			// batch's first record rather than dropping the rejection.
			b.failed[start] = batchWriteFailure{Code: failure.Code, Message: fmt.Sprintf("index %s: %s", key, failure.Message)}
			continue
		}
		b.failed[index] = failure
	}
}

func (b *importFileBatch) failRange(start, end int, err error) {
	for i := start; i < end; i++ {
		b.fatal[i] = err
	}
}

// importFileViaConnector routes the file through the Zotero desktop translator.
// The connector translates the whole file in one session, so every locally
// counted record becomes an operation for previewing and --max-changes, and the
// first applied operation runs the session the rest report against.
func importFileViaConnector(cmd *cobra.Command, flags *rootFlags, filePath string, content []byte, format, collectionKey string) error {
	records := countImportFileRecords(string(content), format)
	if records == 0 {
		return fmt.Errorf("no items found in %s", filePath)
	}
	session := &importFileConnectorSession{
		flags:         flags,
		content:       content,
		format:        format,
		collectionKey: collectionKey,
		records:       records,
	}
	ops := make([]mutation.Op, 0, records)
	for index := range records {
		ops = append(ops, mutation.Op{
			ID:   fmt.Sprintf("import.file:connector:%03d", index+1),
			Key:  fmt.Sprintf("record %d", index+1),
			Kind: "item_create",
			Changes: []mutation.Change{{Field: "item", Add: map[string]any{
				"file":   filePath,
				"via":    "connector",
				"record": index + 1,
			}}},
			Apply: func() (string, any, error) {
				return session.apply(cmd, index)
			},
		})
	}

	env, runErr := runMutation(cmd.Context(), flags, "import.file", ops)
	if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
		return renderErr
	}
	return runErr
}

// importFileConnectorSession performs the one-shot desktop translator import on
// the first applied operation and replays its outcome to the remaining ones.
// s.imported is the actual translator count (len of Import response), which may
// be smaller than the locally counted record count when the translator rejects
// or merges records. s.records is that local count.
type importFileConnectorSession struct {
	flags         *rootFlags
	content       []byte
	format        string
	collectionKey string

	done      bool
	err       error
	sessionID string
	target    string
	keys      []string
	imported  int
	records   int
}

func (s *importFileConnectorSession) apply(cmd *cobra.Command, index int) (string, any, error) {
	if !s.done {
		s.done = true
		s.err = s.run(cmd)
	}
	if s.err != nil {
		// Post-create filing failure after Import committed: return the
		// populated result (session/keys/imported) alongside the error so the
		// caller can journal the create and avoid a duplicating retry — never a
		// zero value. Import-only failures have no session.
		if s.sessionID != "" {
			reason := map[string]any{
				"via":      "connector",
				"session":  s.sessionID,
				"imported": s.imported,
				"keys":     s.keys,
				"target":   s.target,
			}
			return "failed", reason, s.err
		}
		return "failed", nil, s.err
	}
	// Reconcile the per-op status against the actual imported count: the
	// translator may return fewer items than were parsed, so only the first
	// s.imported ops are truly applied.
	if index >= s.imported {
		reason := fmt.Sprintf("translator returned %d item(s) for %d parsed record(s); record %d was not imported", s.imported, s.records, index+1)
		return "skipped", reason, nil
	}
	if index > 0 {
		return "applied", map[string]any{"via": "connector", "session": s.sessionID}, nil
	}
	return "applied", map[string]any{
		"via":      "connector",
		"session":  s.sessionID,
		"imported": s.imported,
		"keys":     s.keys,
		"target":   s.target,
	}, nil
}

func (s *importFileConnectorSession) run(cmd *cobra.Command) error {
	flags := s.flags
	via, err := flags.resolveCreateVia(cmd.Context(), s.collectionKey != "" || strings.TrimSpace(flags.connectorTarget) != "")
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
	if target == "" && strings.TrimSpace(s.collectionKey) != "" {
		target, err = resolveConnectorTarget(cmd.Context(), flags, conn, s.collectionKey)
		if err != nil {
			return err
		}
	}
	sessionID, err := connector.NewID()
	if err != nil {
		return err
	}
	items, err := conn.Import(cmd.Context(), sessionID, s.content, connectorImportContentType(s.format))
	if err != nil {
		return err
	}
	// Populate partial-result fields BEFORE the filing step: if UpdateSession
	// fails after the items are already committed, the caller must receive the
	// populated result (session, keys, count) alongside the error so it can
	// journal the create and avoid a duplicating blind retry.
	s.sessionID = sessionID
	s.keys = connectorImportKeys(items)
	s.imported = len(items)
	s.target = target
	if target != "" {
		if err := conn.UpdateSession(cmd.Context(), sessionID, target, nil, ""); err != nil {
			// The items are already committed; refresh the mirror so local
			// reads can see them even though filing failed.
			refreshItemsFromLocalAPI(cmd.Context(), flags)
			return err
		}
	}
	refreshItemsFromLocalAPI(cmd.Context(), flags)

	return nil
}

// countImportFileRecords counts records without invoking the translator so the
// connector preview stays offline.
func countImportFileRecords(content, format string) int {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "bibtex":
		return len(bibTeXEntryStartPattern.FindAllStringIndex(content, -1))
	case "ris":
		count := 0
		scanner := bufio.NewScanner(strings.NewReader(content))
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			if tag, _, ok := parseRISLine(scanner.Text()); ok && tag == "TY" {
				count++
			}
		}
		return count
	case "csljson":
		var records []json.RawMessage
		if err := json.Unmarshal([]byte(content), &records); err != nil {
			return 0
		}
		return len(records)
	default:
		return 0
	}
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
		return nil, fmt.Errorf("CSL JSON import requires Zotero's translator; re-run with --via connector")
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
