// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero-aware local query planner. Replays the scoping
// semantics of the Zotero item list/search endpoints (itemType, tag,
// collection, top-level, quick-search, sort, direction, limit, start) against
// the local resources table so `--data-source local` returns the same key sets
// and ordering as live reads. Also builds a curated FTS search document for
// items instead of indexing the whole raw JSON blob.

package store

import (
	"encoding/json"
	"strings"
)

// ItemQuery describes a scoped local item query mirroring the parameters of the
// Zotero item list endpoints.
type ItemQuery struct {
	ItemType   string // data.itemType filter (indexed column)
	Tag        string // data.tags[].tag membership
	Collection string // data.collections[] membership (collection key)
	TopOnly    bool   // exclude child items (data.parentItem present)
	Parent     string // data.parentItem == this key (children of an item)
	Query      string // quick search routed through FTS
	Sort       string // Zotero sort field name
	Direction  string // "asc" | "desc"
	Limit      int    // 0 = no limit
	Start      int    // pagination offset
}

// itemSortColumns maps Zotero sort field names to a SQL ORDER BY expression
// over the stored item payload. Unmapped fields fall back to the item key.
var itemSortColumns = map[string]string{
	"title":               "json_extract(r.data, '$.data.title')",
	"date":                "json_extract(r.data, '$.data.date')",
	"dateAdded":           "json_extract(r.data, '$.data.dateAdded')",
	"dateModified":        "json_extract(r.data, '$.data.dateModified')",
	"creator":             "json_extract(r.data, '$.data.creators[0].lastName')",
	"type":                "r.item_type",
	"itemType":            "r.item_type",
	"publisher":           "json_extract(r.data, '$.data.publisher')",
	"publicationTitle":    "json_extract(r.data, '$.data.publicationTitle')",
	"journalAbbreviation": "json_extract(r.data, '$.data.journalAbbreviation')",
}

// QueryItems runs a scoped query over synced items (resource_type = 'items')
// and returns the matching payloads in the requested order. An empty result is
// not an error — it mirrors a live list that matched nothing.
func (s *Store) QueryItems(q ItemQuery) ([]json.RawMessage, error) {
	var sb strings.Builder
	var args []any

	sb.WriteString("SELECT r.data FROM resources r")
	useFTS := strings.TrimSpace(q.Query) != ""
	if useFTS {
		sb.WriteString(" JOIN resources_fts f ON r.id = f.id")
	}
	sb.WriteString(" WHERE r.resource_type = 'items'")

	if useFTS {
		sb.WriteString(" AND resources_fts MATCH ?")
		args = append(args, ftsMatchQuery(q.Query))
	}
	if q.ItemType != "" {
		sb.WriteString(" AND r.item_type = ?")
		args = append(args, q.ItemType)
	}
	if q.Tag != "" {
		sb.WriteString(" AND EXISTS (SELECT 1 FROM json_each(r.data, '$.data.tags') te WHERE json_extract(te.value, '$.tag') = ?)")
		args = append(args, q.Tag)
	}
	if q.Collection != "" {
		sb.WriteString(" AND EXISTS (SELECT 1 FROM json_each(r.data, '$.data.collections') ce WHERE ce.value = ?)")
		args = append(args, q.Collection)
	}
	if q.TopOnly {
		sb.WriteString(" AND json_extract(r.data, '$.data.parentItem') IS NULL")
	}
	if q.Parent != "" {
		sb.WriteString(" AND json_extract(r.data, '$.data.parentItem') = ?")
		args = append(args, q.Parent)
	}

	sb.WriteString(" ORDER BY ")
	sb.WriteString(itemOrderBy(q.Sort, q.Direction))

	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
		if q.Start > 0 {
			sb.WriteString(" OFFSET ?")
			args = append(args, q.Start)
		}
	} else if q.Start > 0 {
		sb.WriteString(" LIMIT -1 OFFSET ?")
		args = append(args, q.Start)
	}

	rows, err := s.db.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []json.RawMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// TrashQuery describes pagination for the Zotero trash item list.
type TrashQuery struct {
	Limit int // 0 = no limit
	Start int // pagination offset
}

