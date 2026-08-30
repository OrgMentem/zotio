// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsFindCmd(flags *rootFlags) *cobra.Command {
	var query findItemsQuery
	cmd := &cobra.Command{
		Use:   "find",
		Short: "Find locally synced items by identifier, URL, or exact title",
		Example: `  zotio items find --doi 10.1145/3290605.3300709
  zotio items find --isbn 978-0-262-03384-8
  zotio items find --url https://example.org/paper
  zotio items find --openalex W2741809807
  zotio items find --title "Attention Is All You Need"
  zotio items find --citekey smith2023 --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			normalizedQuery, err := query.normalized()
			if err != nil {
				return err
			}
			if normalizedQuery.empty() {
				return fmt.Errorf("at least one of --doi, --arxiv, --isbn, --pmid, --citekey, --url, --openalex, or --title is required")
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first to enable item lookup.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{Store: rawDB}

			rows, err := queryFindItemsExact(cmd.Context(), db, normalizedQuery)
			if err != nil {
				return fmt.Errorf("querying local items: %w", err)
			}
			data, err := json.Marshal(extractItemDataRows(rows))
			if err != nil {
				return err
			}
			// Item lookup has no live API equivalent, so it always reads the
			// local mirror. Use the shared envelope to keep `.results` stable.
			prov := localProvenance(rawDB, "items", "local_only")
			printProvenance(cmd, countResultItems(data), prov)
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						return err
					}
					if len(items) >= 25 {
						fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --json --select or more lookup flags.\n", len(items))
					}
					return nil
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&query.DOI, "doi", "", "Find items with this DOI")
	cmd.Flags().StringVar(&query.ArXiv, "arxiv", "", "Find items with this arXiv ID or URL")
	cmd.Flags().StringVar(&query.ISBN, "isbn", "", "Find items with this ISBN")
	cmd.Flags().StringVar(&query.PMID, "pmid", "", "Find items with this PMID in Extra")
	cmd.Flags().StringVar(&query.Citekey, "citekey", "", "Find items with this Better BibTeX citation key")
	cmd.Flags().StringVar(&query.URL, "url", "", "Find items with this normalized URL")
	cmd.Flags().StringVar(&query.OpenAlex, "openalex", "", "Find items with this OpenAlex work ID or URL")
	cmd.Flags().StringVar(&query.Title, "title", "", "Find items with this exact title, ignoring case and surrounding whitespace")

	return cmd
}

type findItemsQuery struct {
	DOI      string
	ArXiv    string
	ISBN     string
	PMID     string
	Citekey  string
	URL      string
	OpenAlex string
	Title    string
}

func (q findItemsQuery) normalized() (findItemsQuery, error) {
	q.DOI = normalizeDOI(q.DOI)
	rawArXiv := strings.TrimSpace(q.ArXiv)
	q.ArXiv = normalizeFindArxivID(rawArXiv)
	if rawArXiv != "" && q.ArXiv == "" {
		return findItemsQuery{}, fmt.Errorf("--arxiv must be an ID such as 2401.00001 or an arxiv.org abs/pdf URL")
	}
	q.ISBN = strings.TrimSpace(q.ISBN)
	q.PMID = strings.TrimSpace(q.PMID)
	q.Citekey = strings.TrimSpace(q.Citekey)
	q.URL = normalizeFindURL(q.URL)
	rawOpenAlex := strings.TrimSpace(q.OpenAlex)
	q.OpenAlex = normalizeOpenAlexWorkID(rawOpenAlex)
	if rawOpenAlex != "" && q.OpenAlex == "" {
		return findItemsQuery{}, fmt.Errorf("--openalex must be a work ID such as W2741809807 or an openalex.org work URL")
	}
	q.Title = strings.TrimSpace(q.Title)
	return q, nil
}

func (q findItemsQuery) empty() bool {
	return q.DOI == "" && q.ArXiv == "" && q.ISBN == "" && q.PMID == "" &&
		q.Citekey == "" && q.URL == "" && q.OpenAlex == "" && q.Title == ""
}

func findItemsCandidateSQL(q findItemsQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 12)
	if q.DOI != "" {
		// DOI normalization happens in Go. Keep URL-form and bare DOI rows.
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.DOI'), '') <> ''`)
	}
	if q.ArXiv != "" {
		clauses = append(clauses, `(
			COALESCE(json_extract(data, '$.data.archiveID'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.url'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'
		)`)
		escaped := escapeSQLiteLikeLiteral(q.ArXiv)
		args = append(args, escaped, escaped, escaped)
	}
	if q.ISBN != "" {
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.ISBN'), '') <> ''`)
	}
	if q.PMID != "" {
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'`)
		args = append(args, escapeSQLiteLikeLiteral(q.PMID))
	}
	if q.Citekey != "" {
		clauses = append(clauses, `(
			COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.citationKey'), '') <> ''
		)`)
		args = append(args, escapeSQLiteLikeLiteral(q.Citekey))
	}
	if q.URL != "" {
		// URL normalization happens in Go. Keep every URL-bearing candidate
		// because equivalent URLs can differ in host case, fragment, or slash.
		clauses = append(clauses, `TRIM(COALESCE(json_extract(data, '$.data.url'), '')) <> ''`)
	}
	if q.OpenAlex != "" {
		clauses = append(clauses, `(
			COALESCE(json_extract(data, '$.data.url'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.archiveID'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.repository'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.archiveLocation'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'
		)`)
		escaped := escapeSQLiteLikeLiteral(q.OpenAlex)
		args = append(args, escaped, escaped, escaped, escaped, escaped)
	}
	if q.Title != "" {
		// SQLite TRIM/LOWER is narrower than Go's Unicode-aware exact check.
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.title'), '') <> ''`)
	}
	return `
SELECT id, data
FROM resources
WHERE resource_type = 'items'
	AND (parent_key IS NULL OR parent_key = '')
	AND (` + strings.Join(clauses, "\n\t\tOR ") + `)
ORDER BY id`, args
}

