// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero-aware local query planner. Replays the scoping
// semantics of the Zotero item list/search endpoints (itemType, tag,
// collection, top-level, quick-search, sort, direction, limit, start) against
// the local resources table so `--data-source local` returns the same key sets
// and ordering as live reads. Also builds a curated FTS search document for
// items instead of indexing the whole raw JSON blob.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
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

// QueryItemsContext runs a scoped query over synced items (resource_type = 'items')
// and returns the matching payloads in the requested order. An empty result is
// not an error — it mirrors a live list that matched nothing. It retries
// transient SQLITE_BUSY/LOCKED errors with the caller's context, honoring
// cancellation and the bounded retry window instead of running to
// migrationLockTimeout on context.Background().
func (s *Store) QueryItemsContext(ctx context.Context, q ItemQuery) ([]json.RawMessage, error) {
	var sb strings.Builder
	var args []any

	sb.WriteString("SELECT r.data FROM resources r")
	useFTS := strings.TrimSpace(q.Query) != ""
	if useFTS {
		sb.WriteString(" JOIN resources_fts f ON r.id = f.id AND r.resource_type = f.resource_type")
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

	rows, err := s.queryWithBusyRetryContext(ctx, sb.String(), args...)
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

// TagQuery describes a scoped local tag-resource query. Tags are separate
// synced resources, while collection membership lives on item rows.
type TagQuery struct {
	Name       string // exact tag name, used by /tags/{name}
	Query      string // literal tag-name filter
	QueryMode  string // "contains" | "startsWith"
	Collection string // collection key whose item tags are returned
	Limit      int    // 0 = no limit
	Start      int    // pagination offset
}

// QueryTagsContext reproduces /tags and /collections/{key}/tags against the
// synced store. It returns the original tag resource payloads so meta.type and
// meta.numItems keep the same shape as live reads.
func (s *Store) QueryTagsContext(ctx context.Context, q TagQuery) ([]json.RawMessage, error) {
	mode := strings.TrimSpace(q.QueryMode)
	if mode == "" {
		mode = "contains"
	}
	if mode != "contains" && mode != "startsWith" {
		return nil, fmt.Errorf("unsupported tag query mode %q", q.QueryMode)
	}

	const tagName = `COALESCE(
		json_extract(t.data, '$.tag'),
		json_extract(t.data, '$.data.tag'),
		t.id
	)`
	const tagType = `COALESCE(
		json_extract(t.data, '$.meta.type'),
		json_extract(t.data, '$.type'),
		json_extract(t.data, '$.data.type'),
		0
	)`

	var sb strings.Builder
	args := make([]any, 0, 5)
	sb.WriteString(`SELECT t.data
FROM resources t
WHERE t.resource_type = 'tags'`)

	if q.Name != "" {
		sb.WriteString("\nAND " + tagName + " = ?")
		args = append(args, q.Name)
	} else if q.Query != "" {
		pattern := escapeTagLikeLiteral(q.Query)
		if mode == "startsWith" {
			pattern += "%"
		} else {
			pattern = "%" + pattern + "%"
		}
		sb.WriteString("\nAND " + tagName + ` LIKE ? ESCAPE '\' COLLATE NOCASE`)
		args = append(args, pattern)
	}

	if q.Collection != "" {
		sb.WriteString(`
AND EXISTS (
	SELECT 1
	FROM resources i
	JOIN json_each(i.data, '$.data.collections') c
	JOIN json_each(i.data, '$.data.tags') it
	WHERE i.resource_type = 'items'
		AND c.value = ?
		AND json_extract(it.value, '$.tag') = ` + tagName + `
		AND COALESCE(json_extract(it.value, '$.type'), 0) = ` + tagType + `
)`)
		args = append(args, q.Collection)
	}

	sb.WriteString("\nORDER BY " + tagName + " COLLATE NOCASE ASC, " + tagName + " ASC, " + tagType + " ASC, t.id ASC")
	if q.Limit > 0 {
		sb.WriteString("\nLIMIT ?")
		args = append(args, q.Limit)
		if q.Start > 0 {
			sb.WriteString(" OFFSET ?")
			args = append(args, q.Start)
		}
	} else if q.Start > 0 {
		sb.WriteString("\nLIMIT -1 OFFSET ?")
		args = append(args, q.Start)
	}

	rows, err := s.queryWithBusyRetryContext(ctx, sb.String(), args...)
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

func escapeTagLikeLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

// TrashQuery describes pagination for the Zotero trash item list.
type TrashQuery struct {
	Limit int // 0 = no limit
	Start int // pagination offset
}

// QueryTrashContext returns only synced trash item payloads in Zotero's default
// order: dateModified descending, then key ascending for deterministic ties.
// Zotero payloads normally nest fields under data, while older/local payloads
// may be flat, so the ordering expression supports both shapes.
func (s *Store) QueryTrashContext(ctx context.Context, q TrashQuery) ([]json.RawMessage, error) {
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

	rows, err := s.queryWithBusyRetryContext(ctx, sb.String(), args...)
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

// QuerySimilarityCandidatesContext returns top-level bibliographic items, excluding
// child-only item types and any key that also exists in the trash mirror.
func (s *Store) QuerySimilarityCandidatesContext(ctx context.Context) ([]SimilarityCandidate, error) {
	rows, err := s.queryWithBusyRetryContext(ctx, `
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

// VisitSimilarityFulltextDocumentsContext streams synced fulltext documents in parent
// order. Streaming lets callers make bounded-memory passes over a large corpus.
// Documents without a known parent and documents under trashed parents are
// excluded.
func (s *Store) VisitSimilarityFulltextDocumentsContext(ctx context.Context, visit func(SimilarityFulltextDocument) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.queryWithBusyRetryContext(ctx, `
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

const maxFulltextSnippetBytes = 4096

// FulltextSearchResult identifies one matching attachment and its parent
// bibliographic item. Snippet contains a bounded excerpt from synced PDF text.
type FulltextSearchResult struct {
	ItemKey       string `json:"item_key"`
	AttachmentKey string `json:"attachment_key"`
	Title         string `json:"title"`
	Snippet       string `json:"snippet"`
}

// SearchFulltextContext searches only synced attachment full text and resolves
// every hit to its parent bibliographic item. Unresolved and trashed parents are
// excluded because they cannot produce an actionable library result.
func (s *Store) SearchFulltextContext(ctx context.Context, query string, limit int) ([]FulltextSearchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit == 0 {
		limit = 50
	}
	rows, err := s.queryWithBusyRetryContext(ctx, `
SELECT
	parent.id,
	attachment.id,
	COALESCE(json_extract(parent.data, '$.data.title'), ''),
	substr(snippet(resources_fts, 2, '', '', ' … ', 24), 1, 4096)
FROM resources_fts
JOIN resources attachment
	ON attachment.resource_type = 'items'
	AND attachment.id = resources_fts.id
JOIN resources parent
	ON parent.resource_type = 'items'
	AND parent.id = attachment.parent_key
WHERE resources_fts MATCH ?
	AND resources_fts.resource_type = 'fulltext'
	AND COALESCE(parent.item_type, '') NOT IN ('attachment', 'note', 'annotation')
	AND NOT EXISTS (
		SELECT 1 FROM resources trashed
		WHERE trashed.resource_type = 'items-trash'
			AND trashed.id = parent.id
	)
ORDER BY rank, parent.id, attachment.id
LIMIT ?`, "content : ("+ftsMatchQuery(query)+")", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]FulltextSearchResult, 0)
	for rows.Next() {
		var result FulltextSearchResult
		if err := rows.Scan(&result.ItemKey, &result.AttachmentKey, &result.Title, &result.Snippet); err != nil {
			return nil, err
		}
		result.Snippet = truncateFulltextSnippet(result.Snippet)
		results = append(results, result)
	}
	return results, rows.Err()
}

func truncateFulltextSnippet(snippet string) string {
	if len(snippet) <= maxFulltextSnippetBytes {
		return snippet
	}
	const suffix = " …"
	cut := maxFulltextSnippetBytes - len(suffix)
	for cut > 0 && !utf8.ValidString(snippet[:cut]) {
		cut--
	}
	return snippet[:cut] + suffix
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
// Items use a curated Zotero-aware document. Fulltext rows index only the PDF
// body, never their JSON keys. Other resource types keep the raw JSON behavior.
func buildSearchDocument(resourceType string, data json.RawMessage) string {
	var obj map[string]any
	if resourceType == "fulltext" {
		if err := json.Unmarshal(data, &obj); err != nil {
			return ""
		}
		return fulltextSearchContent(obj)
	}
	if resourceType != "items" {
		return string(data)
	}
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

func fulltextSearchContent(obj map[string]any) string {
	fields := obj
	if inner, ok := obj["data"].(map[string]any); ok {
		fields = inner
	}
	content, _ := fields["content"].(string)
	return content
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
		switch {
		case !tok.quote && (op == "AND" || op == "OR"):
			if !expectOperand {
				out = append(out, op)
				expectOperand = true
			}
		case !tok.quote && op == "NOT":
			if !expectOperand {
				out = append(out, "AND")
			}
			out = append(out, "NOT")
			expectOperand = true
		default:
			// Quoting is the user's explicit request for a literal term, so a
			// quoted AND/OR/NOT is data, not syntax: it must fall through to
			// the literal-term case even though its uppercased text matches
			// an operator keyword when unquoted.
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
// genuinely narrows local results. The limit contract is Search's: 0 applies the
// interactive default of 50, and a negative limit means no limit (SQLite
// LIMIT -1) so a caller can enumerate the full match cohort for one type.
func (s *Store) SearchByType(query, resourceType string, limit int) ([]json.RawMessage, error) {
	if limit == 0 {
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

// titleCandidateMaxTerms bounds the OR expression built from a title. A very
// long title would otherwise produce a MATCH clause with one clause per word,
// and past roughly two dozen terms the extra words add candidates without
// changing which candidate ranks first.
const titleCandidateMaxTerms = 24

// TitleCandidates returns top-level items whose indexed document shares any
// word with the supplied title, best match first, with each ITEM returned
// once.
//
// The terms are joined with OR, not FTS5's implicit AND, because the caller is
// looking for a title the user may have mistyped: under AND a single wrong word
// eliminates the very item being looked for. OR keeps recall, and bm25 (SQLite
// exposes it as `rank`) supplies the precision by weighting rare words above
// common ones — which is what stops a query for a short generic title from
// ranking every item that happens to contain "the".
//
// The match is grouped by item id before it reaches `resources`, because the
// index can hold more than one document for one item and a plain join then
// emits the item once per document. That is not hypothetical: a library synced
// by an older binary keeps its FTS row under that binary's rowid scheme, the
// current writer inserts a second row under the current scheme, and the join
// returned the same item twice — same key, same title, same score — which
// spent two of the caller's five suggestion slots on one suggestion. Grouping
// also makes LIMIT mean "this many items", which is what the caller's cap is
// counted against. min(rank) keeps the best-scoring document of the group:
// bm25 is negative, so the smallest value is the strongest match, which is why
// the outer ORDER BY is ascending.
//
// This is candidate generation only. The caller re-ranks these rows by actual
// title similarity; the document indexed here spans creators, tags and
// identifiers as well as the title, so bm25 order alone is not title order.
func (s *Store) TitleCandidates(ctx context.Context, title string, limit int) ([]json.RawMessage, error) {
	match := titleCandidateMatchQuery(title)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.queryWithBusyRetryContext(ctx,
		`SELECT r.data FROM resources r
		 JOIN (
		   SELECT f.id AS item_id, min(f.rank) AS best_rank
		   FROM resources_fts f
		   WHERE resources_fts MATCH ? AND f.resource_type = 'items'
		   GROUP BY f.id
		 ) m ON m.item_id = r.id
		 WHERE r.resource_type = 'items'
		 AND (r.parent_key IS NULL OR r.parent_key = '')
		 ORDER BY m.best_rank
		 LIMIT ?`,
		match, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]json.RawMessage, 0, limit)
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, json.RawMessage(data))
	}
	return results, rows.Err()
}

// titleCandidateMatchQuery turns a title into a quoted OR expression, or "" for
// a title with no usable term — strings.Join over no terms already returns "",
// which is the empty-match sentinel TitleCandidates checks. Every term is
// quoted so punctuation inside a title cannot become MATCH syntax. Single
// characters are dropped: they match a large share of any library through the
// porter tokenizer while telling nothing about which item is meant.
func titleCandidateMatchQuery(title string) string {
	terms := make([]string, 0, titleCandidateMaxTerms)
	for _, field := range strings.FieldsFunc(title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(field)) < 2 {
			continue
		}
		terms = append(terms, quoteFTSTerm(field))
		if len(terms) == titleCandidateMaxTerms {
			break
		}
	}
	return strings.Join(terms, " OR ")
}