// QueryTrash returns only synced trash item payloads in Zotero's default
// order: dateModified descending, then key ascending for deterministic ties.
// Zotero payloads normally nest fields under data, while older/local payloads
// may be flat, so the ordering expression supports both shapes.
func (s *Store) QueryTrash(q TrashQuery) ([]json.RawMessage, error) {
	var sb strings.Builder
	args := make([]any, 0, 2)
	sb.WriteString(`
SELECT r.data
FROM resources r
WHERE r.resource_type = 'items-trash'
ORDER BY COALESCE(
	json_extract(r.data, '$.data.dateModified'),
	json_extract(r.data, '$.dateModified')
) DESC, r.id ASC`)

	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
		if q.Start > 0 {
			sb.WriteString(" OFFSET ?")
			args = append(args, q.Start)
		}
	} else if q.Start > 0 {
		sb.WriteString(" LIMIT -1 OFFSET ?")
		args = append(args, q.Start)
	}

	rows, err := s.db.Query(sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []json.RawMessage
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// SimilarityCandidate is a top-level bibliographic item eligible for local
// similarity scoring.
type SimilarityCandidate struct {
	Key  string
	Data json.RawMessage
}

// QuerySimilarityCandidates returns top-level bibliographic items, excluding
// child-only item types and any key that also exists in the trash mirror.
func (s *Store) QuerySimilarityCandidates() ([]SimilarityCandidate, error) {
	rows, err := s.db.Query(`
SELECT r.id, r.data
FROM resources r
WHERE r.resource_type = 'items'
	AND COALESCE(r.parent_key, '') = ''
	AND COALESCE(r.item_type, '') NOT IN ('attachment', 'note', 'annotation')
	AND NOT EXISTS (
		SELECT 1 FROM resources t
		WHERE t.resource_type = 'items-trash' AND t.id = r.id
	)
ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SimilarityCandidate
	for rows.Next() {
		var candidate SimilarityCandidate
		var data string
		if err := rows.Scan(&candidate.Key, &data); err != nil {
			return nil, err
		}
		candidate.Data = json.RawMessage(data)
		results = append(results, candidate)
	}
	return results, rows.Err()
}

// SimilarityFulltextDocument is one synced attachment fulltext payload and its
// parent bibliographic item.
type SimilarityFulltextDocument struct {
	AttachmentKey string
	ParentItemKey string
	Data          json.RawMessage
}

// VisitSimilarityFulltextDocuments streams synced fulltext documents in parent
// order. Streaming lets callers make bounded-memory passes over a large corpus.
// Documents without a known parent and documents under trashed parents are
// excluded.
func (s *Store) VisitSimilarityFulltextDocuments(visit func(SimilarityFulltextDocument) error) error {
	rows, err := s.db.Query(`
SELECT ft.id, att.parent_key, ft.data
FROM resources ft
JOIN resources att ON att.resource_type = 'items' AND att.id = ft.id
WHERE ft.resource_type = 'fulltext'
	AND COALESCE(att.parent_key, '') <> ''
	AND NOT EXISTS (
		SELECT 1 FROM resources t
		WHERE t.resource_type = 'items-trash' AND t.id = att.parent_key
	)
ORDER BY att.parent_key, ft.id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var doc SimilarityFulltextDocument
		var data string
		if err := rows.Scan(&doc.AttachmentKey, &doc.ParentItemKey, &data); err != nil {
			return err
		}
		doc.Data = json.RawMessage(data)
		if err := visit(doc); err != nil {
			return err
		}
	}
	return rows.Err()
}

// itemOrderBy builds the ORDER BY clause for a sort field + direction, always
// appending the item key as a deterministic tiebreaker so ordering is stable.
func itemOrderBy(sortField, direction string) string {
	dir := "ASC"
	if strings.EqualFold(direction, "desc") {
		dir = "DESC"
	}
	expr, ok := itemSortColumns[sortField]
	if !ok {
		// No (or unknown) sort field: order by the most recent modification,
		// matching the Zotero default of dateModified-descending, then key.
		return "json_extract(r.data, '$.data.dateModified') DESC, r.id ASC"
	}
	return expr + " " + dir + ", r.id ASC"
}

// itemSearchFields are the curated string fields fed into the FTS search
// document for items, in addition to creators and tags.
var itemSearchFields = []string{
	"title", "shortTitle", "abstractNote", "note",
	"publicationTitle", "bookTitle", "proceedingsTitle", "conferenceName",
	"publisher", "journalAbbreviation", "series",
	"date", "language", "DOI", "ISBN", "ISSN", "url",
	"itemType", "annotationText", "annotationComment", "annotationLabel",
}

// buildSearchDocument returns the text indexed into resources_fts for a record.
// For items it builds a curated Zotero-aware document (key, bibliographic
// fields, creator names, tag names, annotation text) so search matches real
// content rather than JSON structure. For every other resource type it keeps
// the previous behavior of indexing the raw JSON. Falls back to the raw JSON
// whenever the curated document would be empty so a row is never unindexed.
func buildSearchDocument(resourceType string, data json.RawMessage) string {
	if resourceType != "items" {
		return string(data)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return string(data)
	}
	fields := obj
	if inner, ok := obj["data"].(map[string]any); ok {
		fields = inner
	}

	var parts []string
	add := func(v any) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}

	add(obj["key"])
	for _, f := range itemSearchFields {
		add(fields[f])
	}
	if creators, ok := fields["creators"].([]any); ok {
		for _, c := range creators {
			if cm, ok := c.(map[string]any); ok {
				add(cm["firstName"])
				add(cm["lastName"])
				add(cm["name"])
			}
		}
	}
	if tags, ok := fields["tags"].([]any); ok {
		for _, t := range tags {
			if tm, ok := t.(map[string]any); ok {
				add(tm["tag"])
			}
		}
	}

	doc := strings.Join(parts, " ")
	if strings.TrimSpace(doc) == "" {
		return string(data)
	}
	return doc
}