// queryFindItemsExact streams broad normalization candidates under the command
// context and retains only exact matches. URL and title equivalence cannot be
// expressed safely with SQLite's narrower case and whitespace rules.
func queryFindItemsExact(ctx context.Context, db localQueryStore, query findItemsQuery) ([]map[string]any, error) {
	sqlQuery, sqlArgs := findItemsCandidateSQL(query)
	cursor, err := db.QueryContext(ctx, sqlQuery, sqlArgs...)
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	matches := make([]map[string]any, 0)
	for cursor.Next() {
		var id, data string
		if err := cursor.Scan(&id, &data); err != nil {
			return nil, err
		}
		row := map[string]any{"id": id, "data": data}
		if findRowMatchesExact(row, query) {
			matches = append(matches, row)
		}
	}
	return matches, cursor.Err()
}

var findArxivIDInputPattern = regexp.MustCompile(`(?i)^([a-z-]+/[0-9]{7}|[0-9]{4}\.[0-9]{4,5})(?:v[0-9]+)?(?:\.pdf)?$`)

func normalizeFindArxivID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(value), "arxiv:") {
		value = strings.TrimSpace(value[len("arxiv:"):])
	} else if parsed, err := neturl.Parse(value); err == nil && parsed.Scheme != "" {
		scheme := strings.ToLower(parsed.Scheme)
		host := strings.ToLower(parsed.Hostname())
		if (scheme != "http" && scheme != "https") ||
			(host != "arxiv.org" && host != "www.arxiv.org" && host != "export.arxiv.org") {
			return ""
		}
		path := strings.Trim(parsed.Path, "/")
		lowerPath := strings.ToLower(path)
		switch {
		case strings.HasPrefix(lowerPath, "abs/"):
			value = path[len("abs/"):]
		case strings.HasPrefix(lowerPath, "pdf/"):
			value = path[len("pdf/"):]
		default:
			return ""
		}
	}
	matches := findArxivIDInputPattern.FindStringSubmatch(value)
	if len(matches) != 2 {
		return ""
	}
	return normalizeArxivID(matches[1])
}

func normalizeFindURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path == "/" && parsed.RawPath == "" {
		parsed.Path = ""
	} else if parsed.RawPath != "" {
		// RawPath distinguishes a literal terminal slash from an encoded %2F.
		if strings.HasSuffix(parsed.RawPath, "/") {
			parsed.Path = strings.TrimSuffix(parsed.Path, "/")
			parsed.RawPath = strings.TrimSuffix(parsed.RawPath, "/")
		}
	} else {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}
	return parsed.String()
}

func normalizeOpenAlexWorkID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := neturl.Parse(value); err == nil && strings.EqualFold(parsed.Hostname(), "openalex.org") {
		value = strings.Trim(parsed.Path, "/")
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"openalex id:", "openalex:"} {
		if strings.HasPrefix(lower, prefix) {
			value = strings.TrimSpace(value[len(prefix):])
			break
		}
	}
	value = strings.ToUpper(strings.Trim(value, " /"))
	if len(value) < 2 || value[0] != 'W' {
		return ""
	}
	for _, digit := range value[1:] {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	return value
}

func escapeSQLiteLikeLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

func extractItemDataRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		raw, ok := row["data"].(string)
		if !ok {
			out = append(out, row)
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(raw), &item) != nil {
			out = append(out, row)
			continue
		}
		out = append(out, item)
	}
	return out
}

func findRowMatchesExact(row map[string]any, query findItemsQuery) bool {
	raw, ok := row["data"].(string)
	if !ok {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return false
	}
	fields := decoded
	if inner, ok := decoded["data"].(map[string]any); ok {
		fields = inner
	}
	stringField := func(name string) string {
		value, _ := fields[name].(string)
		return value
	}
	d := struct {
		DOI             string
		ISBN            string
		ArchiveID       string
		ArchiveLocation string
		Repository      string
		CitationKey     string
		Title           string
		URL             string
		Extra           string
	}{
		DOI:             stringField("DOI"),
		ISBN:            stringField("ISBN"),
		ArchiveID:       stringField("archiveID"),
		ArchiveLocation: stringField("archiveLocation"),
		Repository:      stringField("repository"),
		CitationKey:     stringField("citationKey"),
		Title:           stringField("title"),
		URL:             stringField("url"),
		Extra:           stringField("extra"),
	}
	if query.DOI != "" && strings.EqualFold(normalizeDOI(d.DOI), query.DOI) {
		return true
	}
	if query.ISBN != "" && strings.TrimSpace(d.ISBN) == query.ISBN {
		return true
	}
	if query.ArXiv != "" {
		if normalizeFindArxivID(d.ArchiveID) == query.ArXiv ||
			normalizeFindArxivID(d.URL) == query.ArXiv ||
			extractArxivIDFromString(d.Extra) == query.ArXiv {
			return true
		}
		if extraContainsExactToken(d.Extra, "arXiv: ", query.ArXiv) ||
			extraContainsExactToken(d.Extra, "arXiv:", query.ArXiv) {
			return true
		}
	}
	if query.PMID != "" &&
		(extraContainsExactToken(d.Extra, "PMID: ", query.PMID) ||
			extraContainsExactToken(d.Extra, "PMID:", query.PMID)) {
		return true
	}
	if query.Citekey != "" {
		if strings.TrimSpace(d.CitationKey) == query.Citekey {
			return true
		}
		if extraContainsExactToken(d.Extra, "Citation Key: ", query.Citekey) ||
			extraContainsExactToken(d.Extra, "Citation Key:", query.Citekey) {
			return true
		}
	}
	if query.URL != "" && normalizeFindURL(d.URL) == query.URL {
		return true
	}
	if query.OpenAlex != "" {
		for _, value := range []string{d.URL, d.ArchiveID, d.ArchiveLocation, d.Repository} {
			if normalizeOpenAlexWorkID(value) == query.OpenAlex {
				return true
			}
		}
		if extraContainsExactTokenFold(d.Extra, "OpenAlex: ", query.OpenAlex) ||
			extraContainsExactTokenFold(d.Extra, "OpenAlex:", query.OpenAlex) ||
			extraContainsExactTokenFold(d.Extra, "OpenAlex ID: ", query.OpenAlex) ||
			extraContainsExactTokenFold(d.Extra, "OpenAlex ID:", query.OpenAlex) {
			return true
		}
	}
	if query.Title != "" && normalizeDuplicateTitle(d.Title) == normalizeDuplicateTitle(query.Title) {
		return true
	}
	return false
}

func extraContainsExactTokenFold(extra, prefix, token string) bool {
	return extraContainsExactToken(strings.ToLower(extra), strings.ToLower(prefix), strings.ToLower(token))
}

func extraContainsExactToken(extra, prefix, token string) bool {
	if token == "" {
		return false
	}
	// Extra is line-oriented. Require both the label and token to end at a
	// whitespace boundary so labels embedded in words and token prefixes do not
	// match.
	needle := prefix + token
	searchFrom := 0
	for {
		idx := strings.Index(extra[searchFrom:], needle)
		if idx < 0 {
			return false
		}
		pos := searchFrom + idx
		beforeOK := pos == 0 || isFindTokenBoundary(extra[pos-1])
		after := pos + len(needle)
		afterOK := after >= len(extra) || isFindTokenBoundary(extra[after])
		if beforeOK && afterOK {
			return true
		}
		searchFrom = pos + 1
	}
}

func isFindTokenBoundary(ch byte) bool {
	switch ch {
	case '\n', '\r', ' ', '\t':
		return true
	default:
		return false
	}
}