// ftsMatchQuery turns a user quick-search string into a bounded FTS5 MATCH
// expression. It preserves documented boolean operators instead of quoting them
// as literal tokens, while still quoting ordinary terms so punctuation in
// titles/DOIs cannot become syntax.
func ftsMatchQuery(query string) string {
	tokens := scanFTSQuery(query)
	expr := normalizeFTSQuery(tokens)
	if expr != "" {
		return expr
	}
	return quoteAllFTSTerms(query)
}

type ftsQueryToken struct {
	text   string
	quote  bool
	symbol bool
}

func scanFTSQuery(query string) []ftsQueryToken {
	var tokens []ftsQueryToken
	for i := 0; i < len(query); {
		if isFTSSpace(query[i]) {
			i++
			continue
		}
		switch query[i] {
		case '(', ')':
			tokens = append(tokens, ftsQueryToken{text: query[i : i+1], symbol: true})
			i++
		case '"':
			i++
			start := i
			for i < len(query) && query[i] != '"' {
				i++
			}
			if start < i {
				tokens = append(tokens, ftsQueryToken{text: query[start:i], quote: true})
			}
			if i < len(query) && query[i] == '"' {
				i++
			}
		default:
			start := i
			for i < len(query) && !isFTSSpace(query[i]) && query[i] != '(' && query[i] != ')' {
				i++
			}
			if start < i {
				tokens = append(tokens, ftsQueryToken{text: query[start:i]})
			}
		}
	}
	return tokens
}

func normalizeFTSQuery(tokens []ftsQueryToken) string {
	out := make([]string, 0, len(tokens))
	expectOperand := true
	depth := 0
	for _, tok := range tokens {
		text := strings.TrimSpace(tok.text)
		if text == "" {
			continue
		}
		if tok.symbol {
			switch text {
			case "(":
				if !expectOperand {
					out = append(out, "AND")
				}
				out = append(out, "(")
				depth++
				expectOperand = true
			case ")":
				if depth > 0 && !expectOperand {
					out = append(out, ")")
					depth--
					expectOperand = false
				}
			}
			continue
		}
		op := strings.ToUpper(text)
		switch op {
		case "AND", "OR":
			if !expectOperand {
				out = append(out, op)
				expectOperand = true
			}
		case "NOT":
			if !expectOperand {
				out = append(out, "AND")
			}
			out = append(out, "NOT")
			expectOperand = true
		default:
			out = append(out, quoteFTSTerm(text))
			expectOperand = false
		}
	}
	for len(out) > 0 && trailingFTSOperator(out[len(out)-1]) {
		if out[len(out)-1] == "(" {
			depth--
		}
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return ""
	}
	for depth > 0 {
		out = append(out, ")")
		depth--
	}
	return strings.Join(out, " ")
}

func trailingFTSOperator(token string) bool {
	switch token {
	case "AND", "OR", "NOT", "(":
		return true
	default:
		return false
	}
}

func quoteAllFTSTerms(query string) string {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return query
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, quoteFTSTerm(term))
	}
	return strings.Join(quoted, " ")
}

func quoteFTSTerm(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

func isFTSSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', '\f', '\v':
		return true
	default:
		return false
	}
}

// SearchByType runs an FTS search scoped to a single resource type. It mirrors
// Search but adds a resource_type predicate so the search command's --type flag
// genuinely narrows local results.
func (s *Store) SearchByType(query, resourceType string, limit int) ([]json.RawMessage, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.queryWithBusyRetry(
		`SELECT r.data FROM resources r
		 JOIN resources_fts f ON r.id = f.id AND r.resource_type = f.resource_type
		 WHERE resources_fts MATCH ? AND f.resource_type = ?
		 ORDER BY rank
		 LIMIT ?`,
		ftsMatchQuery(query), resourceType, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]json.RawMessage, 0)
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}
